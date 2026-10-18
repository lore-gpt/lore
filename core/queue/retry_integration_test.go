//go:build integration

package queue_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/metrics"
	"github.com/lore-gpt/lore/core/queue"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/core/workmem"
)

// TestExtractRunRetriesOnTheWorkersSchedule proves River schedules a failed pass's retry with the extraction
// worker's own NextRetry rather than its default policy, and that the job carries the larger attempt budget.
// After the first failure the job must wait about ten seconds before it runs again; River's default would
// have brought it back after about one, which is how three attempts used to fit inside two minutes. Without
// this, the unit tests could pin a schedule nothing ever consulted.
func TestExtractRunRetriesOnTheWorkersSchedule(t *testing.T) {
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
	// The fixture extractor answers this payload with ErrExtractorUnavailable: a provider that is not answering.
	if _, err := db.New(st.Pool).InsertEvent(ctx, db.InsertEventParams{
		RunID: run.ID, AgentID: "a", Payload: []byte(`{"fixture_error":"unavailable"}`),
	}); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	enqueueExtract(ctx, t, w, st, uuid.UUID(proj.ID.Bytes).String(), uuid.UUID(run.ID.Bytes).String())

	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for {
		if time.Now().After(deadline) {
			t.Fatalf("the failed pass was not scheduled for a retry within 30s (last read: %v)", lastErr)
		}
		var state string
		var maxAttempts int
		var scheduledAt, failedAt *time.Time
		// errors is a jsonb[] with one entry per failed attempt; the last one records when this failure happened.
		lastErr = st.Pool.QueryRow(ctx, `
			SELECT state::text, max_attempts, scheduled_at, (errors[array_length(errors, 1)]->>'at')::timestamptz
			FROM river_job WHERE kind = 'extract_run' ORDER BY id DESC LIMIT 1`).
			Scan(&state, &maxAttempts, &scheduledAt, &failedAt)
		if lastErr != nil || failedAt == nil {
			time.Sleep(150 * time.Millisecond)
			continue
		}
		if state == "completed" {
			t.Fatal("the job completed despite the extractor failing")
		}
		if maxAttempts != 7 {
			t.Errorf("max_attempts = %d, want 7", maxAttempts)
		}
		// First failure: 10s, plus up to a tenth as jitter; a little slack for the two timestamps being taken
		// a moment apart.
		wait := scheduledAt.Sub(*failedAt)
		if wait < 9500*time.Millisecond || wait > 11500*time.Millisecond {
			t.Errorf("retry scheduled %v after the failure, want about 10s (River's default would be about 1s)", wait)
		}
		return
	}
}
