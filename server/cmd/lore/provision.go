package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/spf13/cobra"

	"github.com/lore-gpt/lore/core/provision"
	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/server/internal/config"
)

// provisionCmd bootstraps a tenant — an organization, a project, its partitions, and one API key — so a first
// project is a single step instead of hand-run seed SQL. Without --out it prints the token once to stdout
// (like `keys create`). With --out it writes the project id and token to a file (0600) and prints nothing
// secret, which is how the compose one-shot provision service surfaces credentials without leaking the token
// into container logs. The --out file doubles as the idempotency guard: if it already exists and is
// non-empty, the command is a no-op, so a second `docker compose up` never mints a second key.
func provisionCmd() *cobra.Command {
	var orgName, projectName, out string
	cmd := &cobra.Command{
		Use:   "provision",
		Short: "Create a project (organization, project, partitions) and mint an API key",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// File-guard: an existing credentials file normally means this project was already provisioned, so
			// the compose one-shot is safe to re-run on every `up`. But a guard on the file alone wrongly
			// no-ops after `docker compose down -v` wipes the database while the host file lingers — every
			// request then fails with a dead key. So when the file names a project, we verify below that the
			// project still exists before trusting it. A file we cannot read a project id from is left
			// untouched (it may be hand-edited); we never provision over something we do not understand.
			var verifyID pgtype.UUID
			verify := false
			if out != "" {
				if info, statErr := os.Stat(out); statErr == nil && info.Size() > 0 {
					id, ok := readProjectID(out)
					if !ok {
						_, err := fmt.Fprintf(cmd.ErrOrStderr(),
							"already provisioned; credentials at %s (could not read a project id; leaving it untouched)\n", out)
						return err
					}
					verifyID, verify = id, true
				}
			}

			cfg, err := config.Load()
			if err != nil {
				return err
			}
			ctx, stop := signalContext()
			defer stop()

			st, err := store.New(ctx, cfg.DatabaseURL)
			if err != nil {
				// The database is unreachable, so we cannot tell a live project from a wiped one. Fail loudly
				// without touching the credentials file — a transient outage must never heal over good keys.
				return err
			}
			defer st.Close()

			if verify {
				exists, err := db.New(st.Pool).ProjectExists(ctx, verifyID)
				if err != nil {
					// A query error is a database problem, not proof the project is gone — same rule as a
					// failed connection: fail loudly and leave the credentials untouched.
					return err
				}
				if exists {
					_, err := fmt.Fprintf(cmd.ErrOrStderr(),
						"already provisioned; credentials at %s (delete it to provision a fresh project)\n", out)
					return err
				}
				// The database answered and the project is gone (most likely a reset). Move the stale
				// credentials aside — never destroy key material — then provision a fresh project below. The
				// rename happens BEFORE the message that announces it, so a rename failure is a loud non-zero
				// exit rather than a false "moved to .bak" line. If the provision that follows fails, only the
				// .bak remains; the next run then provisions cleanly (no credentials file) with the old key
				// still preserved in .bak.
				bak := out + ".bak"
				// Keep a single, latest backup. A prior heal may have left one, and os.Rename does not
				// replace an existing file on every platform, so remove it first for a deterministic overwrite.
				_ = os.Remove(bak)
				if err := os.Rename(out, bak); err != nil {
					return fmt.Errorf("back up stale credentials to %s: %w", bak, err)
				}
				if _, err := fmt.Fprintf(cmd.ErrOrStderr(),
					"previous project not found — was the database reset? Provisioning a fresh project; your old "+
						"credentials were moved to %s (still valid if you pointed at a different database).\n", bak); err != nil {
					return err
				}
			}

			res, err := provision.Provision(ctx, st.Pool, orgName, projectName)
			if err != nil {
				return err
			}

			if out == "" {
				// Interactive: print the token once to stdout; the project id and a reminder go to stderr so a
				// script can capture just the token.
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"provisioned project %s — store the token now, it is not recoverable\n", res.ProjectID)
				_, err = fmt.Fprintln(cmd.OutOrStdout(), res.Token)
				return err
			}

			if err := writeCredentials(out, res); err != nil {
				return err
			}
			// The token is in the file, not here — stdout/logs stay free of the secret.
			_, err = fmt.Fprintf(cmd.OutOrStdout(),
				"provisioned project %s; wrote credentials to %s\n", res.ProjectID, out)
			return err
		},
	}
	cmd.Flags().StringVar(&orgName, "org", "default", "organization name")
	cmd.Flags().StringVar(&projectName, "project", "default", "project name")
	cmd.Flags().StringVar(&out, "out", "",
		"write credentials (project id + token) to this file with 0600 permissions instead of printing the "+
			"token; skips provisioning if the file already exists")
	return cmd
}

// writeCredentials writes the project id and token to path as a dotenv file with owner-only permissions, so a
// user can source it and the token never lands in a log. The parent directory is created if missing, and the
// file is created 0600 up front (not chmod'd after) so the secret is never briefly world-readable.
func writeCredentials(path string, res provision.Result) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create credentials directory %s: %w", dir, err)
		}
	}
	// O_EXCL creates the file atomically and fails if the path already exists — including if it is a
	// symlink — so a race cannot redirect the token to another location and an existing file is never
	// silently overwritten.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create credentials file %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	content := fmt.Sprintf(
		"# Lore project credentials — keep secret. Minted once by `lore provision`; the token is not recoverable.\n"+
			"LORE_PROJECT_ID=%s\n"+
			"LORE_API_KEY=%s\n",
		res.ProjectID, res.Token)
	if _, err := f.WriteString(content); err != nil {
		return fmt.Errorf("write credentials to %s: %w", path, err)
	}
	return nil
}

// readProjectID extracts LORE_PROJECT_ID from a credentials file written by writeCredentials, so the
// file-guard can verify the project against the database. It returns ok=false — meaning "leave the file
// untouched" — if the file cannot be read, has no LORE_PROJECT_ID line, or that line is not a UUID, so a
// hand-edited or unexpected file is never treated as a wiped project and provisioned over.
func readProjectID(path string) (pgtype.UUID, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return pgtype.UUID{}, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "LORE_PROJECT_ID=")
		if !ok {
			continue
		}
		id, err := uuid.Parse(strings.TrimSpace(value))
		if err != nil {
			return pgtype.UUID{}, false
		}
		return pgtype.UUID{Bytes: id, Valid: true}, true
	}
	return pgtype.UUID{}, false
}
