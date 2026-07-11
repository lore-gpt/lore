// Package jobs defines the River background jobs. Phase 0 ships a single stub
// (extract_event); Phase 1 turns it into the real extraction pipeline.
package jobs

import (
	"context"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
)

// ExtractEventArgs is enqueued (in the same transaction as the event insert)
// when an event is appended. Phase 1 extraction reads the event by id.
type ExtractEventArgs struct {
	EventID string `json:"event_id"`
}

// Kind is the stable job identifier River persists.
func (ExtractEventArgs) Kind() string { return "extract_event" }

// InsertOpts sets the retry policy for extraction jobs (3 attempts total).
func (ExtractEventArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 3}
}

// ExtractEventWorker processes ExtractEventArgs jobs. Phase 0 is a stub: it logs
// and simulates a little work so the write path can be exercised end to end.
type ExtractEventWorker struct {
	river.WorkerDefaults[ExtractEventArgs]
}

// NewExtractEventWorker returns the stub extraction worker.
func NewExtractEventWorker() *ExtractEventWorker {
	return &ExtractEventWorker{}
}

// Work logs the stub line and sleeps 50ms, honoring context cancellation.
func (w *ExtractEventWorker) Work(ctx context.Context, job *river.Job[ExtractEventArgs]) error {
	slog.InfoContext(ctx, "extract stub: event received", slog.String("event_id", job.Args.EventID))
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(50 * time.Millisecond):
		return nil
	}
}
