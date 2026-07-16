package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/lore-gpt/lore/core/provision"
	"github.com/lore-gpt/lore/core/store"
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
			// File-guard: existing credentials mean this project was already provisioned. Leave it — and the
			// key it holds — untouched, so the compose one-shot is safe to re-run on every `up`.
			if out != "" {
				if info, statErr := os.Stat(out); statErr == nil && info.Size() > 0 {
					_, err := fmt.Fprintf(cmd.ErrOrStderr(),
						"already provisioned; credentials at %s (delete it to provision a fresh project)\n", out)
					return err
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
				return err
			}
			defer st.Close()

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
