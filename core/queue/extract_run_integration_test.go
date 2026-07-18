//go:build integration

package queue_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/jobs"
	"github.com/lore-gpt/lore/core/metrics"
	"github.com/lore-gpt/lore/core/queue"
	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/core/workmem"
)

// migratedStore starts a ParadeDB container, applies the store and River migrations, and returns an
// open store bound to it (with cleanup registered).
func migratedStore(ctx context.Context, t *testing.T) *store.Store {
	t.Helper()
	ctr, err := tcpostgres.Run(ctx, paradeDBImage,
		tcpostgres.WithDatabase("lore"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start paradedb container: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			t.Logf("terminate container: %v", err)
		}
	})
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	if err := store.RunMigrations(ctx, dsn); err != nil {
		t.Fatalf("store migrations: %v", err)
	}
	st, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	if err := queue.Migrate(ctx, st.Pool); err != nil {
		t.Fatalf("river migrate: %v", err)
	}
	return st
}

// TestPGPersisterSetRunBatch proves the persister's batch-state write: it records the handle and
// covered seq on the run through a tenant-scoped transaction, and surfaces an error (rather than
// silently orphaning the batch) when the run is not visible in the project.
func TestPGPersisterSetRunBatch(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	q := db.New(st.Pool)

	org, err := q.InsertOrganization(ctx, "acme")
	if err != nil {
		t.Fatalf("insert org: %v", err)
	}
	proj, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "a"})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	run, err := q.InsertRun(ctx, proj.ID)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	p := jobs.NewPGPersister(st, ext.LWW{}, ext.FixtureEmbedder{})
	if err := p.SetRunBatch(ctx, proj.ID, run.ID, "batch_xyz", 9); err != nil {
		t.Fatalf("SetRunBatch: %v", err)
	}
	state, err := q.GetRunExtractionState(ctx, db.GetRunExtractionStateParams{RunID: run.ID, ProjectID: proj.ID})
	if err != nil {
		t.Fatalf("GetRunExtractionState: %v", err)
	}
	if state.ExtractionBatchID == nil || *state.ExtractionBatchID != "batch_xyz" ||
		state.ExtractionBatchCoveredSeq == nil || *state.ExtractionBatchCoveredSeq != 9 {
		t.Errorf("pending batch = {id:%v seq:%v}, want {batch_xyz 9}", state.ExtractionBatchID, state.ExtractionBatchCoveredSeq)
	}

	// A run absent from the project updates no row; the persister errors rather than losing the batch.
	absent := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	if err := p.SetRunBatch(ctx, proj.ID, absent, "batch_none", 1); err == nil {
		t.Error("SetRunBatch for an absent run = nil error, want an error")
	}
}

// TestExtractRunWorkerEconomyBatchPath proves the two-phase economy flow against real Postgres: the
// first pass submits the window to the (fixture) batch and records it on the run without persisting;
// the second pass collects the ready batch, persists the distilled memory with provenance, advances
// the checkpoint to the submit-time seq, and clears the batch state.
func TestExtractRunWorkerEconomyBatchPath(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	q := db.New(st.Pool)

	org, err := q.InsertOrganization(ctx, "acme")
	if err != nil {
		t.Fatalf("insert org: %v", err)
	}
	proj, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "a"})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if err := store.CreateProjectPartitions(ctx, st.Pool, proj.ID); err != nil {
		t.Fatalf("create partitions: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE projects SET extraction_mode = 'economy' WHERE id = $1`, proj.ID); err != nil {
		t.Fatalf("set economy mode: %v", err)
	}
	run, err := q.InsertRun(ctx, proj.ID)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	ev, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "planner", Payload: []byte(`{"memory":"batched via economy"}`)})
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}

	// Call Work directly: the fixture batch is immediately ready, so the two attempts run back to back
	// deterministically without waiting on River to reschedule the poll snooze (River's snooze handling
	// is proven by the debounce tests). IdleWindow 0 lets the debounce process the run at once.
	worker := jobs.NewExtractRunWorker(db.New(st.Pool), ext.FixtureExtractor{}, jobs.NewPGPersister(st, ext.LWW{}, ext.FixtureEmbedder{}),
		jobs.Debounce{IdleWindow: 0, MaxEvents: 1, BatchPoll: time.Millisecond})
	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{
		ProjectID: uuid.UUID(proj.ID.Bytes).String(),
		RunID:     uuid.UUID(run.ID.Bytes).String(),
	}}

	memoryCount := func() int64 {
		t.Helper()
		var n int64
		if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM memories WHERE project_id = $1`, proj.ID).Scan(&n); err != nil {
			t.Fatalf("count memories: %v", err)
		}
		return n
	}
	runState := func() db.GetRunExtractionStateRow {
		t.Helper()
		s, err := q.GetRunExtractionState(ctx, db.GetRunExtractionStateParams{RunID: run.ID, ProjectID: proj.ID})
		if err != nil {
			t.Fatalf("GetRunExtractionState: %v", err)
		}
		return s
	}

	// Pass 1: submit. Records the pending batch on the run and snoozes; nothing persisted yet.
	err = worker.Work(ctx, job)
	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) {
		t.Fatalf("economy submit should snooze to poll, got %v", err)
	}
	if s := runState(); s.ExtractionBatchID == nil {
		t.Fatal("submit should record a pending batch handle on the run")
	} else if s.ExtractionBatchCoveredSeq == nil || *s.ExtractionBatchCoveredSeq != ev.Seq {
		t.Errorf("batch covered seq = %v, want %d", s.ExtractionBatchCoveredSeq, ev.Seq)
	}
	if n := memoryCount(); n != 0 {
		t.Errorf("submit must not persist yet, memory count = %d", n)
	}

	// Pass 2: collect. Persists the distilled memory, advances the checkpoint, clears the batch.
	if err := worker.Work(ctx, job); err != nil {
		t.Fatalf("collect pass: %v", err)
	}
	if n := memoryCount(); n != 1 {
		t.Fatalf("collected batch should persist the memory, count = %d want 1", n)
	}
	if s := runState(); s.ExtractionBatchID != nil || s.ExtractionBatchCoveredSeq != nil {
		t.Errorf("collect should clear the batch state, got id=%v seq=%v", s.ExtractionBatchID, s.ExtractionBatchCoveredSeq)
	}
	var covered int64
	if err := st.Pool.QueryRow(ctx, `SELECT covered_seq FROM runs WHERE id = $1`, run.ID).Scan(&covered); err != nil {
		t.Fatalf("read covered_seq: %v", err)
	}
	if covered != ev.Seq {
		t.Errorf("covered_seq = %d, want %d (the batch's submit-time seq)", covered, ev.Seq)
	}
}

// TestExtractRunCoalesces proves the per-run unique job: many enqueues for one run collapse into a
// single extract_run job, while a distinct run gets its own.
func TestExtractRunCoalesces(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	q, err := queue.New(st.Pool)
	if err != nil {
		t.Fatalf("new queue: %v", err)
	}

	enqueue := func(project, run string) {
		tx, err := st.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		if err := q.EnqueueExtract(ctx, tx, project, run); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	project := uuid.NewString()
	runA, runB := uuid.NewString(), uuid.NewString()
	enqueue(project, runA)
	enqueue(project, runA) // coalesced into the pending job for runA
	enqueue(project, runA) // coalesced
	enqueue(project, runB) // distinct run -> its own job

	var n int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind = 'extract_run'`).Scan(&n); err != nil {
		t.Fatalf("count extract_run jobs: %v", err)
	}
	if n != 2 {
		t.Errorf("extract_run jobs = %d, want 2 (one per run; same-run enqueues coalesce)", n)
	}
}

// recordingExtractor captures the window it was called with, signalling on a channel.
type recordingExtractor struct {
	ch chan ext.ExtractInput
}

func (r *recordingExtractor) Extract(_ context.Context, in ext.ExtractInput) (ext.ExtractResult, error) {
	r.ch <- in
	return ext.ExtractResult{}, nil
}

// TestExtractRunWorkerProcessesRun drives the whole path against real River + ParadeDB: seed a run
// with events (one gated tool_log), start the worker, enqueue extract_run, and assert the extractor
// receives exactly the un-gated events for that run, in seq order.
func TestExtractRunWorkerProcessesRun(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)

	rec := &recordingExtractor{ch: make(chan ext.ExtractInput, 1)}
	w, err := queue.NewWorker(st, rec, ext.LWW{}, ext.FixtureEmbedder{}, workmem.NewDisabled(), metrics.NewNoop())
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

	q := db.New(st.Pool)
	org, err := q.InsertOrganization(ctx, "acme")
	if err != nil {
		t.Fatalf("insert org: %v", err)
	}
	proj, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "a"})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	run, err := q.InsertRun(ctx, proj.ID)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	// seq 1 and 3 are extractable; the tool_log at seq 2 must be gated out of the window.
	for _, payload := range []string{`{"memory":"one"}`, `{"kind":"tool_log"}`, `{"memory":"two"}`} {
		if _, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte(payload)}); err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}

	projectID := uuid.UUID(proj.ID.Bytes).String()
	runID := uuid.UUID(run.ID.Bytes).String()
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := w.EnqueueExtract(ctx, tx, projectID, runID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	select {
	case in := <-rec.ch:
		if in.ProjectID != projectID || in.RunID != runID {
			t.Errorf("extract input identity = {%s,%s}, want {%s,%s}", in.ProjectID, in.RunID, projectID, runID)
		}
		if len(in.Events) != 2 {
			t.Fatalf("window = %d events, want 2 (the tool_log is gated out)", len(in.Events))
		}
		if in.Events[0].Seq != 1 || in.Events[1].Seq != 3 {
			t.Errorf("window seqs = [%d,%d], want [1,3]", in.Events[0].Seq, in.Events[1].Seq)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("extractor was not called within 15s")
	}
}

// TestExtractRunWorkerRetriesOnExtractorError proves that when the extractor fails, River does not
// mark the pass complete: the job attempts and becomes retryable (so later events still coalesce
// onto it), rather than silently succeeding and dropping the run's extraction. FixtureExtractor
// maps a fixture_error payload to ErrExtractorUnavailable, which the worker surfaces to River.
func TestExtractRunWorkerRetriesOnExtractorError(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)

	w, err := queue.NewWorker(st, ext.FixtureExtractor{}, ext.LWW{}, ext.FixtureEmbedder{}, workmem.NewDisabled(), metrics.NewNoop())
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

	q := db.New(st.Pool)
	org, err := q.InsertOrganization(ctx, "acme")
	if err != nil {
		t.Fatalf("insert org: %v", err)
	}
	proj, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "a"})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	run, err := q.InsertRun(ctx, proj.ID)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if _, err := q.InsertEvent(ctx, db.InsertEventParams{
		RunID: run.ID, AgentID: "a", Payload: []byte(`{"fixture_error":"unavailable"}`),
	}); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	projectID := uuid.UUID(proj.ID.Bytes).String()
	runID := uuid.UUID(run.ID.Bytes).String()
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := w.EnqueueExtract(ctx, tx, projectID, runID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Poll: once the job has attempted, it must be rescheduled for retry (retryable/available with
	// attempt >= 1), never completed.
	deadline := time.Now().Add(20 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("job did not reach a retryable state within 20s")
		}
		var attempt int
		var state string
		if err := st.Pool.QueryRow(ctx,
			`SELECT attempt, state::text FROM river_job WHERE kind = 'extract_run' ORDER BY id DESC LIMIT 1`).
			Scan(&attempt, &state); err == nil {
			if state == "completed" {
				t.Fatalf("job completed despite an extractor error (attempt=%d); want retryable", attempt)
			}
			if attempt >= 1 && (state == "retryable" || state == "available") {
				return
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// TestExtractRunWorkerDebouncesUntilIdle proves the debounce against real River: a just-written run
// (idle ~ 0 at first pickup) is not processed immediately — the job snoozes — and only once the idle
// window elapses does the extractor run. River's short-snooze optimization sets a snoozed job to
// 'available' with a future scheduled_at (the 2s window is under the ~5s scheduler interval), so the
// snooze is detected via the persisted 'snoozes' metadata, not a transient state.
//
// This assumes the first pickup lands within the 2s window; River wakes on insert via NOTIFY, so
// pickup is sub-second in practice. A pathological >2s stall between insert and first pickup would
// leave the run already idle and skip the snooze (the metadata assertion, not processing, would fail).
func TestExtractRunWorkerDebouncesUntilIdle(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)

	rec := &recordingExtractor{ch: make(chan ext.ExtractInput, 1)}
	w, err := queue.NewWorker(st, rec, ext.LWW{}, ext.FixtureEmbedder{}, workmem.NewDisabled(), metrics.NewNoop()) // DefaultDebounce: 2s idle window
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

	q := db.New(st.Pool)
	org, err := q.InsertOrganization(ctx, "acme")
	if err != nil {
		t.Fatalf("insert org: %v", err)
	}
	proj, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "a"})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	run, err := q.InsertRun(ctx, proj.ID)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if _, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte(`{"memory":"one"}`)}); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	projectID := uuid.UUID(proj.ID.Bytes).String()
	runID := uuid.UUID(run.ID.Bytes).String()
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := w.EnqueueExtract(ctx, tx, projectID, runID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The run was just written (idle ~ 0), so it must be debounced: the pass runs only after the
	// idle window elapses, not immediately on the first pickup.
	select {
	case in := <-rec.ch:
		if len(in.Events) != 1 || in.Events[0].Seq != 1 {
			t.Errorf("processed window = %+v, want the single seq-1 event", in.Events)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("extractor not called after the debounce window elapsed")
	}

	// It snoozed at least once before running — proving the fresh run was deferred, not processed
	// immediately. River records the snooze count in the job's metadata, which persists across the
	// later processing pass (and is robust to River's short-snooze state handling).
	var snoozes int
	if err := st.Pool.QueryRow(ctx,
		`SELECT coalesce((metadata->>'snoozes')::int, 0) FROM river_job WHERE kind = 'extract_run' ORDER BY id DESC LIMIT 1`).
		Scan(&snoozes); err != nil {
		t.Fatalf("read snoozes: %v", err)
	}
	if snoozes < 1 {
		t.Errorf("job snoozes = %d, want >= 1 (the fresh run must be debounced, not processed immediately)", snoozes)
	}
}

// seedProjectRun creates the org -> project -> run chain and provisions the project's memory
// partition. memories is LIST-partitioned with no default partition, so a write for an
// un-provisioned project fails loud; provisioning is a project-setup step, not the write path's job.
func seedProjectRun(ctx context.Context, t *testing.T, st *store.Store) (db.InsertProjectRow, db.InsertRunRow) {
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
	if err := store.CreateProjectPartitions(ctx, st.Pool, proj.ID); err != nil {
		t.Fatalf("create partitions: %v", err)
	}
	run, err := q.InsertRun(ctx, proj.ID)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return proj, run
}

// enqueueExtract commits an extract_run enqueue for the run on its own transaction.
func enqueueExtract(ctx context.Context, t *testing.T, w *queue.Queue, st *store.Store, projectID, runID string) {
	t.Helper()
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := w.EnqueueExtract(ctx, tx, projectID, runID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// waitForMemoryCount polls until a project has at least want memories or the timeout elapses.
func waitForMemoryCount(ctx context.Context, t *testing.T, st *store.Store, projectID pgtype.UUID, want int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var n int64
		if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM memories WHERE project_id = $1`, projectID).Scan(&n); err != nil {
			t.Fatalf("count memories: %v", err)
		}
		if n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("memories reached %d, want >= %d within %s", n, want, timeout)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// waitForCompletedExtractJobs polls until at least want extract_run jobs have reached the completed
// state. Waiting for a pass to fully complete (not just to have persisted) before enqueuing the next
// one guarantees a fresh job rather than a coalesce into a still-running pass — the completed state
// is excluded from the unique states by design, so a post-completion enqueue opens a new window.
func waitForCompletedExtractJobs(ctx context.Context, t *testing.T, st *store.Store, want int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var n int64
		if err := st.Pool.QueryRow(ctx,
			`SELECT count(*) FROM river_job WHERE kind = 'extract_run' AND state = 'completed'`).Scan(&n); err != nil {
			t.Fatalf("count completed extract_run jobs: %v", err)
		}
		if n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("completed extract_run jobs reached %d, want >= %d within %s", n, want, timeout)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// TestExtractRunPersistsMemory drives the whole write path end to end against real River + ParadeDB:
// seed a project (with its memory partition), append an event carrying a memory, run the
// FixtureExtractor worker, and assert the distilled memory lands in `memories` with its provenance
// resolved (source event id + agent) and the run checkpoint advanced past the event.
func TestExtractRunPersistsMemory(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)

	w, err := queue.NewWorker(st, ext.FixtureExtractor{}, ext.LWW{}, ext.FixtureEmbedder{}, workmem.NewDisabled(), metrics.NewNoop())
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
	q := db.New(st.Pool)
	ev, err := q.InsertEvent(ctx, db.InsertEventParams{
		RunID: run.ID, AgentID: "planner", Payload: []byte(`{"memory":"deploy finished"}`),
	})
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
	// A trailing gated tool_log at the highest seq: it distils no memory, but the checkpoint must
	// still advance PAST it, so the archived chatter at the tail is never re-read on the next pass.
	gatedEv, err := q.InsertEvent(ctx, db.InsertEventParams{
		RunID: run.ID, AgentID: "planner", Payload: []byte(`{"kind":"tool_log","data":"noise"}`),
	})
	if err != nil {
		t.Fatalf("insert gated event: %v", err)
	}

	enqueueExtract(ctx, t, w, st, uuid.UUID(proj.ID.Bytes).String(), uuid.UUID(run.ID.Bytes).String())

	// The debounce holds the pass ~2s; poll until the memory is persisted.
	waitForMemoryCount(ctx, t, st, proj.ID, 1, 20*time.Second)

	var (
		content   string
		kind      string
		srcEvent  pgtype.UUID
		createdBy *string
	)
	if err := st.Pool.QueryRow(ctx,
		`SELECT content, kind, source_event_id, created_by_agent FROM memories WHERE project_id = $1`, proj.ID).
		Scan(&content, &kind, &srcEvent, &createdBy); err != nil {
		t.Fatalf("read memory: %v", err)
	}
	if content != "deploy finished" {
		t.Errorf("content = %q, want %q", content, "deploy finished")
	}
	if kind != "semantic" {
		t.Errorf("kind = %q, want semantic (the fixture's memory kind)", kind)
	}
	if srcEvent != ev.ID {
		t.Errorf("source_event_id = %v, want the source event %v", srcEvent, ev.ID)
	}
	if createdBy == nil || *createdBy != "planner" {
		t.Errorf("created_by_agent = %v, want planner (the source event's agent)", createdBy)
	}

	// The checkpoint advanced past the trailing gated event (the highest seq read), not merely to the
	// extracted memory's seq — so the gated tail is never re-read.
	var coveredSeq int64
	if err := st.Pool.QueryRow(ctx, `SELECT covered_seq FROM runs WHERE id = $1`, run.ID).Scan(&coveredSeq); err != nil {
		t.Fatalf("read covered_seq: %v", err)
	}
	if coveredSeq != gatedEv.Seq {
		t.Errorf("covered_seq = %d, want %d (advanced past the trailing gated event, not just the extracted seq %d)", coveredSeq, gatedEv.Seq, ev.Seq)
	}
	// Exactly one memory: the gated tool_log distilled nothing.
	var memCount int64
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM memories WHERE project_id = $1`, proj.ID).Scan(&memCount); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if memCount != 1 {
		t.Errorf("memories = %d, want 1 (the gated event yields no memory)", memCount)
	}
}

// TestExtractRunCheckpointProcessesEachEventOnce proves the checkpoint makes extraction idempotent:
// a second event, appended after the first was extracted, is processed on its own pass reading only
// past the checkpoint — so the first pass's event is never re-extracted and each event yields exactly
// one memory. This is the structural payoff of covered_seq: no duplicates across coalesced passes.
func TestExtractRunCheckpointProcessesEachEventOnce(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)

	w, err := queue.NewWorker(st, ext.FixtureExtractor{}, ext.LWW{}, ext.FixtureEmbedder{}, workmem.NewDisabled(), metrics.NewNoop())
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
	q := db.New(st.Pool)
	projectID := uuid.UUID(proj.ID.Bytes).String()
	runID := uuid.UUID(run.ID.Bytes).String()

	// First event, first pass. Wait for the pass to fully complete (checkpoint now at seq 1) before
	// the second event, so the second lands on a fresh, separate pass rather than coalescing into the
	// first — which is exactly the scenario the checkpoint has to get right.
	if _, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "planner", Payload: []byte(`{"memory":"first"}`)}); err != nil {
		t.Fatalf("insert event 1: %v", err)
	}
	enqueueExtract(ctx, t, w, st, projectID, runID)
	waitForMemoryCount(ctx, t, st, proj.ID, 1, 20*time.Second)
	waitForCompletedExtractJobs(ctx, t, st, 1, 20*time.Second)

	// Second event: a fresh pass reads only past the checkpoint, so it distils just this event.
	if _, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "tester", Payload: []byte(`{"memory":"second"}`)}); err != nil {
		t.Fatalf("insert event 2: %v", err)
	}
	enqueueExtract(ctx, t, w, st, projectID, runID)
	waitForMemoryCount(ctx, t, st, proj.ID, 2, 20*time.Second)
	waitForCompletedExtractJobs(ctx, t, st, 2, 20*time.Second)

	// Exactly two memories, and "first" appears exactly once — the checkpoint kept the second pass
	// from re-reading (and re-distilling) the first pass's event. Counts are structurally stable once
	// both events are covered: a re-run reads only seq past the checkpoint, which is now empty.
	var total, firsts int64
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM memories WHERE project_id = $1`, proj.ID).Scan(&total); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if total != 2 {
		t.Errorf("memories = %d, want exactly 2 (each event distilled once, no reprocessing)", total)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM memories WHERE project_id = $1 AND content = 'first'`, proj.ID).Scan(&firsts); err != nil {
		t.Fatalf("count 'first' memories: %v", err)
	}
	if firsts != 1 {
		t.Errorf("'first' memory count = %d, want 1 (the checkpoint keeps the first event from being re-extracted)", firsts)
	}

	var coveredSeq int64
	if err := st.Pool.QueryRow(ctx, `SELECT covered_seq FROM runs WHERE id = $1`, run.ID).Scan(&coveredSeq); err != nil {
		t.Fatalf("read covered_seq: %v", err)
	}
	if coveredSeq != 2 {
		t.Errorf("covered_seq = %d, want 2 (advanced across both events)", coveredSeq)
	}
}

// TestExtractRunPersistsClaimAndEntity drives one event carrying a memory, a claim, and an entity
// mention through the FixtureExtractor worker and asserts the entity is registered, the claim is
// stored with its subject resolved to that entity, its provenance stamped (source event), and its
// memory_id linked to the memory distilled from the same event.
func TestExtractRunPersistsClaimAndEntity(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)

	w, err := queue.NewWorker(st, ext.FixtureExtractor{}, ext.LWW{}, ext.FixtureEmbedder{}, workmem.NewDisabled(), metrics.NewNoop())
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
	q := db.New(st.Pool)
	ev, err := q.InsertEvent(ctx, db.InsertEventParams{
		RunID:   run.ID,
		AgentID: "planner",
		Payload: []byte(`{"memory":"deploy done","claim":{"entity":"payment-svc","predicate":"status","value":"up"},"entities":[{"name":"payment-svc","type":"service","aliases":["pay-svc"]}]}`),
	})
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}

	enqueueExtract(ctx, t, w, st, uuid.UUID(proj.ID.Bytes).String(), uuid.UUID(run.ID.Bytes).String())
	// Memory and claim commit in one transaction, so once the memory appears the claim is present too.
	waitForMemoryCount(ctx, t, st, proj.ID, 1, 20*time.Second)

	// The entity was registered.
	var entityID pgtype.UUID
	var entityType string
	var aliases []string
	if err := st.Pool.QueryRow(ctx,
		`SELECT id, type, aliases FROM entities WHERE project_id = $1 AND name = 'payment-svc'`, proj.ID).
		Scan(&entityID, &entityType, &aliases); err != nil {
		t.Fatalf("read entity: %v", err)
	}
	if entityType != "service" {
		t.Errorf("entity type = %q, want service", entityType)
	}
	if len(aliases) != 1 || aliases[0] != "pay-svc" {
		t.Errorf("entity aliases = %v, want [pay-svc]", aliases)
	}

	// The claim resolved its subject to that entity, carries its value/predicate, links to the
	// same-event memory, and stamps provenance.
	var claimEntity pgtype.UUID
	var predicate string
	var value []byte
	var memoryLinked bool
	var srcEvent pgtype.UUID
	if err := st.Pool.QueryRow(ctx,
		`SELECT entity_id, predicate, value, memory_id IS NOT NULL, source_event_id FROM claims WHERE project_id = $1`, proj.ID).
		Scan(&claimEntity, &predicate, &value, &memoryLinked, &srcEvent); err != nil {
		t.Fatalf("read claim: %v", err)
	}
	if claimEntity != entityID {
		t.Errorf("claim entity_id = %v, want the registered entity %v", claimEntity, entityID)
	}
	if predicate != "status" || string(value) != `"up"` {
		t.Errorf("claim = {%q, %s}, want {status, \"up\"}", predicate, value)
	}
	if !memoryLinked {
		t.Error("claim from a memory-bearing event should link to that event's memory (memory_id set)")
	}
	if srcEvent != ev.ID {
		t.Errorf("claim source_event_id = %v, want the source event %v", srcEvent, ev.ID)
	}
}

// TestExtractRunClaimLWWWithinPass drives two events asserting the same subject with different values
// through a single coalesced pass and proves last-write-wins by seq: the later claim is active and the
// earlier is superseded. Both events are memory-less, so it also proves standalone claims persist with
// memory_id NULL.
func TestExtractRunClaimLWWWithinPass(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)

	w, err := queue.NewWorker(st, ext.FixtureExtractor{}, ext.LWW{}, ext.FixtureEmbedder{}, workmem.NewDisabled(), metrics.NewNoop())
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
	q := db.New(st.Pool)
	// Both events inserted before the enqueue, so the debounce coalesces them into one window (one
	// pass) — the later (seq 2) must win.
	if _, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte(`{"claim":{"entity":"auth","predicate":"state","value":"active"}}`)}); err != nil {
		t.Fatalf("insert event 1: %v", err)
	}
	if _, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte(`{"claim":{"entity":"auth","predicate":"state","value":"done"}}`)}); err != nil {
		t.Fatalf("insert event 2: %v", err)
	}

	enqueueExtract(ctx, t, w, st, uuid.UUID(proj.ID.Bytes).String(), uuid.UUID(run.ID.Bytes).String())
	// No memory is produced, so wait on the pass completing (the persist committed by then).
	waitForCompletedExtractJobs(ctx, t, st, 1, 20*time.Second)

	// Exactly one active claim for the subject, valued by the later event, and it is standalone.
	var activeID pgtype.UUID
	var activeValue []byte
	var activeMemoryLinked bool
	if err := st.Pool.QueryRow(ctx,
		`SELECT c.id, c.value, c.memory_id IS NOT NULL
		   FROM claims c JOIN entities e ON e.id = c.entity_id
		  WHERE c.project_id = $1 AND e.name = 'auth' AND c.predicate = 'state' AND c.superseded_by IS NULL`,
		proj.ID).Scan(&activeID, &activeValue, &activeMemoryLinked); err != nil {
		t.Fatalf("read active claim: %v", err)
	}
	if string(activeValue) != `"done"` {
		t.Errorf("active claim value = %s, want \"done\" (last-write-wins by seq)", activeValue)
	}
	if activeMemoryLinked {
		t.Error("a claim from a memory-less event should be standalone (memory_id NULL)")
	}

	// Two claims exist for the subject: one active, one superseded — the invariant the partial-unique
	// index enforces, reached via the deferred supersede-then-insert swap.
	var total, superseded int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE c.superseded_by IS NOT NULL)
		   FROM claims c JOIN entities e ON e.id = c.entity_id
		  WHERE c.project_id = $1 AND e.name = 'auth' AND c.predicate = 'state'`,
		proj.ID).Scan(&total, &superseded); err != nil {
		t.Fatalf("count subject claims: %v", err)
	}
	if total != 2 || superseded != 1 {
		t.Errorf("subject claims = %d (superseded %d), want 2 (1 superseded)", total, superseded)
	}

	// The superseded (earlier) claim points at the active replacement — the chain head — proving the
	// deferred supersede-then-insert swap linked them, not merely that some row happens to be superseded.
	var supersededPointsAt pgtype.UUID
	if err := st.Pool.QueryRow(ctx,
		`SELECT c.superseded_by
		   FROM claims c JOIN entities e ON e.id = c.entity_id
		  WHERE c.project_id = $1 AND e.name = 'auth' AND c.predicate = 'state' AND c.superseded_by IS NOT NULL`,
		proj.ID).Scan(&supersededPointsAt); err != nil {
		t.Fatalf("read superseded claim: %v", err)
	}
	if supersededPointsAt != activeID {
		t.Errorf("superseded claim points at %v, want the active replacement %v", supersededPointsAt, activeID)
	}
}

// TestExtractRunWorkerFieldMergeComposition proves the composed Adjudicator reaches the persister through
// the FULL worker wiring: with FieldMerge injected via queue.NewWorker, two object-valued claims about one
// subject in a coalesced pass MERGE, rather than the later one simply replacing the earlier (which the
// default LWW would do). This closes the composition-level gap — a regression that drops the threaded
// adjudicator at the worker boundary (hardcoding LWW) is caught end to end, not just in the persister
// unit-of-integration test.
func TestExtractRunWorkerFieldMergeComposition(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)

	w, err := queue.NewWorker(st, ext.FixtureExtractor{}, ext.FieldMerge{}, ext.FixtureEmbedder{}, workmem.NewDisabled(), metrics.NewNoop())
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
	q := db.New(st.Pool)
	// Two events assert the same subject with OBJECT values, both before the enqueue so they coalesce into
	// one window. FieldMerge combines them (incoming overrides); LWW would keep only the second.
	for _, payload := range []string{
		`{"claim":{"entity":"cfg","predicate":"opts","value":{"a":1,"b":2}}}`,
		`{"claim":{"entity":"cfg","predicate":"opts","value":{"b":3,"c":4}}}`,
	} {
		if _, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte(payload)}); err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}

	enqueueExtract(ctx, t, w, st, uuid.UUID(proj.ID.Bytes).String(), uuid.UUID(run.ID.Bytes).String())
	waitForCompletedExtractJobs(ctx, t, st, 1, 20*time.Second)

	var activeValue []byte
	if err := st.Pool.QueryRow(ctx,
		`SELECT c.value FROM claims c JOIN entities e ON e.id = c.entity_id
		  WHERE c.project_id = $1 AND e.name = 'cfg' AND c.predicate = 'opts' AND c.superseded_by IS NULL`,
		proj.ID).Scan(&activeValue); err != nil {
		t.Fatalf("read active claim: %v", err)
	}
	var got map[string]int
	if err := json.Unmarshal(activeValue, &got); err != nil {
		t.Fatalf("active claim value is not an object: %v (%s)", err, activeValue)
	}
	want := map[string]int{"a": 1, "b": 3, "c": 4}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("merged key %q = %d, want %d — FieldMerge did not reach the persister through the worker composition", k, got[k], v)
		}
	}
}
