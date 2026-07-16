package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"
)

// doctorCmd diagnoses a Lore install for the quickstart: can it reach the database, is the schema migrated,
// and is the server healthy. It stays deliberately thin — connectivity, schema, and health, not a full audit.
// It connects with a plain pool (no pgvector type registration) so it can still report clearly on a database
// where migrations have not run yet. It exits non-zero if any check fails, so a script can gate on it.
func doctorCmd() *cobra.Command {
	var url string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose the install: database connectivity, schema, and server health",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signalContext()
			defer stop()
			out := cmd.OutOrStdout()

			var failed bool
			check := func(name string, err error) {
				if err != nil {
					failed = true
					_, _ = fmt.Fprintf(out, "x %s: %v\n", name, err)
					return
				}
				_, _ = fmt.Fprintf(out, "ok %s\n", name)
			}

			dsn := strings.TrimSpace(os.Getenv("LORE_DATABASE_URL"))
			if dsn == "" {
				check("database url (LORE_DATABASE_URL)", errors.New("not set"))
			} else if pool, err := pgxpool.New(ctx, dsn); err != nil {
				check("database connection", err)
			} else {
				defer pool.Close()
				check("database connection", pool.Ping(ctx))
				check("extension: vector (pgvector)", checkExtension(ctx, pool, "vector"))
				check("extension: pg_search", checkExtension(ctx, pool, "pg_search"))
				check("schema: application tables migrated", checkRelation(ctx, pool, "api_keys"))
				check("schema: job queue migrated", checkRelation(ctx, pool, "river_job"))
			}

			check("server /healthz", checkHealthz(ctx, url))

			if failed {
				return errors.New("one or more checks failed")
			}
			_, _ = fmt.Fprintln(out, "\nall checks passed")
			return nil
		},
	}
	cmd.Flags().StringVar(&url, "url", "http://localhost:8080", "base URL of the Lore server to probe")
	return cmd
}

// checkExtension reports whether a Postgres extension is installed.
func checkExtension(ctx context.Context, pool *pgxpool.Pool, name string) error {
	var present bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname = $1)`, name).Scan(&present); err != nil {
		return err
	}
	if !present {
		return errors.New("not installed")
	}
	return nil
}

// checkRelation reports whether a relation exists, standing in for "the migrations that create it have run".
func checkRelation(ctx context.Context, pool *pgxpool.Pool, name string) error {
	var reg *string
	if err := pool.QueryRow(ctx, `SELECT to_regclass($1)::text`, name).Scan(&reg); err != nil {
		return err
	}
	if reg == nil {
		return errors.New("missing — run `lore migrate` (or serve --auto-migrate)")
	}
	return nil
}

// checkHealthz probes the server's /healthz and fails on a non-200 (which the endpoint returns when a
// dependency is down).
func checkHealthz(ctx context.Context, url string) error {
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("returned %s", resp.Status)
	}
	return nil
}
