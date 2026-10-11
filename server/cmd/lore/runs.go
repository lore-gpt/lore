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

	"github.com/lore-gpt/lore/core/queue"
	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/server/internal/config"
)

// runsCmd groups the maintenance an operator does to a run's distillation. Extraction is a background pass
// that an event insert enqueues, so in the ordinary case there is nothing to operate: events arrive, a pass
// runs, the checkpoint advances. What these two commands exist for is the case where a pass fails the same
// way on every attempt and gives up — after that the checkpoint stays put and no new pass is enqueued unless
// another event arrives, which for a finished conversation never happens. `stalled` finds those runs and
// `requeue` gives one another pass. Both run against the same database the server reads.
func runsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runs",
		Short: "Inspect and repair run distillation",
	}
	cmd.AddCommand(runsStalledCmd(), runsRequeueCmd())
	return cmd
}

// runsStalledCmd lists the runs whose distillation has stopped: undistilled events, and no pass left to work
// them. It reports rather than repairs, because the right repair is not always another pass — a run that gave
// up because its content did not fit the output ceiling will give up again until the ceiling is raised, and
// the failure that discarded the job says which setting it means. Requeueing before that would spend the
// model's time reproducing the same refusal.
func runsStalledCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stalled",
		Short: "List runs whose distillation has stopped",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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

			q, err := queue.New(st.Pool, nil)
			if err != nil {
				return err
			}
			stalled, err := q.StalledRuns(ctx)
			if err != nil {
				return err
			}
			if len(stalled) == 0 {
				_, err := fmt.Fprintln(cmd.ErrOrStderr(), "no stalled runs")
				return err
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "RUN\tPROJECT\tEVENTS BEHIND\tSTALLED FOR")
			for _, r := range stalled {
				// A run whose uncovered events are no longer there to date cannot say how long it has been
				// parked. It is still behind, so it is still listed, with the one column it cannot fill
				// named rather than guessed — a zero instant printed as an age reads as two thousand years.
				stalledFor := "unknown"
				if !r.OldestUncovered.IsZero() {
					stalledFor = time.Since(r.OldestUncovered).Truncate(time.Second).String()
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", r.RunID, r.ProjectID, r.Behind, stalledFor)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.ErrOrStderr(),
				"\n%d run(s) stopped distilling. Check why the pass gave up before requeueing one — the worker\n"+
					"logs the failure that discarded it, and a window that overran the output ceiling will overrun\n"+
					"it again. Then: lore runs requeue <run>\n", len(stalled))
			return err
		},
	}
}

// runsRequeueCmd enqueues one more extraction pass for a run. The pass starts from the run's checkpoint, so
// it covers exactly the events the stalled one did not; asking for a pass that is already queued or running
// is a no-op rather than a second pass, because the job is unique per run.
func runsRequeueCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "requeue <run>",
		Short: "Queue another extraction pass for a run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID, err := uuid.Parse(args[0])
			if err != nil {
				return fmt.Errorf("run must be a UUID: %w", err)
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

			// The run carries its own tenant, so the operator names a run and not also the project it is in.
			projectID, err := db.New(st.Pool).GetRunProject(ctx, pgtype.UUID{Bytes: runID, Valid: true})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("unknown run %s", runID)
				}
				return fmt.Errorf("look up run: %w", err)
			}

			q, err := queue.New(st.Pool, nil)
			if err != nil {
				return err
			}
			queued, err := q.RequeueExtract(ctx, uuid.UUID(projectID.Bytes).String(), runID.String())
			if err != nil {
				return err
			}
			if !queued {
				_, err := fmt.Fprintf(cmd.ErrOrStderr(),
					"run %s already has an extraction pass queued or running; nothing to do\n", runID)
				return err
			}
			_, err = fmt.Fprintf(cmd.ErrOrStderr(),
				"queued an extraction pass for run %s — a worker has to be running for it to be worked\n", runID)
			return err
		},
	}
}
