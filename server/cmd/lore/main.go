// Command lore is the OSS server binary. It wires the open-core packages with
// their default (OSS) extension implementations and exposes the serve, worker,
// migrate, and version subcommands.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/lore-gpt/lore/core"
	"github.com/lore-gpt/lore/core/queue"
	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/server/internal/config"
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
	root.AddCommand(serveCmd(), workerCmd(), migrateCmd(), versionCmd())
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

			srv, err := core.NewServer(ctx, core.Config{
				Addr:        cfg.Addr,
				DatabaseURL: cfg.DatabaseURL,
				APIKey:      cfg.APIKey,
			})
			if err != nil {
				return err
			}
			defer srv.Close()

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

			w, err := core.NewWorker(ctx, core.Config{
				DatabaseURL: cfg.DatabaseURL,
				APIKey:      cfg.APIKey,
			})
			if err != nil {
				return err
			}
			defer w.Close()

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
