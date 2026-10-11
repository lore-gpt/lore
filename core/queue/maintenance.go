package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lore-gpt/lore/core/jobs"
)

// StalledRun is a run whose distillation has stopped: it has events past its extraction checkpoint, and no
// pass left that would move it.
//
// A run reaches this state when an extraction pass fails the same way on every attempt — a window too dense
// for the output ceiling even at the floor, a deadline a pathological pass keeps overrunning, a provider that
// refuses for as long as the attempts last. Each of those is loud when it happens (the job's final failure is
// logged as an error), and each leaves the same quiet aftermath: the checkpoint stays where it was, the queue
// is empty, and freshness grows for as long as the run lives. Nothing re-enqueues it on its own, because the
// event insert is what enqueues a pass and this run may never see another event.
type StalledRun struct {
	RunID     string
	ProjectID string
	// Behind is how many events sit past the checkpoint, undistilled.
	Behind int64
	// OldestUncovered is when the first of those events arrived — how long the run has been parked, which is
	// also the freshness a reader of this run is living with. It is the zero time when the run is behind but
	// none of the uncovered events are there to date it: unreachable while nothing deletes events, and
	// reachable once the retention the schema already carries is enforced. A run in that state is still worth
	// listing, so this degrades rather than failing the whole read.
	OldestUncovered time.Time
}

// stalledRunsSQL finds runs with undistilled events and no extraction pass in a state that would work them.
//
// The five states are exactly the set that makes the job unique per run: while one is in any of them, a new
// event coalesces into it rather than enqueuing another pass. Their complement is therefore the honest
// definition of stalled — not "behind for a while", which a healthy run in the middle of a slow pass also
// looks like, but "behind with nothing left to move it".
//
// This is deliberately raw rather than a generated query: river_job belongs to River's own migrations, so the
// query layer has no model for it, and the join is the whole point. It reads across tenants on purpose — it
// is an operator's maintenance view over their own deployment, run from the CLI or a worker, never on behalf
// of a request. The per-run cost is a scalar subquery on (run_id, seq), the same index the raw tail rides.
const stalledRunsSQL = `
SELECT r.id::text,
       r.project_id::text,
       r.last_seq - r.covered_seq,
       (SELECT e.created_at FROM events e
         WHERE e.run_id = r.id AND e.seq > r.covered_seq
         ORDER BY e.seq LIMIT 1)
  FROM runs r
 WHERE r.covered_seq < r.last_seq
   AND NOT EXISTS (
         SELECT 1 FROM river_job j
          WHERE j.kind = 'extract_run'
            AND j.state IN ('available', 'pending', 'running', 'scheduled', 'retryable')
            AND j.args->>'run_id' = r.id::text)
 ORDER BY 4`

// StalledRuns lists every run whose distillation has stopped, oldest first.
//
// It is a read: it reports what is parked and leaves the decision to requeue to the operator, because the
// permanence is sometimes deliberate. A pass that gave up at the floor did so after halving its window as far
// as it goes, and requeueing it unchanged will simply reproduce that — the run needs a bigger output ceiling
// first, which is why the failure said so by name. Automatic rescue would turn that into a loop.
func (q *Queue) StalledRuns(ctx context.Context) ([]StalledRun, error) {
	rows, err := q.pool.Query(ctx, stalledRunsSQL)
	if err != nil {
		return nil, fmt.Errorf("list stalled runs: %w", err)
	}
	defer rows.Close()
	var out []StalledRun
	for rows.Next() {
		var r StalledRun
		var oldest pgtype.Timestamptz
		if err := rows.Scan(&r.RunID, &r.ProjectID, &r.Behind, &oldest); err != nil {
			return nil, fmt.Errorf("scan stalled run: %w", err)
		}
		if oldest.Valid {
			r.OldestUncovered = oldest.Time
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list stalled runs: %w", err)
	}
	return out, nil
}

// RequeueExtract enqueues an extraction pass for one run, and reports whether it actually queued one.
//
// Requeueing is safe to ask for at any time: the job is unique per run across the states that can still work
// it, so asking for a pass that is already queued or running skips the insert instead of stacking a second
// one. A false return means exactly that, and is not an error — the run already has what this would have
// given it.
//
// The pass this queues starts from the run's checkpoint, so it picks up precisely the events the stalled one
// never covered; nothing is distilled twice.
func (q *Queue) RequeueExtract(ctx context.Context, projectID, runID string) (bool, error) {
	res, err := q.Client.Insert(ctx, jobs.ExtractRunArgs{ProjectID: projectID, RunID: runID}, nil)
	if err != nil {
		return false, fmt.Errorf("enqueue extract_run: %w", err)
	}
	return !res.UniqueSkippedAsDuplicate, nil
}
