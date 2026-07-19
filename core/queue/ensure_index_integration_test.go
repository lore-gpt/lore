//go:build integration

package queue_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/jobs"
	"github.com/lore-gpt/lore/core/metrics"
	"github.com/lore-gpt/lore/core/queue"
	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/core/workmem"
)

func uuidStr(u pgtype.UUID) string { return uuid.UUID(u.Bytes).String() }

// hasValidIndex reports whether the project's embedding partition carries a valid HNSW index.
func hasValidIndex(ctx context.Context, t *testing.T, st *store.Store, projectID pgtype.UUID) bool {
	t.Helper()
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ok, err := store.HasValidEmbeddingIndex(ctx, tx, projectID)
	if err != nil {
		t.Fatalf("check index: %v", err)
	}
	return ok
}

// TestEnsureIndexWorkerBuildsIndex proves the ensure_index worker builds the per-partition vector index with
// the composed embedder's dimension: a project with an embedding but no index has a valid HNSW index after
// the worker runs.
func TestEnsureIndexWorkerBuildsIndex(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, run := seedProjectRun(ctx, t, st)
	emb := ext.FixtureEmbedder{}

	// One consolidated memory gives the partition an embedding to index (and pins the model).
	if err := jobs.NewPGPersister(st, ext.LWW{}, emb).Persist(ctx, jobs.PersistInput{
		ProjectID: proj.ID, RunID: run.ID, ExpectedCoveredSeq: 0, CoveredSeq: 1,
		Memories: []jobs.MemoryWrite{{Kind: "semantic", Content: "auth is done", CreatedByAgent: "a", SourceSeq: 1}},
	}); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if hasValidIndex(ctx, t, st, proj.ID) {
		t.Fatal("index should not exist before the build")
	}

	w := jobs.NewEnsureIndexWorker(store.NewPgVectorIndex(st.Pool), emb)
	job := &river.Job[jobs.EnsureIndexArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 5},
		Args:   jobs.EnsureIndexArgs{ProjectID: uuidStr(proj.ID)},
	}
	if err := w.Work(ctx, job); err != nil {
		t.Fatalf("ensure_index Work: %v", err)
	}
	if !hasValidIndex(ctx, t, st, proj.ID) {
		t.Error("ensure_index worker did not leave a valid vector index")
	}
}

// recordingEnqueuer captures the projects an ensure_index build was requested for, so a persister test can
// assert the enqueue fires exactly once — on the pass that pins the model.
type recordingEnqueuer struct{ calls []pgtype.UUID }

func (r *recordingEnqueuer) EnqueueEnsureIndex(_ context.Context, _ pgx.Tx, projectID pgtype.UUID) error {
	r.calls = append(r.calls, projectID)
	return nil
}

// TestPersisterEnqueuesIndexBuildOnFirstPin proves the write path requests a one-time index build exactly on
// the pass that first pins the model: the enqueuer is called once for the first pass and NOT for a later pass
// on the already-pinned project.
func TestPersisterEnqueuesIndexBuildOnFirstPin(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, run := seedProjectRun(ctx, t, st)
	enq := &recordingEnqueuer{}
	p := jobs.NewPGPersister(st, ext.LWW{}, ext.FixtureEmbedder{}, jobs.WithIndexEnqueuer(enq))

	if err := p.Persist(ctx, jobs.PersistInput{
		ProjectID: proj.ID, RunID: run.ID, ExpectedCoveredSeq: 0, CoveredSeq: 1,
		Memories: []jobs.MemoryWrite{{Kind: "semantic", Content: "auth is done", CreatedByAgent: "a", SourceSeq: 1}},
	}); err != nil {
		t.Fatalf("persist pass 1: %v", err)
	}
	if len(enq.calls) != 1 {
		t.Fatalf("enqueue calls after first pin = %d, want 1", len(enq.calls))
	}
	if enq.calls[0] != proj.ID {
		t.Errorf("enqueued project = %s, want %s", uuidStr(enq.calls[0]), uuidStr(proj.ID))
	}

	// A second pass on the already-pinned project must NOT re-enqueue.
	if err := p.Persist(ctx, jobs.PersistInput{
		ProjectID: proj.ID, RunID: run.ID, ExpectedCoveredSeq: 1, CoveredSeq: 2,
		Memories: []jobs.MemoryWrite{{Kind: "semantic", Content: "search is pending", CreatedByAgent: "a", SourceSeq: 2}},
	}); err != nil {
		t.Fatalf("persist pass 2: %v", err)
	}
	if len(enq.calls) != 1 {
		t.Errorf("enqueue calls after a second (already-pinned) pass = %d, want still 1", len(enq.calls))
	}
}

// TestBackfillMissingIndexesEnqueuesPinnedWithoutIndex proves the startup sweep enqueues an index build for
// every project that pinned a model but has no valid index, and skips one that already has a valid index — so
// no pinned project is left permanently on the exact-scan path, and a healthy one is not rebuilt.
func TestBackfillMissingIndexesEnqueuesPinnedWithoutIndex(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	q, err := queue.NewWorker(st, ext.FixtureExtractor{}, ext.LWW{}, ext.FixtureEmbedder{}, workmem.NewDisabled(), metrics.NewNoop(), tracenoop.NewTracerProvider())
	if err != nil {
		t.Fatalf("new worker queue: %v", err)
	}

	// Two pinned projects with partitions but no index, and one pinned project that already has a valid index.
	missingA := seedPinnedProject(ctx, t, st)
	missingB := seedPinnedProject(ctx, t, st)
	healthy := seedPinnedProject(ctx, t, st)
	if err := store.NewPgVectorIndex(st.Pool).EnsureIndex(ctx, healthy, ext.FixtureEmbedder{}.Dim()); err != nil {
		t.Fatalf("build healthy index: %v", err)
	}

	n, err := q.BackfillMissingIndexes(ctx)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 2 {
		t.Errorf("backfill enqueued %d builds, want 2 (the two indexless pinned projects, not the healthy one)", n)
	}

	// Assert the enqueued builds are EXACTLY the two indexless projects — not just the count. A membership
	// swap (enqueue the healthy one, drop an indexless one) would keep the count at 2 but leave a project
	// permanently on the exact-scan path, the very thing the sweep exists to prevent.
	enqueued := ensureIndexJobProjects(ctx, t, st)
	want := map[string]bool{uuidStr(missingA): true, uuidStr(missingB): true}
	if len(enqueued) != 2 {
		t.Fatalf("enqueued ensure_index projects = %v, want the two indexless projects", enqueued)
	}
	for _, p := range enqueued {
		if !want[p] {
			t.Errorf("enqueued project %s, want only the indexless set (healthy %s must be skipped)", p, uuidStr(healthy))
		}
	}

	// A second sweep must not re-enqueue (no worker ran, so both are still missing an index): the jobs are
	// unique per project and still pending, so the enqueues coalesce and the job count stays 2.
	if _, err := q.BackfillMissingIndexes(ctx); err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if got := len(ensureIndexJobProjects(ctx, t, st)); got != 2 {
		t.Errorf("ensure_index jobs after a second sweep = %d, want 2 (unique per project, coalesced)", got)
	}
}

// ensureIndexJobProjects returns the project ids of all enqueued ensure_index jobs.
func ensureIndexJobProjects(ctx context.Context, t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, err := st.Pool.Query(ctx, `SELECT args->>'project_id' FROM river_job WHERE kind = 'ensure_index'`)
	if err != nil {
		t.Fatalf("query ensure_index jobs: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan project id: %v", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// TestEnsureIndexEndToEnd drives the whole slice against real River + ParadeDB: a fresh project's event is
// extracted, which pins the model and — through the concrete ClientFromContext enqueuer, in the persist
// transaction — enqueues the index build, which the ensure_index worker then runs. It asserts the partition
// ends with a valid vector index, pinning the in-tx enqueue path the fake-enqueuer unit test cannot reach.
func TestEnsureIndexEndToEnd(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	w, err := queue.NewWorker(st, ext.FixtureExtractor{}, ext.LWW{}, ext.FixtureEmbedder{}, workmem.NewDisabled(), metrics.NewNoop(), tracenoop.NewTracerProvider())
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = w.Stop(stopCtx)
	})

	proj, run := seedProjectRun(ctx, t, st)
	if _, err := db.New(st.Pool).InsertEvent(ctx, db.InsertEventParams{
		RunID: run.ID, AgentID: "planner", Payload: []byte(`{"memory":"deploy finished"}`),
	}); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	enqueueExtract(ctx, t, w, st, uuidStr(proj.ID), uuidStr(run.ID))

	// Extraction distils the memory and pins the model (enqueuing the build); the ensure_index worker builds
	// the index. Poll for the memory, then the valid index — proving pin → in-tx enqueue → build end to end.
	waitForMemoryCount(ctx, t, st, proj.ID, 1, 20*time.Second)
	waitForValidIndex(ctx, t, st, proj.ID, 20*time.Second)
}

// waitForValidIndex polls until the project's embedding partition carries a valid vector index.
func waitForValidIndex(ctx context.Context, t *testing.T, st *store.Store, projectID pgtype.UUID, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if hasValidIndex(ctx, t, st, projectID) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no valid vector index built for project %s within %s", uuidStr(projectID), timeout)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// seedPinnedProject creates an org + project + partitions and pins its active model to the fixture model,
// leaving no vector index — the state the startup sweep must reconcile.
func seedPinnedProject(ctx context.Context, t *testing.T, st *store.Store) pgtype.UUID {
	t.Helper()
	proj, _ := seedProjectRun(ctx, t, st)
	if _, err := st.Pool.Exec(ctx, `UPDATE projects SET active_model_id = $2 WHERE id = $1`, proj.ID, ext.FixtureEmbedder{}.ModelID()); err != nil {
		t.Fatalf("pin model: %v", err)
	}
	return proj.ID
}

var _ jobs.IndexEnqueuer = (*recordingEnqueuer)(nil)
