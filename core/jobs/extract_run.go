// Package jobs defines the River background jobs. Phase 1 extraction is coalesced per run: events
// for one run collapse into a single extract_run job (a River unique job), which reads the run's
// events, applies a cheap gate, and hands the survivors to the Extractor.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
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

// ExtractRunWorker processes ExtractRunArgs: it debounces the run, reads the events past its
// checkpoint, gates the machine chatter, hands the survivors to the Extractor, and persists the
// distilled memories while advancing the run's checkpoint atomically. Claims and entities are
// distilled and logged but their persistence lands in a later increment.
type ExtractRunWorker struct {
	river.WorkerDefaults[ExtractRunArgs]
	source    EventSource
	extractor ext.Extractor
	persister Persister
	debounce  Debounce
}

// NewExtractRunWorker builds the worker from its event source, extractor, persister, and debounce
// window.
func NewExtractRunWorker(source EventSource, extractor ext.Extractor, persister Persister, debounce Debounce) *ExtractRunWorker {
	return &ExtractRunWorker{source: source, extractor: extractor, persister: persister, debounce: debounce}
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

	// Debounce over the events past the run's checkpoint: defer until the run has been idle for the
	// window or enough events have accumulated. Snoozing keeps the job in a unique state, so further
	// events for the run keep collapsing into it rather than enqueuing another pass. JobSnooze does
	// not consume an attempt.
	ready, err := w.source.RunExtractionReadiness(ctx, db.RunExtractionReadinessParams{ProjectID: projectID, RunID: runID})
	if err != nil {
		return fmt.Errorf("extract_run: readiness: %w", err)
	}
	if ready.EventCount == 0 {
		return nil // nothing past the checkpoint: the run is drained, so the pass is done.
	}
	if ready.EventCount < int64(w.debounce.MaxEvents) && ready.IdleSeconds < w.debounce.IdleWindow.Seconds() {
		return river.JobSnooze(w.debounce.IdleWindow)
	}

	events, err := w.source.ListRunEvents(ctx, db.ListRunEventsParams{ProjectID: projectID, RunID: runID})
	if err != nil {
		return fmt.Errorf("extract_run: list events: %w", err)
	}
	if len(events) == 0 {
		return nil // readiness saw pending events but none remain past the checkpoint: nothing to do.
	}

	// Gate machine chatter before the model; the survivors form the extraction window. bySeq indexes
	// every event READ so a candidate's provenance resolves back to its source event. The checkpoint
	// advances to the highest seq read — gated events included — so archived chatter is never re-read.
	window := make([]ext.InputEvent, 0, len(events))
	bySeq := make(map[int64]db.Event, len(events))
	gated := 0
	for _, e := range events {
		bySeq[e.Seq] = e
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
	coveredSeq := events[len(events)-1].Seq // events are seq-ordered; the last is the highest read.

	// Only invoke the extractor when there is something to distil; an all-gated window still advances
	// the checkpoint below but needs no model call.
	var res ext.ExtractResult
	if len(window) > 0 {
		res, err = w.extractor.Extract(ctx, ext.ExtractInput{
			ProjectID: job.Args.ProjectID,
			RunID:     job.Args.RunID,
			Events:    window,
		})
		if err != nil {
			return fmt.Errorf("extract_run: extract: %w", err)
		}
	}

	// Resolve each candidate memory's provenance from the event it was distilled from. A candidate
	// naming a seq outside the window is a misbehaving extractor: drop it rather than store a memory
	// with no provenance.
	memories := make([]MemoryWrite, 0, len(res.Memories))
	for _, m := range res.Memories {
		src, ok := bySeq[m.SourceSeq]
		if !ok {
			slog.WarnContext(ctx, "extract_run: candidate memory references a seq outside the window; dropped",
				slog.String("run_id", job.Args.RunID),
				slog.Int64("source_seq", m.SourceSeq))
			continue
		}
		memories = append(memories, MemoryWrite{
			Kind:           m.Kind,
			Content:        m.Content,
			SourceEventID:  src.ID,
			CreatedByAgent: src.AgentID,
			SourceSeq:      m.SourceSeq,
		})
	}

	// Entities carry through as-is (name/type/aliases); the persister registers them and resolves ids.
	entities := make([]EntityWrite, 0, len(res.Entities))
	for _, e := range res.Entities {
		entities = append(entities, EntityWrite{Name: e.Name, Type: e.Type, Aliases: e.Aliases})
	}

	// Resolve each candidate claim's provenance the same way, dropping any that name a seq outside the
	// window. Sort by SourceSeq so the persister's per-subject supersession is deterministic
	// last-write-wins regardless of the extractor's output order.
	claims := make([]ClaimWrite, 0, len(res.Claims))
	for _, c := range res.Claims {
		src, ok := bySeq[c.SourceSeq]
		if !ok {
			slog.WarnContext(ctx, "extract_run: candidate claim references a seq outside the window; dropped",
				slog.String("run_id", job.Args.RunID),
				slog.Int64("source_seq", c.SourceSeq))
			continue
		}
		// A claim's value is a NOT NULL jsonb; a candidate whose value is empty or not well-formed JSON
		// is malformed. Drop it rather than let it abort the whole coalesced pass at the insert (a
		// deterministic failure would just retry to exhaustion and strand the checkpoint). A JSON
		// `null` literal is a non-empty, valid value and is kept.
		if len(c.Value) == 0 || !json.Valid(c.Value) {
			slog.WarnContext(ctx, "extract_run: candidate claim has no valid JSON value; dropped",
				slog.String("run_id", job.Args.RunID),
				slog.Int64("source_seq", c.SourceSeq))
			continue
		}
		claims = append(claims, ClaimWrite{
			Entity:        c.Entity,
			Predicate:     c.Predicate,
			Value:         c.Value,
			EventTime:     c.EventTime,
			SourceEventID: src.ID,
			SourceSeq:     c.SourceSeq,
		})
	}
	sort.SliceStable(claims, func(i, j int) bool { return claims[i].SourceSeq < claims[j].SourceSeq })

	// Persist the memories, entities, and claims and advance the checkpoint in one transaction: a
	// committed pass moves the checkpoint past its events so they are never reprocessed, and a crashed
	// one rolls it all back together.
	if err := w.persister.Persist(ctx, PersistInput{
		ProjectID:  projectID,
		RunID:      runID,
		CoveredSeq: coveredSeq,
		Memories:   memories,
		Entities:   entities,
		Claims:     claims,
	}); err != nil {
		return fmt.Errorf("extract_run: persist: %w", err)
	}

	slog.InfoContext(ctx, "extract_run pass complete",
		slog.String("run_id", job.Args.RunID),
		slog.Int("events", len(events)),
		slog.Int("gated", gated),
		slog.Int("extracted", len(window)),
		slog.Int64("covered_seq", coveredSeq),
		slog.Int("memories", len(memories)),
		slog.Int("claims", len(claims)),
		slog.Int("entities", len(entities)))

	// Tail drain: events may have arrived while this pass ran — unique-by-state coalesced them into
	// this running job, so they got no job of their own. If any remain past the advanced checkpoint,
	// snooze to process them; otherwise the run is drained and the pass completes. This narrows but
	// does not fully close the window: an event that lands after this check yet before the job leaves
	// the running state still coalesces into this finishing job and is left for the next enqueue's
	// fresh pass to pick up (the checkpoint guarantees it is processed exactly once whenever that is).
	tail, err := w.source.RunExtractionReadiness(ctx, db.RunExtractionReadinessParams{ProjectID: projectID, RunID: runID})
	if err != nil {
		return fmt.Errorf("extract_run: tail readiness: %w", err)
	}
	if tail.EventCount > 0 {
		return river.JobSnooze(w.debounce.IdleWindow)
	}
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
