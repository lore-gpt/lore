// Command lore is the OSS server binary. It wires the open-core packages with
// their default (OSS) extension implementations and exposes the server, worker,
// and operator subcommands (serve, worker, migrate, provision, keys, models,
// version, health, and the quickstart helpers).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/lore-gpt/lore/core"
	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/queue"
	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/workmem"
	"github.com/lore-gpt/lore/server/internal/config"
	"github.com/lore-gpt/lore/server/internal/embedding"
	"github.com/lore-gpt/lore/server/internal/extraction"
	"github.com/lore-gpt/lore/server/internal/telemetry"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "lore",
		Short: "Lore — the coordination memory layer for multi-agent AI systems",
		// Setting Version makes cobra add a `--version` flag alongside the `version` subcommand.
		Version: core.Version,
		// Errors are returned by RunE and printed by cobra; don't also dump usage.
		SilenceUsage: true,
	}
	root.AddCommand(serveCmd(), workerCmd(), migrateCmd(), versionCmd(), healthCmd(), keysCmd(), modelsCmd(),
		provisionCmd(), packCmd(), doctorCmd(), initCmd())
	return root
}

// signalContext returns a context canceled on SIGINT/SIGTERM so the server and
// worker shut down gracefully.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// buildEmbedder selects the embedding provider from configuration. Both serve and
// worker call it so the read path (query embedding) and the write path (stored
// memories) build the same embedder and share a model space; a misconfiguration
// fails the process at boot, before it does any work.
func buildEmbedder(ctx context.Context, cfg config.Config) (ext.Embedder, error) {
	return embedding.Build(ctx, embedding.Config{
		Provider:       cfg.EmbeddingProvider,
		BaseURL:        cfg.EmbeddingBaseURL,
		APIKey:         cfg.EmbeddingAPIKey,
		Model:          cfg.EmbeddingModel,
		Dim:            cfg.EmbeddingDim,
		SendDimensions: cfg.EmbeddingSendDimensions,
	})
}

// serveWorkerMetrics exposes /metrics on addr for the worker, which has no API
// server of its own. It runs until ctx is done, then shuts down with a short
// grace. A nil handler (metrics disabled) is a no-op. A bind failure is logged,
// not fatal: telemetry is optional and must not take the worker down.
func serveWorkerMetrics(ctx context.Context, addr string, handler http.Handler) {
	if handler == nil {
		return
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", handler)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.ErrorContext(ctx, "worker metrics listener failed", slog.String("addr", addr), slog.Any("error", err))
		}
	}()
	slog.InfoContext(ctx, "worker metrics listener", slog.String("addr", addr))
}

func serveCmd() *cobra.Command {
	var autoMigrate bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the HTTP API server",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			ctx, stop := signalContext()
			defer stop()

			// The distroless image has no shell, so migrations cannot be chained
			// before serve with `&&`; this flag runs them in-process instead.
			if autoMigrate {
				if err := runMigrations(ctx, cfg.DatabaseURL); err != nil {
					return err
				}
			}

			// Open the working-memory stripe from config. An unset URL disables it (hot facts fall through
			// to durable extraction); a malformed URL is fatal; an unreachable server degrades but does not
			// fail the boot. The server takes ownership and closes it.
			wm, err := workmem.Open(ctx, cfg.ValkeyURL)
			if err != nil {
				return err
			}
			slog.InfoContext(ctx, "working memory", slog.String("mode", wm.Mode().String()))

			// The read path embeds the query, so serve builds the same embedder the
			// worker does; a mismatch would put the query and the stored vectors in
			// different model spaces. A misconfigured provider fails here, at boot.
			embedder, err := buildEmbedder(ctx, cfg)
			if err != nil {
				wm.Close()
				return err
			}

			// Build the metrics registry + /metrics handler; the server exposes the
			// handler on its main port, unauthenticated beside /healthz.
			meter, metricsHandler := telemetry.Build(telemetry.Config{
				MetricsEnabled: cfg.MetricsEnabled, Version: core.Version, Role: "server",
			})

			srv, err := core.NewServer(ctx, core.Config{
				Addr:                 cfg.Addr,
				DatabaseURL:          cfg.DatabaseURL,
				WorkmemMaxValueBytes: cfg.WorkmemMaxValueBytes,
			}, core.WithWorkmem(wm), core.WithEmbedder(embedder),
				core.WithMeterRegistry(meter), core.WithMetricsHandler(metricsHandler))
			if err != nil {
				wm.Close()
				return err
			}
			defer srv.Close() // closes the store and the working-memory stripe

			slog.InfoContext(ctx, "starting server",
				slog.String("addr", cfg.Addr), slog.String("version", core.Version))
			return srv.Start(ctx)
		},
	}
	cmd.Flags().BoolVar(&autoMigrate, "auto-migrate", false,
		"apply database migrations before serving (for shell-less images)")
	return cmd
}

func workerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "worker",
		Short: "Run the background job worker",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			ctx, stop := signalContext()
			defer stop()

			// Choose the extraction provider from configuration and inject it into
			// the worker. A misconfiguration (e.g. anthropic without a key) fails
			// here, before the worker starts consuming jobs.
			extractor, err := extraction.Build(ctx, cfg.ExtractionProvider, cfg.AnthropicAPIKey, cfg.ExtractionModel)
			if err != nil {
				return err
			}

			// The write path embeds stored memories; serve embeds the query. Both
			// build the embedder the same way so the vectors share a model space.
			embedder, err := buildEmbedder(ctx, cfg)
			if err != nil {
				return err
			}

			// Open the working-memory stripe (same config as serve): the worker routes kind:"state" events
			// to it when healthy, and to a durable claim otherwise. Unset disables it; a malformed URL is
			// fatal; unreachable degrades. The worker takes ownership and closes it.
			wm, err := workmem.Open(ctx, cfg.ValkeyURL)
			if err != nil {
				return err
			}
			slog.InfoContext(ctx, "working memory", slog.String("mode", wm.Mode().String()))

			// The worker has no API server, so it exposes /metrics on its own listener
			// (LORE_METRICS_ADDR). Build the registry, inject it for the job
			// instrumentation, and serve the handler.
			meter, metricsHandler := telemetry.Build(telemetry.Config{
				MetricsEnabled: cfg.MetricsEnabled, Version: core.Version, Role: "worker",
			})
			serveWorkerMetrics(ctx, cfg.MetricsAddr, metricsHandler)

			w, err := core.NewWorker(ctx, core.Config{
				DatabaseURL: cfg.DatabaseURL,
			}, core.WithExtractor(extractor), core.WithWorkmem(wm), core.WithEmbedder(embedder),
				core.WithMeterRegistry(meter))
			if err != nil {
				wm.Close()
				return err
			}
			defer w.Close() // closes the store and the working-memory stripe

			slog.InfoContext(ctx, "starting worker", slog.String("version", core.Version))
			return w.Start(ctx)
		},
	}
}

func migrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Apply database migrations and exit",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			ctx, stop := signalContext()
			defer stop()

			if err := runMigrations(ctx, cfg.DatabaseURL); err != nil {
				return err
			}
			slog.InfoContext(ctx, "migrations applied")
			return nil
		},
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version and exit",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), core.Version)
			return err
		},
	}
}

// healthCmd probes the local /healthz endpoint and exits non-zero if the server
// is not healthy. It backs the container HEALTHCHECK: the distroless image has
// no shell or curl, so the binary probes itself. It reads only LORE_ADDR (for
// the port) so it works without the full server configuration.
func healthCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "Probe the local /healthz endpoint; exit non-zero if unhealthy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, port, err := net.SplitHostPort(os.Getenv("LORE_ADDR"))
			if err != nil || port == "" {
				port = "8080"
			}
			url := "http://127.0.0.1:" + port + "/healthz"

			ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return err
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("unhealthy: /healthz returned %d", resp.StatusCode)
			}
			return nil
		},
	}
}

// runMigrations applies the goose application schema and then the River queue
// schema. It is the single migration entry point for both `lore migrate` and
// `serve --auto-migrate`.
func runMigrations(ctx context.Context, dsn string) error {
	if err := store.RunMigrations(ctx, dsn); err != nil {
		return fmt.Errorf("apply app migrations: %w", err)
	}

	st, err := store.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open store for queue migration: %w", err)
	}
	defer st.Close()

	if err := queue.Migrate(ctx, st.Pool); err != nil {
		return fmt.Errorf("apply queue migrations: %w", err)
	}
	return nil
}
