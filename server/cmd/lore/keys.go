package main

import (
	"errors"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/spf13/cobra"

	"github.com/lore-gpt/lore/core/apikey"
	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/server/internal/config"
)

// keysCmd groups API-key administration: minting and revoking the bearer keys the HTTP API authenticates. The
// OSS build has no self-serve key endpoint; an operator runs these against the same database the server reads.
func keysCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "Manage API keys",
	}
	cmd.AddCommand(keysCreateCmd(), keysListCmd(), keysRevokeCmd())
	return cmd
}

// keysCreateCmd mints a key for a project and prints the raw token ONCE (stdout); the store keeps only its hash
// and a non-secret prefix. The key id (for a later revoke) and a reminder go to stderr, so a script can capture
// just the token from stdout.
func keysCreateCmd() *cobra.Command {
	var project, name string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Mint an API key for a project and print it once",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectID, err := uuid.Parse(project)
			if err != nil {
				return fmt.Errorf("--project must be a UUID: %w", err)
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			ctx, stop := signalContext()
			defer stop()

			st, err := store.New(ctx, cfg.DatabaseURL)
			if err != nil {
				return err
			}
			defer st.Close()

			token, hash, prefix, err := apikey.New()
			if err != nil {
				return fmt.Errorf("generate key: %w", err)
			}
			var namePtr *string
			if name != "" {
				namePtr = &name
			}
			row, err := db.New(st.Pool).CreateAPIKey(ctx, db.CreateAPIKeyParams{
				ProjectID: pgtype.UUID{Bytes: projectID, Valid: true},
				Name:      namePtr,
				KeyPrefix: &prefix,
				KeyHash:   hash,
			})
			if err != nil {
				return fmt.Errorf("create key: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"created api key %s for project %s — store the token now, it is not recoverable\n",
				uuid.UUID(row.ID.Bytes), project)
			_, err = fmt.Fprintln(cmd.OutOrStdout(), token)
			return err
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project UUID the key authorises (required)")
	cmd.Flags().StringVar(&name, "name", "", "optional label for the key")
	_ = cmd.MarkFlagRequired("project")
	return cmd
}

// keysListCmd shows every key minted for a project. Without it the id printed once at creation was the only
// handle an operator ever had on a key: lose the scrollback and `revoke` becomes unusable, because there was
// no way to enumerate what exists. It prints the non-secret prefix — the reason that column exists — so a row
// can be matched against a key someone is holding, and it includes revoked keys, since "was this already
// revoked, and when" is usually the question being asked.
func keysListCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List a project's API keys (never the keys themselves)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectID, err := uuid.Parse(project)
			if err != nil {
				return fmt.Errorf("--project must be a UUID: %w", err)
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			ctx, stop := signalContext()
			defer stop()

			st, err := store.New(ctx, cfg.DatabaseURL)
			if err != nil {
				return err
			}
			defer st.Close()

			rows, err := db.New(st.Pool).ListProjectAPIKeys(ctx, pgtype.UUID{Bytes: projectID, Valid: true})
			if err != nil {
				return fmt.Errorf("list keys: %w", err)
			}
			out := cmd.OutOrStdout()
			if len(rows) == 0 {
				// Not an error: a project with no keys is a legitimate state, and exiting non-zero here would
				// make `list` unusable as a scripted precondition check.
				_, err := fmt.Fprintf(cmd.ErrOrStderr(), "no api keys for project %s\n", project)
				return err
			}
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "ID\tNAME\tPREFIX\tCREATED\tSTATUS")
			for _, r := range rows {
				status := "active"
				if r.RevokedAt.Valid {
					status = "revoked " + r.RevokedAt.Time.UTC().Format(time.RFC3339)
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					uuid.UUID(r.ID.Bytes), orDash(r.Name), orDash(r.KeyPrefix),
					r.CreatedAt.Time.UTC().Format(time.RFC3339), status)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project id (UUID)")
	_ = cmd.MarkFlagRequired("project")
	return cmd
}

// orDash renders a nullable text column for the listing. Both `name` and `key_prefix` are nullable — a key
// minted without a name has no name, and rows predating the prefix column have no prefix — and an empty cell
// in a tab-aligned table reads as a rendering bug rather than as absent data.
func orDash(s *string) string {
	if s == nil || *s == "" {
		return "-"
	}
	return *s
}

// keysRevokeCmd revokes a key by the id printed at creation, or shown by `keys list`.
//
// The exit code answers "is this key revoked now?", not "did this call change something". Re-running a revoke
// is a normal thing for a script to do — a retried deploy step, a cleanup that runs twice — and failing it
// would mean the caller's desired state was already true and it got an error anyway. So an already-revoked key
// exits 0 and says nothing changed, while an unknown id still exits non-zero, because that one means the
// operator is holding an id that names nothing and the run should stop.
//
// Telling those apart needs a second query: the UPDATE reports zero rows affected for both cases, and no
// rewriting of it can distinguish them, since "no row matched the predicate" is the same result whether the
// row is missing or merely already revoked. The follow-up read only runs on that failed path.
func keysRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <key-id>",
		Short: "Revoke an API key by its id",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			keyID, err := uuid.Parse(args[0])
			if err != nil {
				return fmt.Errorf("key id must be a UUID: %w", err)
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			ctx, stop := signalContext()
			defer stop()

			st, err := store.New(ctx, cfg.DatabaseURL)
			if err != nil {
				return err
			}
			defer st.Close()

			q := db.New(st.Pool)
			id := pgtype.UUID{Bytes: keyID, Valid: true}
			n, err := q.RevokeAPIKey(ctx, id)
			if err != nil {
				return fmt.Errorf("revoke key: %w", err)
			}
			if n == 0 {
				revokedAt, err := q.GetAPIKeyRevokedAt(ctx, id)
				switch {
				case errors.Is(err, pgx.ErrNoRows):
					return fmt.Errorf("no api key with id %s (check `lore keys list --project <id>`)", args[0])
				case err != nil:
					return fmt.Errorf("look up key %s: %w", args[0], err)
				}
				// A concurrent revoke that landed between the two statements reports the same thing, which is
				// the true state either way: the key is revoked and this call did not do it.
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "api key %s was already revoked at %s; no change\n",
					args[0], revokedAt.Time.UTC().Format(time.RFC3339))
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "revoked api key %s\n", args[0])
			return err
		},
	}
}
