// Command lore is the OSS server binary. It wires the open-core packages with
// their default (OSS) extension implementations and exposes the serve, worker,
// migrate, version, and health subcommands.
package main

import (
	"context"
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
	"github.com/lore-gpt/lore/core/queue"
	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/workmem"
	"github.com/lore-gpt/lore/server/internal/config"
	"github.com/lore-gpt/lore/server/internal/extraction"
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
		// Errors are returned by RunE and printed by cobra; don't also dump usage.
		SilenceUsage: true,
	}
	root.AddCommand(serveCmd(), workerCmd(), migrateCmd(), versionCmd(), healthCmd(), keysCmd(), modelsCmd())
	return root
}

// signalContext returns a context canceled on SIGINT/SIGTERM so the server and
// worker shut down gracefully.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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

			srv, err := core.NewServer(ctx, core.Config{
				Addr:                 cfg.Addr,
				DatabaseURL:          cfg.DatabaseURL,
				WorkmemMaxValueBytes: cfg.WorkmemMaxValueBytes,
			}, core.WithWorkmem(wm))
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

			// Open the working-memory stripe (same config as serve): the worker routes kind:"state" events
			// to it when healthy, and to a durable claim otherwise. Unset disables it; a malformed URL is
			// fatal; unreachable degrades. The worker takes ownership and closes it.
			wm, err := workmem.Open(ctx, cfg.ValkeyURL)
			if err != nil {
				return err
			}
			slog.InfoContext(ctx, "working memory", slog.String("mode", wm.Mode().String()))

			w, err := core.NewWorker(ctx, core.Config{
				DatabaseURL: cfg.DatabaseURL,
			}, core.WithExtractor(extractor), core.WithWorkmem(wm))
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
