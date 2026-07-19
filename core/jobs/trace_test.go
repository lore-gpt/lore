package jobs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/lore-gpt/lore/core/jobs"
	"github.com/lore-gpt/lore/core/store/db"
)

// spanByName returns the first recorded span with the given name, or nil.
func spanByName(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

// TestExtractRunSpanUnsetOnSnooze pins the deliberate error-status carve-out in ExtractRunWorker.Work: a debounce
// snooze is the queue working as designed (and the common case for an accumulating run), NOT a failure, so the
// extract.run span must end with an UNSET status. Without the errors.As(&snooze) guard in the deferred span-end,
// every routine snooze would paint the span red and pollute trace-based error-rate dashboards. The pass is driven
// under a recording ambient span so obs.StartSpan resolves the recording provider and the span's status is
// captured. A prior version of the suite asserted the snooze RETURN VALUE but never the span status, so this
// carve-out was unguarded — a false green.
func TestExtractRunSpanUnsetOnSnooze(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	ctx, root := tp.Tracer("test").Start(context.Background(), "root")

	src := &fakeSource{
		// Under the event cap and not idle → the debounce snoozes rather than processing.
		readiness: db.RunExtractionReadinessRow{EventCount: 3, IdleSeconds: 0.5},
		events:    []db.Event{{Seq: 1, AgentID: "a", Payload: []byte(`{"memory":"x"}`)}},
	}
	w := jobs.NewExtractRunWorker(src, &spyExtractor{}, &spyPersister{}, jobs.Debounce{IdleWindow: 2 * time.Second, MaxEvents: 20})
	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}

	err := w.Work(ctx, job)
	root.End()

	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) {
		t.Fatalf("expected a snooze from the accumulating run, got %v", err)
	}
	span := spanByName(sr.Ended(), "extract.run")
	if span == nil {
		t.Fatal("no extract.run span recorded")
	}
	if got := span.Status().Code; got != codes.Unset {
		t.Errorf("extract.run span status = %v on a snooze, want Unset (a snooze is not an error)", got)
	}
}
