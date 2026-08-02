package queue

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/riverqueue/river/rivertype"
)

// recordingHandler captures slog records so a test can assert on what an operator would actually see.
type recordingHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r.Clone())
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.recs...)
}

// only returns the single captured record, failing if there is not exactly one — a report that fires
// twice (or not at all) is as wrong as one at the wrong level.
func (h *recordingHandler) only(t *testing.T) slog.Record {
	t.Helper()
	recs := h.snapshot()
	if len(recs) != 1 {
		t.Fatalf("captured %d records, want exactly 1", len(recs))
	}
	return recs[0]
}

func attrsOf(r slog.Record) map[string]string {
	out := make(map[string]string, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		out[a.Key] = a.Value.String()
		return true
	})
	return out
}

// TestJobErrorReporterLevelsSeparateRetryFromFinal pins the distinction the report exists to make. A
// retryable attempt is a warning (the queue will try again on its own); the final attempt is an error
// (the job is discarded and its work stays undone). Collapsing the two into one level is what made the
// failure easy to miss in the first place, so both directions are asserted.
func TestJobErrorReporterLevelsSeparateRetryFromFinal(t *testing.T) {
	tests := []struct {
		name      string
		attempt   int
		maxTries  int
		wantLevel slog.Level
	}{
		{name: "first of three", attempt: 1, maxTries: 3, wantLevel: slog.LevelWarn},
		{name: "middle of three", attempt: 2, maxTries: 3, wantLevel: slog.LevelWarn},
		{name: "final of three", attempt: 3, maxTries: 3, wantLevel: slog.LevelError},
		{name: "single attempt is immediately final", attempt: 1, maxTries: 1, wantLevel: slog.LevelError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &recordingHandler{}
			r := &jobErrorReporter{logger: slog.New(h)}

			job := &rivertype.JobRow{ID: 42, Kind: "extract_run", Attempt: tc.attempt, MaxAttempts: tc.maxTries}
			got := r.HandleError(context.Background(), job, errors.New("boom: the embedder and the stored vectors disagree"))

			// The handler must never change the job's fate: a non-nil result with SetCancelled would
			// discard the job on its first error instead of retrying it.
			if got != nil {
				t.Errorf("HandleError returned %+v, want nil so the configured retry schedule is kept", got)
			}

			rec := h.only(t)
			if rec.Level != tc.wantLevel {
				t.Errorf("level = %v, want %v", rec.Level, tc.wantLevel)
			}
			attrs := attrsOf(rec)
			if attrs["job_kind"] != "extract_run" {
				t.Errorf("job_kind = %q, want extract_run", attrs["job_kind"])
			}
			if attrs["job_id"] != "42" {
				t.Errorf("job_id = %q, want 42", attrs["job_id"])
			}
			// attempt/max_attempts is the whole point of the report: it tells an operator whether to
			// wait for the next attempt or to go look at the job now.
			if attrs["attempt"] != itoa(tc.attempt) || attrs["max_attempts"] != itoa(tc.maxTries) {
				t.Errorf("attempt/max_attempts = %s/%s, want %d/%d",
					attrs["attempt"], attrs["max_attempts"], tc.attempt, tc.maxTries)
			}
			// The error text is the diagnosis; a report without it sends the operator to the database.
			if attrs["error"] == "" {
				t.Error("error attribute is empty; the report carries no diagnosis")
			}
		})
	}
}

// TestJobErrorReporterPanicIsAlwaysAnErrorWithItsTrace pins that a panic is never downgraded to a
// warning just because attempts remain — a panic is a defect, and the stack is the only evidence of
// where it came from. Without a handler this path is silent too.
func TestJobErrorReporterPanicIsAlwaysAnErrorWithItsTrace(t *testing.T) {
	h := &recordingHandler{}
	r := &jobErrorReporter{logger: slog.New(h)}

	// Attempt 1 of 3: retryable, so the level here can only come from the panic rule.
	job := &rivertype.JobRow{ID: 7, Kind: "ensure_index", Attempt: 1, MaxAttempts: 3}
	got := r.HandlePanic(context.Background(), job, "nil map write", "goroutine 1 [running]:\nmain.work()")
	if got != nil {
		t.Errorf("HandlePanic returned %+v, want nil so the configured retry schedule is kept", got)
	}

	rec := h.only(t)
	if rec.Level != slog.LevelError {
		t.Errorf("level = %v, want ERROR even on a retryable attempt", rec.Level)
	}
	attrs := attrsOf(rec)
	if attrs["panic"] != "nil map write" {
		t.Errorf("panic = %q, want the panic value", attrs["panic"])
	}
	if attrs["trace"] == "" {
		t.Error("trace attribute is empty; the stack is the only evidence of where the panic came from")
	}
	if attrs["job_kind"] != "ensure_index" {
		t.Errorf("job_kind = %q, want ensure_index", attrs["job_kind"])
	}
}

// TestJobErrorReporterFallsBackToTheProcessLogger pins the zero value as usable and the fallback as
// late-bound: a caller that passes no logger still gets reports, resolved at the moment of the report
// rather than frozen at construction.
func TestJobErrorReporterFallsBackToTheProcessLogger(t *testing.T) {
	h := &recordingHandler{}
	r := &jobErrorReporter{} // no logger

	prev := slog.Default()
	// Swap AFTER the reporter exists: a reporter that captured the default eagerly would miss this.
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	job := &rivertype.JobRow{ID: 1, Kind: "extract_run", Attempt: 1, MaxAttempts: 3}
	r.HandleError(context.Background(), job, errors.New("boom"))

	if rec := h.only(t); rec.Level != slog.LevelWarn {
		t.Errorf("level = %v, want WARN", rec.Level)
	}
}

func itoa(i int) string {
	return slog.IntValue(i).String()
}
