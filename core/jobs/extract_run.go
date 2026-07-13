// Package jobs defines the River background jobs. Phase 1 extraction is coalesced per run: events
// for one run collapse into a single extract_run job (a River unique job), which reads the run's
// events, applies a cheap gate, and hands the survivors to the Extractor.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/lore-gpt/lore/core/workmem"
)

// workmemSetTimeout bounds a single hot-lane write from the worker so a stalled cache degrades the pass
// quickly (falling the fact through to a durable claim) rather than hanging the job.
const workmemSetTimeout = 2 * time.Second

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

// modeEconomy is the projects.extraction_mode value that routes a run's extraction through a
// provider's batch interface (submit now, collect later) instead of a synchronous call.
const modeEconomy = "economy"

// EventSource reads a run's events, its debounce readiness, and its extraction mode + pending batch.
// *db.Queries satisfies it; tests supply a fake.
type EventSource interface {
	RunExtractionReadiness(ctx context.Context, arg db.RunExtractionReadinessParams) (db.RunExtractionReadinessRow, error)
	ListRunEvents(ctx context.Context, arg db.ListRunEventsParams) ([]db.Event, error)
	GetRunExtractionState(ctx context.Context, arg db.GetRunExtractionStateParams) (db.GetRunExtractionStateRow, error)
}

// Debounce controls the coalescing window: a run's extraction pass runs once the run has been idle
// for IdleWindow, or once MaxEvents have accumulated — whichever comes first. MaxEvents also bounds
// the wait, so a run that never idles still gets processed.
type Debounce struct {
	IdleWindow time.Duration
	MaxEvents  int
	// BatchPoll is how often an economy-mode pass re-checks its submitted batch for completion,
	// snoozing between attempts until the provider finishes.
	BatchPoll time.Duration
}

// DefaultDebounce is the production window: process after 2s idle or 20 accumulated events, and poll
// a submitted economy batch every minute.
func DefaultDebounce() Debounce {
	return Debounce{IdleWindow: 2 * time.Second, MaxEvents: 20, BatchPoll: 60 * time.Second}
}

// ExtractRunWorker processes ExtractRunArgs: it debounces the run, reads the events past its
// checkpoint, gates the machine chatter, and distils the survivors — synchronously for a realtime
// run, or via the provider's batch interface for an economy run (submit in one attempt, collect in a
// later one). Either way it persists the distilled memories, entities, and claims while advancing the
// run's checkpoint atomically.
type ExtractRunWorker struct {
	river.WorkerDefaults[ExtractRunArgs]
	source    EventSource
	extractor ext.Extractor
	persister Persister
	debounce  Debounce
	workmem   workmem.Store
}

// ExtractRunOption configures optional ExtractRunWorker dependencies.
type ExtractRunOption func(*ExtractRunWorker)

// WithWorkmemStore injects the working-memory store used to route kind:"state" events: a healthy stripe
// takes the hot fact and skips extraction; a disabled or degraded one (or a failed write) preserves it as
// a durable claim instead. Without it the worker defaults to the disabled no-op, so state facts become
// durable claims.
func WithWorkmemStore(s workmem.Store) ExtractRunOption {
	return func(w *ExtractRunWorker) { w.workmem = s }
}

// NewExtractRunWorker builds the worker from its event source, extractor, persister, and debounce
// window. Options inject optional dependencies (the working-memory store); by default the store is the
// disabled no-op.
func NewExtractRunWorker(source EventSource, extractor ext.Extractor, persister Persister, debounce Debounce, opts ...ExtractRunOption) *ExtractRunWorker {
	w := &ExtractRunWorker{source: source, extractor: extractor, persister: persister, debounce: debounce, workmem: workmem.NewDisabled()}
	for _, o := range opts {
		o(w)
	}
	if w.workmem == nil {
		w.workmem = workmem.NewDisabled()
	}
	return w
}

// Work runs one coalesced extraction pass for the job's run. A run may be in one of two phases: if an
// earlier attempt submitted an economy batch that is still processing, this attempt collects it;
// otherwise it debounces, reads the window, and either distils it synchronously (realtime) or submits
// it to the batch interface and defers collection (economy).
func (w *ExtractRunWorker) Work(ctx context.Context, job *river.Job[ExtractRunArgs]) error {
	projectID, err := parseUUID(job.Args.ProjectID)
	if err != nil {
		return fmt.Errorf("extract_run: project_id: %w", err)
	}
	runID, err := parseUUID(job.Args.RunID)
	if err != nil {
		return fmt.Errorf("extract_run: run_id: %w", err)
	}

	state, err := w.source.GetRunExtractionState(ctx, db.GetRunExtractionStateParams{RunID: runID, ProjectID: projectID})
	if err != nil {
		return fmt.Errorf("extract_run: state: %w", err)
	}

	// Collect phase: an earlier attempt submitted a batch for this run that is awaiting its result.
	if state.ExtractionBatchID != nil {
		if state.ExtractionBatchCoveredSeq == nil {
			return fmt.Errorf("extract_run: pending batch with no covered seq (corrupt run state)")
		}
		return w.collectBatch(ctx, job, projectID, runID, state.CoveredSeq, *state.ExtractionBatchID, *state.ExtractionBatchCoveredSeq)
	}

	// Submit / realtime phase: debounce over the events past the checkpoint, then read them. Snoozing
	// keeps the job in a unique state, so further events for the run keep collapsing into it rather
	// than enqueuing another pass. JobSnooze does not consume an attempt.
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

	window, stateEvents, bySeq, gated := gate(ctx, job.Args.RunID, events)
	coveredSeq := events[len(events)-1].Seq // events are seq-ordered; the last is the highest read.
	slog.InfoContext(ctx, "extract_run window",
		slog.String("run_id", job.Args.RunID),
		slog.Int("events", len(events)),
		slog.Int("gated", gated),
		slog.Int("state", len(stateEvents)),
		slog.Int("extracted", len(window)),
		slog.String("mode", state.ExtractionMode))

	// Economy mode: submit the window to the provider's batch interface and defer collection to a
	// later attempt. An all-gated window (nothing to submit) falls through to advance the checkpoint
	// synchronously below.
	if state.ExtractionMode == modeEconomy && len(window) > 0 {
		if batch, ok := w.extractor.(ext.BatchExtractor); ok {
			// Submit, then record the handle so a later attempt can collect it. These two steps are not
			// atomic — one is a provider call, the other a DB write — so a crash between them orphans the
			// submitted batch: its handle is never recorded, so it is never collected, and the retry
			// resubmits the same append-only window. That is at-least-once submission with exactly-once
			// persistence (the checkpoint advances only in the collect-time transaction), so the worst
			// case is a wasted batch, never a duplicated memory or a skipped event.
			handle, err := batch.SubmitBatch(ctx, ext.ExtractInput{ProjectID: job.Args.ProjectID, RunID: job.Args.RunID, Events: window})
			if err != nil {
				return fmt.Errorf("extract_run: submit batch: %w", err)
			}
			if err := w.persister.SetRunBatch(ctx, projectID, runID, handle, coveredSeq); err != nil {
				return fmt.Errorf("extract_run: record batch: %w", err)
			}
			slog.InfoContext(ctx, "extract_run batch submitted",
				slog.String("run_id", job.Args.RunID), slog.Int64("covered_seq", coveredSeq))
			return river.JobSnooze(w.debounce.BatchPoll) // collect on a later attempt.
		}
		// Economy configured but this extractor has no batch capability: distil synchronously rather
		// than stall. (A real economy deployment pairs the mode with a batch-capable provider.)
		slog.WarnContext(ctx, "extract_run: economy mode but extractor is not batch-capable; distilling synchronously",
			slog.String("run_id", job.Args.RunID))
	}

	// Realtime (or the economy fallback): distil the window synchronously in this attempt.
	var res ext.ExtractResult
	if len(window) > 0 {
		res, err = w.extractor.Extract(ctx, ext.ExtractInput{ProjectID: job.Args.ProjectID, RunID: job.Args.RunID, Events: window})
		if err != nil {
			return fmt.Errorf("extract_run: extract: %w", err)
		}
	}
	return w.persistAndDrain(ctx, job, projectID, runID, state.CoveredSeq, coveredSeq, bySeq, stateEvents, res)
}

// collectBatch handles the economy collect phase: it polls the run's pending batch and, once the
// provider has finished, re-reads the window the batch covered to rebuild provenance and persists the
// result — advancing the checkpoint to the submit-time seq and clearing the batch state atomically.
// expectedCoveredSeq is the checkpoint the collect started from (unchanged since submit, which moved only
// the batch columns), which the persist compare-and-swaps on.
func (w *ExtractRunWorker) collectBatch(ctx context.Context, job *river.Job[ExtractRunArgs], projectID, runID pgtype.UUID, expectedCoveredSeq int64, handle string, batchCoveredSeq int64) error {
	batch, ok := w.extractor.(ext.BatchExtractor)
	if !ok {
		// A batch was submitted by an earlier attempt but this worker's extractor cannot collect it
		// (the provider configuration changed). Fail loudly; the run stays put — its checkpoint has not
		// advanced — until a batch-capable provider is restored.
		return fmt.Errorf("extract_run: run has a pending batch but the extractor is not batch-capable")
	}

	res, done, err := batch.CollectBatch(ctx, handle)
	if err != nil {
		return fmt.Errorf("extract_run: collect batch: %w", err)
	}
	if !done {
		return river.JobSnooze(w.debounce.BatchPoll) // not ready yet; poll again later.
	}

	// Re-read the events the batch covered (past the checkpoint, up to the submit-time seq) to rebuild
	// provenance. Events are append-only, so this is the same set the window was built from; anything
	// beyond batchCoveredSeq arrived after submission and is left for the next pass by the tail drain.
	events, err := w.source.ListRunEvents(ctx, db.ListRunEventsParams{ProjectID: projectID, RunID: runID})
	if err != nil {
		return fmt.Errorf("extract_run: collect list events: %w", err)
	}
	bySeq := make(map[int64]db.Event, len(events))
	var stateEvents []db.Event
	for _, e := range events {
		if e.Seq > batchCoveredSeq {
			break // seq-ordered: the rest arrived after the batch window.
		}
		bySeq[e.Seq] = e
		if isStateEvent(e.Payload) {
			stateEvents = append(stateEvents, e) // state facts were excluded from the batch; route them now.
		}
	}

	slog.InfoContext(ctx, "extract_run batch collected",
		slog.String("run_id", job.Args.RunID),
		slog.Int64("covered_seq", batchCoveredSeq),
		slog.Int("memories", len(res.Memories)),
		slog.Int("claims", len(res.Claims)),
		slog.Int("entities", len(res.Entities)))
	return w.persistAndDrain(ctx, job, projectID, runID, expectedCoveredSeq, batchCoveredSeq, bySeq, stateEvents, res)
}

// persistAndDrain resolves the extractor's candidates against the read events, persists them while
// advancing the checkpoint from expectedCoveredSeq to coveredSeq (clearing any batch state in the same
// transaction), then drains any tail that arrived while the pass ran.
func (w *ExtractRunWorker) persistAndDrain(ctx context.Context, job *river.Job[ExtractRunArgs], projectID, runID pgtype.UUID, expectedCoveredSeq, coveredSeq int64, bySeq map[int64]db.Event, stateEvents []db.Event, res ext.ExtractResult) error {
	memories, entities, claims := resolveCandidates(ctx, job.Args.RunID, bySeq, res)

	// Route kind:"state" events: a healthy stripe takes each hot fact (skipped here); a disabled/degraded
	// stripe (or a failed write) turns it into a durable claim so it is not lost. The claims join the
	// extracted ones and re-sort by SourceSeq so per-subject supersession stays last-write-wins.
	if stateClaims := w.routeStateFacts(ctx, projectID, runID, stateEvents); len(stateClaims) > 0 {
		claims = append(claims, stateClaims...)
		sort.SliceStable(claims, func(i, j int) bool { return claims[i].SourceSeq < claims[j].SourceSeq })
	}

	// Persist the memories, entities, and claims and advance the checkpoint in one transaction: a
	// committed pass moves the checkpoint past its events so they are never reprocessed, and a crashed
	// one rolls it all back together.
	if err := w.persister.Persist(ctx, PersistInput{
		ProjectID:          projectID,
		RunID:              runID,
		ExpectedCoveredSeq: expectedCoveredSeq,
		CoveredSeq:         coveredSeq,
		Memories:           memories,
		Entities:           entities,
		Claims:             claims,
	}); err != nil {
		// A concurrent pass for this run advanced the checkpoint first: it owns this window and its tail,
		// so this pass's writes were rolled back and there is nothing more to do.
		if errors.Is(err, errCheckpointConflict) {
			slog.InfoContext(ctx, "extract_run: checkpoint advanced concurrently; another pass owns the window",
				slog.String("run_id", job.Args.RunID))
			return nil
		}
		return fmt.Errorf("extract_run: persist: %w", err)
	}

	slog.InfoContext(ctx, "extract_run pass complete",
		slog.String("run_id", job.Args.RunID),
		slog.Int64("covered_seq", coveredSeq),
		slog.Int("memories", len(memories)),
		slog.Int("claims", len(claims)),
		slog.Int("entities", len(entities)))

	// Tail drain: events may have arrived while this pass ran — unique-by-state coalesced them into
	// this job, so they got no job of their own. If any remain past the advanced checkpoint, snooze to
	// process them; otherwise the run is drained and the pass completes. This narrows but does not fully
	// close the window: an event that lands after this check yet before the job leaves the running state
	// still coalesces into this finishing job and is left for the next enqueue's fresh pass to pick up
	// (the checkpoint guarantees it is processed exactly once whenever that is).
	tail, err := w.source.RunExtractionReadiness(ctx, db.RunExtractionReadinessParams{ProjectID: projectID, RunID: runID})
	if err != nil {
		return fmt.Errorf("extract_run: tail readiness: %w", err)
	}
	if tail.EventCount > 0 {
		return river.JobSnooze(w.debounce.IdleWindow)
	}
	return nil
}

// routeStateFacts sends each kind:"state" event to the run's working memory and returns the durable-claim
// fallbacks for the ones the hot lane could not take. Healthy stripe: write the fact and let the hot lane
// own it. The write is an unguarded overwrite, so it converges a dropped event-time write-through
// eventually, not immediately: a delayed pass could momentarily re-emit an older value over a newer
// concurrent write-through for the same subject, but the checkpoint keeps that newer event pending, so the
// next coalesced pass restores it (the hot lane is a best-effort cache; durability is never at risk).
// Disabled, degraded, or a failed write: preserve the fact as a durable claim through the same
// entity-upsert + last-write-wins path the extractor's claims use, so a same-run reader still sees it. The
// event log is the durability floor,
// so this fallback covers only writes made DURING an outage; hot state written before an outage cools when
// the stripe expires it (a run-close snapshot to persist end-of-run state is future work). A malformed
// state event is dropped rather than sent to the model — the server validates state facts at ingestion, so
// this is only a non-standard or pre-validation event.
func (w *ExtractRunWorker) routeStateFacts(ctx context.Context, projectID, runID pgtype.UUID, stateEvents []db.Event) []ClaimWrite {
	if len(stateEvents) == 0 {
		return nil
	}
	var claims []ClaimWrite
	hotLane := 0
	for _, e := range stateEvents { // seq-ordered, so a same-subject supersession stays last-write-wins.
		fact, err := workmem.DecodeStateFact(e.Payload)
		if err != nil {
			slog.WarnContext(ctx, "extract_run: malformed state event dropped",
				slog.String("run_id", uuidString(runID)), slog.Int64("seq", e.Seq))
			continue
		}
		if w.workmem.Mode() == workmem.Healthy {
			sctx, cancel := context.WithTimeout(ctx, workmemSetTimeout)
			err := w.workmem.Set(sctx, workmem.Key{
				ProjectID: uuidString(projectID),
				RunID:     uuidString(runID),
				Entity:    fact.Entity,
				Predicate: fact.Predicate,
			}, workmem.Value{Value: fact.Value, Seq: e.Seq, Agent: e.AgentID})
			cancel()
			if err == nil {
				hotLane++
				continue // the hot lane owns it; nothing durable.
			}
			// The write failed under a nominally-healthy stripe (which just flipped Degraded): fall through
			// to a durable claim so the fact is not lost.
		}
		claims = append(claims, ClaimWrite{
			Entity:        fact.Entity,
			Predicate:     fact.Predicate,
			Value:         fact.Value,
			SourceEventID: e.ID,
			SourceSeq:     e.Seq,
		})
	}
	if hotLane > 0 || len(claims) > 0 {
		slog.InfoContext(ctx, "extract_run state facts routed",
			slog.String("run_id", uuidString(runID)),
			slog.Int("hot_lane", hotLane),
			slog.Int("durable", len(claims)))
	}
	return claims
}

// gate splits the read events three ways: kind:"state" events (routed to working memory, never the
// model), archived machine chatter (kept raw, never the model), and the extraction window (survivors). It
// returns the window, the state events, an index of every event read (so a candidate's provenance
// resolves back to its source event), and the count gated out.
func gate(ctx context.Context, runID string, events []db.Event) (window []ext.InputEvent, stateEvents []db.Event, bySeq map[int64]db.Event, gated int) {
	window = make([]ext.InputEvent, 0, len(events))
	bySeq = make(map[int64]db.Event, len(events))
	for _, e := range events {
		bySeq[e.Seq] = e
		if isStateEvent(e.Payload) {
			stateEvents = append(stateEvents, e) // routed to the hot lane or a durable claim, not the model.
			continue
		}
		if reason := gatedReason(e.Payload); reason != "" {
			gated++
			slog.DebugContext(ctx, "extract gate: event archived",
				slog.String("run_id", runID),
				slog.Int64("seq", e.Seq),
				slog.String("gated_reason", reason))
			continue
		}
		window = append(window, ext.InputEvent{Seq: e.Seq, AgentID: e.AgentID, Payload: json.RawMessage(e.Payload)})
	}
	return window, stateEvents, bySeq, gated
}

// resolveCandidates maps the extractor's candidates to writes, resolving each one's provenance from
// the event it was distilled from. A candidate naming a seq outside the read window is a misbehaving
// extractor and is dropped rather than stored without provenance; a claim whose value is empty or not
// well-formed JSON is dropped rather than allowed to abort the whole pass at the NOT NULL jsonb insert
// (a JSON null literal is a non-empty valid value and is kept). Claims are sorted by SourceSeq so the
// persister's per-subject supersession is deterministic last-write-wins regardless of output order.
func resolveCandidates(ctx context.Context, runID string, bySeq map[int64]db.Event, res ext.ExtractResult) ([]MemoryWrite, []EntityWrite, []ClaimWrite) {
	memories := make([]MemoryWrite, 0, len(res.Memories))
	for _, m := range res.Memories {
		src, ok := bySeq[m.SourceSeq]
		if !ok {
			slog.WarnContext(ctx, "extract_run: candidate memory references a seq outside the window; dropped",
				slog.String("run_id", runID), slog.Int64("source_seq", m.SourceSeq))
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

	claims := make([]ClaimWrite, 0, len(res.Claims))
	for _, c := range res.Claims {
		src, ok := bySeq[c.SourceSeq]
		if !ok {
			slog.WarnContext(ctx, "extract_run: candidate claim references a seq outside the window; dropped",
				slog.String("run_id", runID), slog.Int64("source_seq", c.SourceSeq))
			continue
		}
		if len(c.Value) == 0 || !json.Valid(c.Value) {
			slog.WarnContext(ctx, "extract_run: candidate claim has no valid JSON value; dropped",
				slog.String("run_id", runID), slog.Int64("source_seq", c.SourceSeq))
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
	return memories, entities, claims
}

// parseUUID converts a canonical UUID string into a pgtype.UUID for a query parameter.
func parseUUID(s string) (pgtype.UUID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return pgtype.UUID{}, err
	}
	return pgtype.UUID{Bytes: u, Valid: true}, nil
}
