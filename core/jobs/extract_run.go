// Package jobs defines the River background jobs. Phase 1 extraction is coalesced per run: events
// for one run collapse into a single extract_run job (a River unique job), which reads the run's
// events, applies a cheap gate, and hands the survivors to the Extractor.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/store/db"
)

// ExtractRunArgs enqueues a coalesced extraction pass for one run. It is inserted on the event
// insert's transaction whenever an event is appended; the unique constraint collapses a burst of
// events for the same run into a single job.
type ExtractRunArgs struct {
	ProjectID string `json:"project_id"`
	RunID     string `json:"run_id"`
}

// Kind is the stable job identifier River persists.
func (ExtractRunArgs) Kind() string { return "extract_run" }

// InsertOpts makes extract_run a per-run unique job: while a run's job is active — available,
// pending, running, scheduled (including snoozed), or retryable — further events for that run
// coalesce into it rather than enqueuing another. Completed is deliberately excluded so a fresh
// window starts once a pass finishes. Three attempts.
func (ExtractRunArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		MaxAttempts: 3,
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRunning,
				rivertype.JobStateScheduled,
				rivertype.JobStateRetryable,
			},
		},
	}
}

// EventSource reads a run's events and its debounce readiness. *db.Queries satisfies it; tests
// supply a fake.
type EventSource interface {
	RunExtractionReadiness(ctx context.Context, arg db.RunExtractionReadinessParams) (db.RunExtractionReadinessRow, error)
	ListRunEvents(ctx context.Context, arg db.ListRunEventsParams) ([]db.Event, error)
}

// Debounce controls the coalescing window: a run's extraction pass runs once the run has been idle
// for IdleWindow, or once MaxEvents have accumulated — whichever comes first. MaxEvents also bounds
// the wait, so a run that never idles still gets processed.
type Debounce struct {
	IdleWindow time.Duration
	MaxEvents  int
}

// DefaultDebounce is the production window: process after 2s idle or 20 accumulated events.
func DefaultDebounce() Debounce {
	return Debounce{IdleWindow: 2 * time.Second, MaxEvents: 20}
}

// ExtractRunWorker processes ExtractRunArgs: it debounces the run, reads its events, gates the
// machine chatter, and hands the survivors to the Extractor. This increment does not yet persist
// the result — the memory/claim writes land in a later increment; here the pass runs end to end
// and logs its output.
type ExtractRunWorker struct {
	river.WorkerDefaults[ExtractRunArgs]
	source    EventSource
	extractor ext.Extractor
	debounce  Debounce
}

// NewExtractRunWorker builds the worker from its event source, extractor, and debounce window.
func NewExtractRunWorker(source EventSource, extractor ext.Extractor, debounce Debounce) *ExtractRunWorker {
	return &ExtractRunWorker{source: source, extractor: extractor, debounce: debounce}
}

// Work runs one coalesced extraction pass for the job's run.
func (w *ExtractRunWorker) Work(ctx context.Context, job *river.Job[ExtractRunArgs]) error {
	projectID, err := parseUUID(job.Args.ProjectID)
	if err != nil {
		return fmt.Errorf("extract_run: project_id: %w", err)
	}
	runID, err := parseUUID(job.Args.RunID)
	if err != nil {
		return fmt.Errorf("extract_run: run_id: %w", err)
	}

	// Debounce: defer until the run has been idle for the window or enough events have accumulated.
	// Snoozing keeps the job in a unique state, so further events for the run keep collapsing into it
	// rather than enqueuing another pass. JobSnooze does not consume an attempt.
	ready, err := w.source.RunExtractionReadiness(ctx, db.RunExtractionReadinessParams{ProjectID: projectID, RunID: runID})
	if err != nil {
		return fmt.Errorf("extract_run: readiness: %w", err)
	}
	if ready.EventCount > 0 &&
		ready.EventCount < int64(w.debounce.MaxEvents) &&
		ready.IdleSeconds < w.debounce.IdleWindow.Seconds() {
		return river.JobSnooze(w.debounce.IdleWindow)
	}

	events, err := w.source.ListRunEvents(ctx, db.ListRunEventsParams{ProjectID: projectID, RunID: runID})
	if err != nil {
		return fmt.Errorf("extract_run: list events: %w", err)
	}

	// Gate machine chatter before the model; the survivors form the extraction window.
	window := make([]ext.InputEvent, 0, len(events))
	gated := 0
	for _, e := range events {
		if reason := gatedReason(e.Payload); reason != "" {
			gated++
			slog.DebugContext(ctx, "extract gate: event archived",
				slog.String("run_id", job.Args.RunID),
				slog.Int64("seq", e.Seq),
				slog.String("gated_reason", reason))
			continue
		}
		window = append(window, ext.InputEvent{
			Seq:     e.Seq,
			AgentID: e.AgentID,
			Payload: json.RawMessage(e.Payload),
		})
	}

	res, err := w.extractor.Extract(ctx, ext.ExtractInput{
		ProjectID: job.Args.ProjectID,
		RunID:     job.Args.RunID,
		Events:    window,
	})
	if err != nil {
		return fmt.Errorf("extract_run: extract: %w", err)
	}

	slog.InfoContext(ctx, "extract_run pass complete",
		slog.String("run_id", job.Args.RunID),
		slog.Int("events", len(events)),
		slog.Int("gated", gated),
		slog.Int("extracted", len(window)),
		slog.Int("memories", len(res.Memories)),
		slog.Int("claims", len(res.Claims)),
		slog.Int("entities", len(res.Entities)))
	return nil
}

// parseUUID converts a canonical UUID string into a pgtype.UUID for a query parameter.
func parseUUID(s string) (pgtype.UUID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return pgtype.UUID{}, err
	}
	return pgtype.UUID{Bytes: u, Valid: true}, nil
}
