package main

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/spf13/cobra"

	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/server/internal/config"
	"github.com/lore-gpt/lore/server/internal/embedding"
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

			// Describe the embedder this deployment's configuration selects — the same call, from the same
			// configuration, that `lore doctor` and the server's boot both resolve. Composing a fixed
			// embedder here instead would make the command answer for a deployment other than the one it is
			// pointed at: it would report MATCH against the OSS default while the running server rejected
			// every recall with a 409, which is precisely the failure this command exists to explain.
			//
			// The remaining limit is honest and narrow: a build that injects an embedder programmatically
			// rather than through LORE_EMBEDDING_* is invisible to a separate CLI process. /healthz reports
			// what the server actually composed and stays the final authority.
			embedderModel, embedderDim, _, err := embedding.Describe(embedding.Config{
				Provider:       cfg.EmbeddingProvider,
				BaseURL:        cfg.EmbeddingBaseURL,
				Model:          cfg.EmbeddingModel,
				Dim:            cfg.EmbeddingDim,
				SendDimensions: cfg.EmbeddingSendDimensions,
			})
			if err != nil {
				// The same misconfiguration would refuse to start the server; say so here rather than
				// falling back to a default and reporting a comparison against an embedder nothing runs.
				return err
			}

			st, err := store.New(ctx, cfg.DatabaseURL)
			if err != nil {
				return err
			}
			defer st.Close()

			active, err := db.New(st.Pool).GetActiveModelID(ctx, pgtype.UUID{Bytes: projectID, Valid: true})
			if err != nil {
				return fmt.Errorf("read active model: %w", err)
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "project:        %s\n", args[0])
			_, _ = fmt.Fprintf(out, "embedder model: %s\n", embedderModel)
			_, _ = fmt.Fprintf(out, "embedder dim:   %d\n", embedderDim)
			switch {
			case active == nil:
				_, _ = fmt.Fprintln(out, "active model:   (none — pinned on the first consolidated memory)")
				_, _ = fmt.Fprintln(out, "status:         UNSET")
			case *active == embedderModel:
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
