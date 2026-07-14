package main

import (
	"fmt"

	"github.com/google/uuid"
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
	cmd.AddCommand(keysCreateCmd(), keysRevokeCmd())
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

// keysRevokeCmd revokes a key by the id printed at creation. A key that is unknown or already revoked reports a
// non-zero exit, so revocation is unambiguous.
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

			n, err := db.New(st.Pool).RevokeAPIKey(ctx, pgtype.UUID{Bytes: keyID, Valid: true})
			if err != nil {
				return fmt.Errorf("revoke key: %w", err)
			}
			if n == 0 {
				return fmt.Errorf("no active key with id %s (unknown or already revoked)", args[0])
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "revoked api key %s\n", args[0])
			return err
		},
	}
}
