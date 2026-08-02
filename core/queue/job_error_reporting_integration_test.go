//go:build integration

package queue_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/metrics"
	"github.com/lore-gpt/lore/core/queue"
	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/core/workmem"
)

// capturingHandler collects slog records emitted from the worker's goroutines.
type capturingHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r.Clone())
	return nil
}

func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

// jobFailures returns the records this change exists to produce, identified by the attribute set a
// job report carries rather than by message text.
func (h *capturingHandler) jobFailures() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []slog.Record
	for _, r := range h.recs {
		var isJobReport bool
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "job_kind" {
				isJobReport = true
				return false
			}
			return true
		})
		if isJobReport {
			out = append(out, r.Clone())
		}
	}
	return out
}

// failingExtractor fails every pass with a recognisable error, standing in for any durable failure
// that leaves the job retrying — a model mismatch, an unreachable provider, a bad credential.
type failingExtractor struct{}

func (failingExtractor) Extract(context.Context, ext.ExtractInput) (ext.ExtractResult, error) {
	return ext.ExtractResult{}, errors.New("sentinel-extractor-failure")
}

func recordAttrs(r slog.Record) map[string]string {
	out := make(map[string]string, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		out[a.Key] = a.Value.String()
		return true
	})
	return out
}

// seedRunWithEvents creates an org/project/run with one extractable event and returns the ids.
func seedRunWithEvents(ctx context.Context, t *testing.T, st *store.Store, name string) (projectID, runID string) {
	t.Helper()
	q := db.New(st.Pool)
	org, err := q.InsertOrganization(ctx, name)
	if err != nil {
		t.Fatalf("insert org: %v", err)
	}
	proj, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: name})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	run, err := q.InsertRun(ctx, proj.ID)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if _, err := q.InsertEvent(ctx, db.InsertEventParams{
		RunID: run.ID, AgentID: "a", Payload: []byte(`{"memory":"one"}`),
	}); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	return uuid.UUID(proj.ID.Bytes).String(), uuid.UUID(run.ID.Bytes).String()
}

func startWorker(ctx context.Context, t *testing.T, st *store.Store, extractor ext.Extractor, h slog.Handler) *queue.Queue {
	t.Helper()
	w, err := queue.NewWorker(st, extractor, ext.LWW{}, ext.FixtureEmbedder{}, workmem.NewDisabled(),
		metrics.NewNoop(), tracenoop.NewTracerProvider(), queue.WithJobErrorLogger(slog.New(h)))
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
	return w
}

func enqueue(ctx context.Context, t *testing.T, st *store.Store, w *queue.Queue, projectID, runID string) {
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

// TestFailedJobIsReportedOnTheWorkerLog is the wiring proof. A failure that the queue records durably
// and retries was, until now, invisible on the worker's own log — it reports failures at info level
// through a logger it builds at warn, so an operator watching the process saw a pass start and never
// finish, with no error line to explain it.
//
// The test drives a real failure through the real client (a failing extractor, the production worker
// construction) and asserts the operator-visible report exists and carries what a diagnosis needs:
// the kind, the position in the retry budget, and the error text. Removing the handler from the client
// configuration makes this fail.
func TestFailedJobIsReportedOnTheWorkerLog(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)

	h := &capturingHandler{}
	w := startWorker(ctx, t, st, failingExtractor{}, h)
	projectID, runID := seedRunWithEvents(ctx, t, st, "reported")
	enqueue(ctx, t, st, w, projectID, runID)

	// The pass debounces before it extracts, so the failure arrives a couple of seconds in.
	var failures []slog.Record
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if failures = h.jobFailures(); len(failures) > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(failures) == 0 {
		t.Fatal("no job failure was reported on the worker log; a failing pass is still a silent stall")
	}

	rec := failures[0]
	if rec.Level != slog.LevelWarn {
		t.Errorf("first failure logged at %v, want WARN (attempt 1 of 3 is retryable)", rec.Level)
	}
	attrs := recordAttrs(rec)
	if attrs["job_kind"] != "extract_run" {
		t.Errorf("job_kind = %q, want extract_run", attrs["job_kind"])
	}
	// The error text must reach the log, not just the database — otherwise the report tells an
	// operator that something failed without telling them what.
	if !strings.Contains(attrs["error"], "sentinel-extractor-failure") {
		t.Errorf("error = %q, want it to carry the underlying failure", attrs["error"])
	}
	if attrs["attempt"] != "1" || attrs["max_attempts"] != "3" {
		t.Errorf("attempt/max_attempts = %s/%s, want 1/3", attrs["attempt"], attrs["max_attempts"])
	}
}

// TestSnoozedPassIsNotReportedAsAFailure locks the quiet side of the same wiring. Debouncing is
// implemented by snoozing, which the queue delivers as a sentinel error; a report that fired on it
// would put a warning on the log for every normal pass and drown the failures this change exists to
// surface. The assertion is only meaningful if a snooze actually happened, so the job's own snooze
// counter is checked first — otherwise a green result would prove nothing.
func TestSnoozedPassIsNotReportedAsAFailure(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)

	h := &capturingHandler{}
	w := startWorker(ctx, t, st, ext.FixtureExtractor{}, h)
	projectID, runID := seedRunWithEvents(ctx, t, st, "quiet")
	enqueue(ctx, t, st, w, projectID, runID)

	// Wait for the pass to actually snooze at least once.
	var snoozes int
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if err := st.Pool.QueryRow(ctx,
			`SELECT coalesce((metadata->>'snoozes')::int, 0) FROM river_job WHERE kind = 'extract_run'`,
		).Scan(&snoozes); err == nil && snoozes > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if snoozes == 0 {
		t.Fatal("the pass never snoozed, so this test would pass even if snoozes were reported as failures")
	}

	if failures := h.jobFailures(); len(failures) > 0 {
		attrs := recordAttrs(failures[0])
		t.Errorf("a snoozed pass produced %d job failure report(s); first: level=%v error=%q",
			len(failures), failures[0].Level, attrs["error"])
	}
}
