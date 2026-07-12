//go:build integration

package queue_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/queue"
	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
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
	w, err := queue.NewWorker(st.Pool, rec)
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

	w, err := queue.NewWorker(st.Pool, ext.FixtureExtractor{})
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
