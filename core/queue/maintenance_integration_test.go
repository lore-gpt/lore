//go:build integration

package queue_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lore-gpt/lore/core/jobs"
	"github.com/lore-gpt/lore/core/queue"
	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
)

// seedRunOfSize creates a project and a run holding n events, and returns both ids. The run's checkpoint
// stays at 0, so every event is undistilled — which is what a run looks like before its first pass, and what
// it still looks like after a pass has given up. (The sibling helper in this package seeds exactly one event
// and returns strings; these tests need a size they choose and the ids in their database form.)
func seedRunOfSize(ctx context.Context, t *testing.T, st *store.Store, n int) (project, run pgtype.UUID) {
	t.Helper()
	q := db.New(st.Pool)
	org, err := q.InsertOrganization(ctx, "acme")
	if err != nil {
		t.Fatalf("insert org: %v", err)
	}
	proj, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "a"})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	r, err := q.InsertRun(ctx, proj.ID)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := q.InsertEvent(ctx, db.InsertEventParams{
			AgentID: "agent", Payload: []byte(`{"content":"hello"}`), RunID: r.ID,
		}); err != nil {
			t.Fatalf("insert event %d: %v", i, err)
		}
	}
	return proj.ID, r.ID
}

func findStalled(t *testing.T, stalled []queue.StalledRun, runID pgtype.UUID) *queue.StalledRun {
	t.Helper()
	want := uuid.UUID(runID.Bytes).String()
	for i := range stalled {
		if stalled[i].RunID == want {
			return &stalled[i]
		}
	}
	return nil
}

// TestStalledRunsFindsARunWithNoPassLeft proves the definition the command depends on: a run counts as
// stalled when it has events past its checkpoint AND no extraction job in a state that would work them. The
// same run with a queued pass must NOT be listed — otherwise every healthy run in flight would be reported as
// broken, and the operator would learn to ignore the list.
func TestStalledRunsFindsARunWithNoPassLeft(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	project, run := seedRunOfSize(ctx, t, st, 3)

	q, err := queue.New(st.Pool, nil)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}

	stalled, err := q.StalledRuns(ctx)
	if err != nil {
		t.Fatalf("StalledRuns: %v", err)
	}
	got := findStalled(t, stalled, run)
	if got == nil {
		t.Fatalf("run %s not listed; a run with undistilled events and no job is exactly what this finds",
			uuid.UUID(run.Bytes))
	}
	if got.Behind != 3 {
		t.Errorf("Behind = %d, want 3 (every event is past the checkpoint)", got.Behind)
	}
	if got.ProjectID != uuid.UUID(project.Bytes).String() {
		t.Errorf("ProjectID = %s, want %s", got.ProjectID, uuid.UUID(project.Bytes))
	}
	if got.OldestUncovered.IsZero() {
		t.Error("OldestUncovered is zero; the operator reads how long the run has been parked from it")
	}

	// Queue a pass for it: the run is still behind, but something will move it, so it is no longer stalled.
	queued, err := q.RequeueExtract(ctx, uuid.UUID(project.Bytes).String(), uuid.UUID(run.Bytes).String())
	if err != nil {
		t.Fatalf("RequeueExtract: %v", err)
	}
	if !queued {
		t.Fatal("RequeueExtract = false on a run with no job, want true")
	}
	stalled, err = q.StalledRuns(ctx)
	if err != nil {
		t.Fatalf("StalledRuns after requeue: %v", err)
	}
	if got := findStalled(t, stalled, run); got != nil {
		t.Error("a run with a queued pass is still listed as stalled; being behind is not the same as being parked")
	}
}

// TestStalledRunsIgnoresACaughtUpRun pins the other half of the predicate. A run whose checkpoint has reached
// its last event has nothing to distil, and must not be listed just because no job exists for it — which is
// the normal resting state of every finished run in the database.
func TestStalledRunsIgnoresACaughtUpRun(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	_, run := seedRunOfSize(ctx, t, st, 2)

	if _, err := st.Pool.Exec(ctx, `UPDATE runs SET covered_seq = last_seq WHERE id = $1`, run); err != nil {
		t.Fatalf("advance checkpoint: %v", err)
	}

	q, err := queue.New(st.Pool, nil)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	stalled, err := q.StalledRuns(ctx)
	if err != nil {
		t.Fatalf("StalledRuns: %v", err)
	}
	if got := findStalled(t, stalled, run); got != nil {
		t.Errorf("caught-up run listed as stalled (behind %d)", got.Behind)
	}
}

// TestStalledRunsCountsOnlyWhatIsPastTheCheckpoint proves Behind is measured from the checkpoint rather than
// from zero: a run that distilled part of its history and then stopped is behind by the remainder, and an
// operator sizing the damage reads that number.
func TestStalledRunsCountsOnlyWhatIsPastTheCheckpoint(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	_, run := seedRunOfSize(ctx, t, st, 5)

	if _, err := st.Pool.Exec(ctx, `UPDATE runs SET covered_seq = 2 WHERE id = $1`, run); err != nil {
		t.Fatalf("advance checkpoint: %v", err)
	}

	q, err := queue.New(st.Pool, nil)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	stalled, err := q.StalledRuns(ctx)
	if err != nil {
		t.Fatalf("StalledRuns: %v", err)
	}
	got := findStalled(t, stalled, run)
	if got == nil {
		t.Fatal("partially distilled run not listed")
	}
	if got.Behind != 3 {
		t.Errorf("Behind = %d, want 3 (5 events, checkpoint at 2)", got.Behind)
	}

	// The reported instant is the OLDEST uncovered event, not the newest and not the run's first: that is the
	// age of the backlog the run is carrying, which is what the operator reads as "stalled for".
	var wantAt time.Time
	if err := st.Pool.QueryRow(ctx,
		`SELECT created_at FROM events WHERE run_id = $1 AND seq = 3`, run).Scan(&wantAt); err != nil {
		t.Fatalf("read the first uncovered event: %v", err)
	}
	if !got.OldestUncovered.Equal(wantAt) {
		t.Errorf("OldestUncovered = %s, want %s (the first event past the checkpoint, seq 3)",
			got.OldestUncovered, wantAt)
	}
}

// TestRequeueExtractIsIdempotent proves the safety the command leans on: asking for a pass that is already
// queued does not stack a second one. Without it an operator working through a list of stalled runs could
// double-queue the ones that had already recovered, and the report says so rather than lying about success.
func TestRequeueExtractIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	project, run := seedRunOfSize(ctx, t, st, 1)
	projectStr, runStr := uuid.UUID(project.Bytes).String(), uuid.UUID(run.Bytes).String()

	q, err := queue.New(st.Pool, nil)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	first, err := q.RequeueExtract(ctx, projectStr, runStr)
	if err != nil {
		t.Fatalf("first RequeueExtract: %v", err)
	}
	second, err := q.RequeueExtract(ctx, projectStr, runStr)
	if err != nil {
		t.Fatalf("second RequeueExtract: %v", err)
	}
	if !first || second {
		t.Errorf("queued = (%v, %v), want (true, false): the second ask must report that one was already there",
			first, second)
	}

	var n int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = $1 AND args->>'run_id' = $2`,
		jobs.ExtractRunArgs{}.Kind(), runStr).Scan(&n); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if n != 1 {
		t.Errorf("job rows = %d, want 1 (a duplicate ask must not create a second pass)", n)
	}
}

// TestStalledRunsSurvivesUndatableBacklog covers the degrade path: a run that is behind but whose uncovered
// events are no longer in the table cannot say how long it has been parked. It must still be listed — it is
// still a run with work nothing will do — and it must not take the rest of the listing down with it, which a
// scan straight into time.Time would. Nothing deletes events today; the retention columns the schema already
// carries are what makes this reachable later.
func TestStalledRunsSurvivesUndatableBacklog(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	_, purged := seedRunOfSize(ctx, t, st, 2)
	_, intact := seedRunOfSize(ctx, t, st, 2)

	if _, err := st.Pool.Exec(ctx, `DELETE FROM events WHERE run_id = $1`, purged); err != nil {
		t.Fatalf("purge events: %v", err)
	}

	q, err := queue.New(st.Pool, nil)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	stalled, err := q.StalledRuns(ctx)
	if err != nil {
		t.Fatalf("StalledRuns: %v", err)
	}
	got := findStalled(t, stalled, purged)
	if got == nil {
		t.Fatal("a run behind with no datable events dropped off the list; it still has work nothing will do")
	}
	if !got.OldestUncovered.IsZero() {
		t.Errorf("OldestUncovered = %s, want the zero time when there is nothing left to date it", got.OldestUncovered)
	}
	if got.Behind != 2 {
		t.Errorf("Behind = %d, want 2 (the checkpoint still says so)", got.Behind)
	}
	if findStalled(t, stalled, intact) == nil {
		t.Error("the undatable run took a healthy row out of the listing with it")
	}
}
