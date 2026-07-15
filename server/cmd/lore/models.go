package main

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/spf13/cobra"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/server/internal/config"
)

// modelsCmd groups embedding-model inspection. A project's active embedding model is pinned automatically to
// the running embedder on its first consolidated memory (there is no set command — one embedder is composed
// per deployment), so this is a read-only diagnostic: what a project adopted, and whether it still matches
// the running embedder.
func modelsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "models",
		Short: "Inspect embedding-model selection",
	}
	cmd.AddCommand(modelsShowCmd())
	return cmd
}

// modelsShowCmd reports a project's active embedding model next to the running embedder's model and
// dimension, and whether they match. A MISMATCH is the one-glance explanation for stalled extraction (the
// write path refuses to store vectors in a second model's space) and a 409 on recall — until the deployment's
// embedder is restored or the model is deliberately migrated.
func modelsShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <project>",
		Short: "Show a project's active embedding model and whether it matches the running embedder",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, err := uuid.Parse(args[0])
			if err != nil {
				return fmt.Errorf("project must be a UUID: %w", err)
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

			active, err := db.New(st.Pool).GetActiveModelID(ctx, pgtype.UUID{Bytes: projectID, Valid: true})
			if err != nil {
				return fmt.Errorf("read active model: %w", err)
			}

			// The running embedder is the source of truth a project pins to. This composes the OSS default
			// (fixture) embedder directly — accurate for the OSS binary, which passes no embedder override to
			// the server or worker. A downstream build that swaps the embedder must compose the same one here
			// for this diagnostic to stay truthful (there is no shared config the CLI could read it from yet).
			var embedder ext.Embedder = ext.FixtureEmbedder{}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "project:        %s\n", args[0])
			_, _ = fmt.Fprintf(out, "embedder model: %s\n", embedder.ModelID())
			_, _ = fmt.Fprintf(out, "embedder dim:   %d\n", embedder.Dim())
			switch {
			case active == nil:
				_, _ = fmt.Fprintln(out, "active model:   (none — pinned on the first consolidated memory)")
				_, _ = fmt.Fprintln(out, "status:         UNSET")
			case *active == embedder.ModelID():
				_, _ = fmt.Fprintf(out, "active model:   %s\n", *active)
				_, _ = fmt.Fprintln(out, "status:         MATCH")
			default:
				_, _ = fmt.Fprintf(out, "active model:   %s\n", *active)
				_, _ = fmt.Fprintln(out, "status:         MISMATCH — writes and recall fail until the embedder matches")
			}
			return nil
		},
	}
}
