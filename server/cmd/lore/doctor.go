package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/lore-gpt/lore/server/internal/embedding"
)

// doctorComposeInvocation is how to run this command against a compose stack. It is quoted verbatim in the
// README, and a test asserts the two match — the whole point of the hints below is that someone can copy the
// line out of their terminal, so a version that drifted from the documented one would be worse than none.
//
// The flags earn their place: --no-deps because doctor only needs the network, not a second copy of the
// stack, and --url because inside the network the server is a different container, so the default localhost
// is never right there.
const doctorComposeInvocation = "docker compose run --rm --no-deps lore-server doctor --url http://lore-server:8080"

// errServerUnreachable marks a /healthz failure where the probe never reached an HTTP server at all, as
// opposed to reaching one that answered non-200. The distinction decides whether the compose hint is shown:
// a 503 means the server is right there and unhealthy, so telling the operator to look for it elsewhere
// would send them in the wrong direction.
var errServerUnreachable = errors.New("no server answered")

// doctorCmd diagnoses a Lore install for the quickstart: can it reach the database, is the schema migrated,
// and is the server healthy. It stays deliberately thin — connectivity, schema, and health, not a full audit.
// It connects with a plain pool (no pgvector type registration) so it can still report clearly on a database
// where migrations have not run yet. It exits non-zero if any check fails, so a script can gate on it.
//
// A compose install can only satisfy every check from inside the network, and that is not obvious from
// either failure: run it on the host and the database is unreachable because the stack does not publish
// 5432; run it in the stack and the default --url points at the container's own localhost, where nothing
// listens. Both failures therefore carry a hint naming the invocation that works. The hints are phrased
// conditionally ("if this is a compose install") because doctor is equally valid against a bare-metal
// Postgres, where a connection failure means what it says and the hint would be noise.
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
			report := func(name string, err error, hint string) {
				if err != nil {
					failed = true
					_, _ = fmt.Fprintf(out, "x %s: %v\n", name, err)
					if hint != "" {
						_, _ = fmt.Fprintf(out, "  hint: %s\n        %s\n", hint, doctorComposeInvocation)
					}
					return
				}
				_, _ = fmt.Fprintf(out, "ok %s\n", name)
			}
			check := func(name string, err error) { report(name, err, "") }

			// Anything that answers on 5432 of a compose user's host is some OTHER Postgres, which is why
			// this hint covers a failed handshake and not only a refused connection: an unexpected
			// "password authentication failed" is the same misconfiguration wearing a more confusing face.
			const dbHint = "if this is a compose install, the stack does not publish 5432 — reach it from inside the network:"

			dsn := strings.TrimSpace(os.Getenv("LORE_DATABASE_URL"))
			if dsn == "" {
				check("database url (LORE_DATABASE_URL)", errors.New("not set"))
			} else if pool, err := pgxpool.New(ctx, dsn); err != nil {
				report("database connection", err, dbHint)
			} else {
				defer pool.Close()
				report("database connection", pool.Ping(ctx), dbHint)
				check("extension: vector (pgvector)", checkExtension(ctx, pool, "vector"))
				check("extension: pg_search", checkExtension(ctx, pool, "pg_search"))
				check("schema: application tables migrated", checkRelation(ctx, pool, "api_keys"))
				check("schema: job queue migrated", checkRelation(ctx, pool, "river_job"))
			}

			healthzErr := checkHealthz(ctx, url)
			var healthzHint string
			if errors.Is(healthzErr, errServerUnreachable) {
				healthzHint = "if this is a compose install, the server is a different container — localhost is not it:"
			}
			report("server /healthz", healthzErr, healthzHint)

			// Embedding provider: report the configured model identity, and warn
			// (not fail) when it's the offline fixture, so a real install doesn't
			// silently ship deterministic fixture vectors instead of semantic ones.
			dim, _ := strconv.Atoi(strings.TrimSpace(os.Getenv("LORE_EMBEDDING_DIM")))
			modelID, _, isFixture, embErr := embedding.Describe(embedding.Config{
				Provider: strings.TrimSpace(os.Getenv("LORE_EMBEDDING_PROVIDER")),
				BaseURL:  strings.TrimSpace(os.Getenv("LORE_EMBEDDING_BASE_URL")),
				Model:    strings.TrimSpace(os.Getenv("LORE_EMBEDDING_MODEL")),
				Dim:      dim,
			})
			switch {
			case embErr != nil:
				check("embedding provider", embErr)
			case isFixture:
				_, _ = fmt.Fprintf(out, "! embedding provider: offline fixture (%s) — set LORE_EMBEDDING_PROVIDER=openai "+
					"with LORE_EMBEDDING_MODEL and LORE_EMBEDDING_DIM for semantic recall\n", modelID)
			default:
				check(fmt.Sprintf("embedding provider: %s", modelID), nil)
			}

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
// dependency is down). A transport failure is wrapped in errServerUnreachable so the caller can tell "no
// server here" from "the server is here and unwell" — only the first is a sign of looking in the wrong place.
func checkHealthz(ctx context.Context, url string) error {
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", errServerUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("returned %s", resp.Status)
	}
	return nil
}
