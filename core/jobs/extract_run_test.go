package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/jobs"
	"github.com/lore-gpt/lore/core/metrics"
	"github.com/lore-gpt/lore/core/store/db"
)

func TestExtractRunArgs_Kind(t *testing.T) {
	if got := (jobs.ExtractRunArgs{}).Kind(); got != "extract_run" {
		t.Errorf("Kind() = %q, want extract_run", got)
	}
}

func TestExtractRunArgs_Unique(t *testing.T) {
	opts := (jobs.ExtractRunArgs{}).InsertOpts()
	if opts.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d, want 3", opts.MaxAttempts)
	}
	if !opts.UniqueOpts.ByArgs {
		t.Error("UniqueOpts.ByArgs = false, want true (coalesce per run)")
	}
	states := map[rivertype.JobState]bool{}
	for _, s := range opts.UniqueOpts.ByState {
		states[s] = true
	}
	// Completed must be excluded so a fresh window opens once a pass finishes.
	if states[rivertype.JobStateCompleted] {
		t.Error("ByState includes Completed; a finished pass must not block the next window")
	}
	// The active states must all be present — including Retryable (the one River treats as optional),
	// so events still coalesce onto a pass that is retrying after an extractor failure.
	want := []rivertype.JobState{
		rivertype.JobStateAvailable, rivertype.JobStatePending,
		rivertype.JobStateRunning, rivertype.JobStateScheduled,
		rivertype.JobStateRetryable,
	}
	for _, req := range want {
		if !states[req] {
			t.Errorf("ByState missing state %v", req)
		}
	}
	if len(opts.UniqueOpts.ByState) != len(want) {
		t.Errorf("ByState has %d members, want exactly %d (no accidental additions)", len(opts.UniqueOpts.ByState), len(want))
	}
}

// fakeSource returns a canned event set and readiness, recording the ListRunEvents params. Readiness
// is stateful: the first call (the debounce decision) returns readiness, and every later call — the
// worker's post-persist tail check — returns tailReadiness (zero value = drained, no tail).
type fakeSource struct {
	events         []db.Event
	readiness      db.RunExtractionReadinessRow
	tailReadiness  db.RunExtractionReadinessRow
	readinessCalls int
	gotArg         db.ListRunEventsParams
	// state defaults to the zero value: an empty mode (!= "economy", so the realtime path) and no
	// pending batch — which is what the pre-existing realtime tests expect.
	state db.GetRunExtractionStateRow
}

func (f *fakeSource) RunExtractionReadiness(_ context.Context, _ db.RunExtractionReadinessParams) (db.RunExtractionReadinessRow, error) {
	f.readinessCalls++
	if f.readinessCalls == 1 {
		return f.readiness, nil
	}
	return f.tailReadiness, nil
}

// ListRunEvents honours WindowLimit the way the real query's LIMIT does. A fake that returned everything
// regardless would let a test "prove" the window is bounded while the worker still handed the extractor the
// whole backlog — the assertion would be about the fake, not the code.
func (f *fakeSource) ListRunEvents(_ context.Context, arg db.ListRunEventsParams) ([]db.Event, error) {
	f.gotArg = arg
	if n := int(arg.WindowLimit); n > 0 && n < len(f.events) {
		return f.events[:n], nil
	}
	return f.events, nil
}

func (f *fakeSource) GetRunExtractionState(_ context.Context, _ db.GetRunExtractionStateParams) (db.GetRunExtractionStateRow, error) {
	return f.state, nil
}

// spyExtractor records the window it was called with and returns a canned result.
type spyExtractor struct {
	got    ext.ExtractInput
	calls  int
	result ext.ExtractResult
}

func (s *spyExtractor) Extract(_ context.Context, in ext.ExtractInput) (ext.ExtractResult, error) {
	s.got = in
	s.calls++
	return s.result, nil
}

// spyPersister records the last unit it was asked to persist.
type spyPersister struct {
	calls int
	last  jobs.PersistInput

	batchCalls      int
	batchHandle     string
	batchCoveredSeq int64
}

func (p *spyPersister) Persist(_ context.Context, in jobs.PersistInput) error {
	p.calls++
	p.last = in
	return nil
}

func (p *spyPersister) SetRunBatch(_ context.Context, _, _ pgtype.UUID, handle string, coveredSeq int64) error {
	p.batchCalls++
	p.batchHandle = handle
	p.batchCoveredSeq = coveredSeq
	return nil
}

// spyBatchExtractor implements both ext.Extractor and ext.BatchExtractor so a test can drive the
// economy submit/collect phases. CollectBatch returns done=false until doneAfter calls have elapsed,
// so a test can exercise the poll-then-complete path.
type spyBatchExtractor struct {
	result ext.ExtractResult

	submitCalls int
	submitGot   ext.ExtractInput
	handle      string
	submitErr   error

	collectCalls int
	collectGot   []string
	doneAfter    int
	collectErr   error
}

func (s *spyBatchExtractor) Extract(_ context.Context, in ext.ExtractInput) (ext.ExtractResult, error) {
	s.submitGot = in // records the window on the synchronous (economy-fallback) path too
	return s.result, nil
}

func (s *spyBatchExtractor) SubmitBatch(_ context.Context, in ext.ExtractInput) (string, error) {
	s.submitCalls++
	s.submitGot = in
	if s.submitErr != nil {
		return "", s.submitErr
	}
	return s.handle, nil
}

func (s *spyBatchExtractor) CollectBatch(_ context.Context, handle string) (ext.ExtractResult, bool, error) {
	s.collectCalls++
	s.collectGot = append(s.collectGot, handle)
	if s.collectErr != nil {
		return ext.ExtractResult{}, false, s.collectErr
	}
	if s.collectCalls <= s.doneAfter {
		return ext.ExtractResult{}, false, nil
	}
	return s.result, true, nil
}

// ready is a readiness that always processes (idle far past any window).
func ready(count int64) db.RunExtractionReadinessRow {
	return db.RunExtractionReadinessRow{EventCount: count, IdleSeconds: 3600}
}

// pgUUID builds a pgtype.UUID from a canonical string, for stamping event ids in fakes.
func pgUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	u, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("parse uuid %q: %v", s, err)
	}
	return pgtype.UUID{Bytes: u, Valid: true}
}

// TestExtractRunWorker_BoundsTheWindowAndDrainsTheRest pins the cap that keeps a burst from stalling a run
// forever.
//
// The failure it prevents is not "a slow pass" — it is permanent. An extractor's output ceiling is finite, so
// a large enough window truncates the model's response; the pass errors, and because the window is rebuilt
// from the same events past the same checkpoint, every retry fails identically until the job is discarded with
// the checkpoint frozen. The run stops distilling for good, and (before the job-error reporting landed) said
// nothing. Measured for real: a client writing one event per conversational turn produced a 511-event window
// that hit the ceiling on all three attempts.
//
// So this asserts the split, not just the limit argument: with 500 pending and a window of 200, the extractor
// must see exactly the first 200, the checkpoint must advance only that far, and the worker must SNOOZE rather
// than report success — a snooze is what returns for the rest without spending an attempt. Asserting only that
// WindowLimit was passed would pass against a worker that then extracted everything anyway.
func TestExtractRunWorker_BoundsTheWindowAndDrainsTheRest(t *testing.T) {
	const pending, window = 500, 200
	events := make([]db.Event, 0, pending)
	for i := 1; i <= pending; i++ {
		events = append(events, db.Event{Seq: int64(i), AgentID: "a", Payload: []byte(`{"memory":"keep"}`)})
	}
	src := &fakeSource{
		events:    events,
		readiness: ready(pending),
		// After the pass, the events beyond the window are still pending — this is what the real
		// readiness query would report, and what makes the worker drain instead of finishing.
		tailReadiness: ready(pending - window),
	}
	spy := &spyExtractor{}
	per := &spyPersister{}
	d := jobs.DefaultDebounce()
	d.MaxWindow = window
	w := jobs.NewExtractRunWorker(src, spy, per, d)

	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}
	err := w.Work(context.Background(), job)

	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) {
		t.Fatalf("Work = %v, want a snooze — the remaining %d events must come back without burning an attempt",
			err, pending-window)
	}
	if src.gotArg.WindowLimit != int32(window) {
		t.Errorf("WindowLimit = %d, want %d", src.gotArg.WindowLimit, window)
	}
	if len(spy.got.Events) != window {
		t.Fatalf("extraction window = %d events, want %d — an unbounded pass is what truncates and stalls",
			len(spy.got.Events), window)
	}
	if spy.got.Events[0].Seq != 1 || spy.got.Events[window-1].Seq != window {
		t.Errorf("window spans seq %d..%d, want 1..%d (the oldest events first, in order)",
			spy.got.Events[0].Seq, spy.got.Events[window-1].Seq, window)
	}
	// The checkpoint may only advance over what was actually distilled, or the untouched remainder is
	// skipped rather than drained.
	if per.last.CoveredSeq != int64(window) {
		t.Errorf("covered_seq = %d, want %d — advancing past undistilled events loses them silently",
			per.last.CoveredSeq, window)
	}
}

// truncatingExtractor truncates every window larger than fitsAt and succeeds at or below it, recording the
// size of each window it was handed. Recording the sizes is the point: the shrink schedule is the
// behaviour under test, and asserting only the final outcome would pass against a worker that gave up and
// re-sent the same window forever.
type truncatingExtractor struct {
	fitsAt int
	sizes  []int
	err    error // when set, returned instead of a truncation — a failure the window cannot fix
}

func (e *truncatingExtractor) Extract(_ context.Context, in ext.ExtractInput) (ext.ExtractResult, error) {
	e.sizes = append(e.sizes, len(in.Events))
	if e.err != nil {
		return ext.ExtractResult{}, e.err
	}
	if len(in.Events) > e.fitsAt {
		return ext.ExtractResult{}, fmt.Errorf("%w: max_tokens=1", ext.ErrResponseTruncated)
	}
	return ext.ExtractResult{}, nil
}

func seqEvents(n int) []db.Event {
	events := make([]db.Event, 0, n)
	for i := 1; i <= n; i++ {
		events = append(events, db.Event{Seq: int64(i), AgentID: "a", Payload: []byte(`{"memory":"keep"}`)})
	}
	return events
}

// TestExtractRunWorker_ShrinksTheWindowOnTruncationAndKeepsProgress is the regression lock for a
// permanent stall.
//
// A truncated response is the one extractor failure a retry cannot fix: events are append-only and the
// window is rebuilt from the same checkpoint, so every attempt sends byte-identical input and truncates
// identically until River discards the job — leaving the run's checkpoint frozen and its distillation
// stopped for good. Capping the window did not remove that, it only moved it: a real workload was measured
// truncating at the 200-event cap, three identical attempts, job discarded, 470 of 514 events never
// distilled.
//
// Two assertions carry the fix. The shrink schedule proves the input actually changed between attempts —
// the mutant that returns the error instead of halving, or that re-sends the same size, dies here. And
// covered_seq proves the shorter pass still moved the checkpoint: partial progress must be kept, or a dense
// run makes no headway at all. The snooze is what brings the remainder back without spending an attempt.
func TestExtractRunWorker_ShrinksTheWindowOnTruncationAndKeepsProgress(t *testing.T) {
	const window, fitsAt = 200, 60
	src := &fakeSource{
		events:        seqEvents(window),
		readiness:     ready(window),
		tailReadiness: ready(window - 50), // the events the shrunk pass did not cover are still pending
	}
	spy := &truncatingExtractor{fitsAt: fitsAt}
	per := &spyPersister{}
	d := jobs.DefaultDebounce()
	d.MaxWindow = window
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	w := jobs.NewExtractRunWorker(src, spy, per, d, jobs.WithExtractMetrics(m))

	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}
	err := w.Work(context.Background(), job)

	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) {
		t.Fatalf("Work = %v, want a snooze so the undistilled remainder returns without burning an attempt", err)
	}
	// 200 truncates, 100 truncates, 50 fits (fitsAt=60). Each step must be strictly smaller, or the retry
	// is the byte-identical one that caused the stall.
	want := []int{200, 100, 50}
	if len(spy.sizes) != len(want) {
		t.Fatalf("extractor saw %v, want %v — the window must shrink between attempts", spy.sizes, want)
	}
	for i, n := range want {
		if spy.sizes[i] != n {
			t.Fatalf("extractor saw %v, want %v", spy.sizes, want)
		}
	}
	if per.calls != 1 {
		t.Fatalf("persisted %d times, want 1 — the pass that fit must commit", per.calls)
	}
	if per.last.CoveredSeq != 50 {
		t.Errorf("covered_seq = %d, want 50 — the checkpoint must advance over exactly the events that were "+
			"distilled: less loses the pass's progress, more skips events that never reached the model",
			per.last.CoveredSeq)
	}
	// The counters must describe what the pass committed, not what it read. The 150 events it shrank away
	// are still past the checkpoint and will be read again by the next pass, so counting the read here
	// would count them twice and report 200 events reaching a model that only saw 50.
	if got := counterValue(t, reg, "lore_extract_events_extracted_total", nil); got != 50 {
		t.Errorf("extract_events_extracted = %v, want 50 — reporting the read window double-counts the "+
			"events the next pass will read again", got)
	}
	if got := counterValue(t, reg, "lore_extract_events_ingested_total", nil); got != 50 {
		t.Errorf("extract_events_ingested = %v, want 50 (the events this pass covered)", got)
	}
	if got := counterValue(t, reg, "lore_extract_window_shrink_total", map[string]string{"outcome": "retried"}); got != 2 {
		t.Errorf("window_shrink{retried} = %v, want 2 — one per halving, so the rate is the operator's "+
			"signal that the window and ceiling are mismatched", got)
	}
	if got := counterValue(t, reg, "lore_extract_window_shrink_total", map[string]string{"outcome": "exhausted"}); got != 0 {
		t.Errorf("window_shrink{exhausted} = %v, want 0 — the pass recovered, so no run stopped", got)
	}
}

// counterValue reads one counter out of a registry, optionally matching labels. Gathering rather than
// reaching into the collector keeps this to the dependencies the repo already has, and matches how the
// other metric assertions here read the registry. A metric that was never incremented reports 0 rather
// than failing, so a test can assert "this did not happen".
func counterValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			match := true
			for _, lp := range m.GetLabel() {
				if want, ok := labels[lp.GetName()]; ok && want != lp.GetValue() {
					match = false
				}
			}
			if match && len(m.GetLabel()) >= len(labels) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// TestExtractRunWorker_TruncationAtTheFloorCancelsInsteadOfRetrying covers the end of the schedule.
//
// Halving has to stop somewhere, and when it does the answer must be permanent: a plain error would send
// the job back to River, which would re-run it from the top and re-walk the identical schedule on every
// attempt before discarding it — the same wasted attempts the fix exists to remove, just more expensive.
// So this asserts a JobCancel (no further attempts) and a bounded, strictly-decreasing schedule that
// actually reaches the floor. A missing floor shows up here as a hang, not a failure, which is why the
// schedule is asserted rather than just the error.
func TestExtractRunWorker_TruncationAtTheFloorCancelsInsteadOfRetrying(t *testing.T) {
	const window = 200
	src := &fakeSource{events: seqEvents(window), readiness: ready(window)}
	spy := &truncatingExtractor{fitsAt: 0} // nothing ever fits
	per := &spyPersister{}
	d := jobs.DefaultDebounce()
	d.MaxWindow = window
	w := jobs.NewExtractRunWorker(src, spy, per, d)

	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}
	err := w.Work(context.Background(), job)

	var cancel *river.JobCancelError
	if !errors.As(err, &cancel) {
		t.Fatalf("Work = %T %v, want a JobCancelError — more attempts would send the same events to the same ceiling", err, err)
	}
	if !errors.Is(err, ext.ErrResponseTruncated) {
		t.Errorf("Work = %v, want the truncation cause preserved so the log says why the run stopped", err)
	}
	want := []int{200, 100, 50, 25, 12, 8}
	if len(spy.sizes) != len(want) {
		t.Fatalf("extractor saw %v, want %v — halving must reach the floor and stop there", spy.sizes, want)
	}
	for i, n := range want {
		if spy.sizes[i] != n {
			t.Fatalf("extractor saw %v, want %v", spy.sizes, want)
		}
	}
	if per.calls != 0 {
		t.Errorf("persisted %d times, want 0 — nothing was distilled, so the checkpoint must not move", per.calls)
	}
}

// TestExtractRunWorker_ProviderFailureDoesNotShrinkTheWindow is the other half of the taxonomy.
//
// Shrinking is the right answer to truncation and the wrong one to an outage: the window is fine, the
// provider is not, and waiting fixes it. A worker that shrank on any error would keep a shorter window
// after a blip and — worse — could commit a partial prefix and advance the checkpoint on what is really a
// transient failure. Exactly one call proves the error went straight back to River's backoff.
func TestExtractRunWorker_ProviderFailureDoesNotShrinkTheWindow(t *testing.T) {
	const window = 200
	src := &fakeSource{events: seqEvents(window), readiness: ready(window)}
	spy := &truncatingExtractor{err: ext.ErrExtractorUnavailable}
	per := &spyPersister{}
	d := jobs.DefaultDebounce()
	d.MaxWindow = window
	w := jobs.NewExtractRunWorker(src, spy, per, d)

	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}
	err := w.Work(context.Background(), job)

	if !errors.Is(err, ext.ErrExtractorUnavailable) {
		t.Fatalf("Work = %v, want ErrExtractorUnavailable passed through for River to retry", err)
	}
	var cancel *river.JobCancelError
	if errors.As(err, &cancel) {
		t.Error("a provider outage was cancelled; it is retryable and must not end the run's distillation")
	}
	if len(spy.sizes) != 1 {
		t.Fatalf("extractor called %d times with %v, want exactly 1 — an outage is not fixed by a smaller window",
			len(spy.sizes), spy.sizes)
	}
	if per.calls != 0 {
		t.Errorf("persisted %d times, want 0 — a failed pass must not advance the checkpoint", per.calls)
	}
}

// TestExtractRunWorker_UnsetWindowDoesNotReadNothing guards the zero value. MaxWindow reaches the query as a
// LIMIT, so an unset one would be LIMIT 0 — a worker that distils nothing at all, quietly. A caller who has
// simply never heard of the field must get the production cap, not silence.
func TestExtractRunWorker_UnsetWindowDoesNotReadNothing(t *testing.T) {
	src := &fakeSource{
		events:    []db.Event{{Seq: 1, AgentID: "a", Payload: []byte(`{"memory":"keep"}`)}},
		readiness: ready(1),
	}
	spy := &spyExtractor{}
	w := jobs.NewExtractRunWorker(src, spy, &spyPersister{}, jobs.Debounce{IdleWindow: 0, MaxEvents: 1})

	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if src.gotArg.WindowLimit <= 0 {
		t.Fatalf("WindowLimit = %d; an unset MaxWindow must not become LIMIT 0", src.gotArg.WindowLimit)
	}
	if len(spy.got.Events) != 1 {
		t.Errorf("extracted %d events, want 1 — a zero window silently distils nothing", len(spy.got.Events))
	}
}

func TestExtractRunWorker_GatesThenExtracts(t *testing.T) {
	proj := uuid.NewString()
	run := uuid.NewString()
	src := &fakeSource{
		events: []db.Event{
			{Seq: 1, AgentID: "a", Payload: []byte(`{"memory":"keep"}`)},
			{Seq: 2, AgentID: "a", Payload: []byte(`{"kind":"tool_log","data":"noise"}`)}, // gated out
			{Seq: 3, AgentID: "b", Payload: []byte(`{"claim":{"entity":"e","predicate":"p","value":1}}`)},
			{Seq: 4, AgentID: "a", Payload: []byte(`{"kind":"tool_log","data":"tail"}`)}, // gated, and the highest seq
		},
		readiness: ready(4),
	}
	spy := &spyExtractor{}
	per := &spyPersister{}
	w := jobs.NewExtractRunWorker(src, spy, per, jobs.DefaultDebounce())

	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: proj, RunID: run}}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work: %v", err)
	}

	if spy.calls != 1 {
		t.Fatalf("extractor calls = %d, want 1 (a single coalesced pass)", spy.calls)
	}
	// The gated tool_log (seq 2) is excluded; only seq 1 and 3 reach the extractor, in order.
	if len(spy.got.Events) != 2 {
		t.Fatalf("extraction window = %d events, want 2 (tool_log gated out)", len(spy.got.Events))
	}
	if spy.got.Events[0].Seq != 1 || spy.got.Events[1].Seq != 3 {
		t.Errorf("window seqs = [%d,%d], want [1,3]", spy.got.Events[0].Seq, spy.got.Events[1].Seq)
	}
	// All three mapped fields carry through, not just Seq: agent provenance and the raw payload.
	if spy.got.Events[0].AgentID != "a" || spy.got.Events[1].AgentID != "b" {
		t.Errorf("window agents = [%q,%q], want [a,b]", spy.got.Events[0].AgentID, spy.got.Events[1].AgentID)
	}
	if string(spy.got.Events[1].Payload) != `{"claim":{"entity":"e","predicate":"p","value":1}}` {
		t.Errorf("window[1] payload = %s, want the claim JSON round-tripped", spy.got.Events[1].Payload)
	}
	if spy.got.ProjectID != proj || spy.got.RunID != run {
		t.Errorf("extract input identity = {%s,%s}, want {%s,%s}", spy.got.ProjectID, spy.got.RunID, proj, run)
	}
	// The worker scoped the read to the job's project and run (a valid UUID pair reached the source).
	if !src.gotArg.ProjectID.Valid || !src.gotArg.RunID.Valid {
		t.Error("source was not called with a valid project_id/run_id")
	}
	// The checkpoint advances to the highest seq READ — the trailing gated seq 4 included, even
	// though the highest EXTRACTED event is seq 3 — so archived chatter at the tail is never re-read.
	if per.calls != 1 {
		t.Fatalf("persister calls = %d, want 1", per.calls)
	}
	if per.last.CoveredSeq != 4 {
		t.Errorf("covered_seq = %d, want 4 (highest read, past the trailing gated event; not the ungated-window max of 3)", per.last.CoveredSeq)
	}
}

func TestExtractRunWorker_Debounces(t *testing.T) {
	debounce := jobs.Debounce{IdleWindow: 2 * time.Second, MaxEvents: 20}
	cases := []struct {
		name       string
		readiness  db.RunExtractionReadinessRow
		wantSnooze bool
	}{
		{"still accumulating (not idle, under cap) -> snooze", db.RunExtractionReadinessRow{EventCount: 3, IdleSeconds: 0.5}, true},
		{"just under the idle window -> snooze", db.RunExtractionReadinessRow{EventCount: 3, IdleSeconds: 1.999}, true},
		{"idle exactly at the window -> process", db.RunExtractionReadinessRow{EventCount: 3, IdleSeconds: 2.0}, false},
		{"idle past the window -> process", db.RunExtractionReadinessRow{EventCount: 3, IdleSeconds: 2.5}, false},
		{"one under the event cap -> snooze", db.RunExtractionReadinessRow{EventCount: 19, IdleSeconds: 0.1}, true},
		{"event cap reached -> process", db.RunExtractionReadinessRow{EventCount: 20, IdleSeconds: 0.1}, false},
		{"empty run -> complete (no snooze, no work)", db.RunExtractionReadinessRow{EventCount: 0, IdleSeconds: 0}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeSource{
				readiness: tc.readiness,
				// A single extractable event so a processing case runs the whole path; the drained
				// case (EventCount 0) returns before it is ever read.
				events: []db.Event{{Seq: 1, AgentID: "a", Payload: []byte(`{"memory":"x"}`)}},
			}
			spy := &spyExtractor{}
			per := &spyPersister{}
			w := jobs.NewExtractRunWorker(src, spy, per, debounce)
			job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}

			err := w.Work(context.Background(), job)
			var snooze *river.JobSnoozeError
			gotSnooze := errors.As(err, &snooze)
			if gotSnooze != tc.wantSnooze {
				t.Fatalf("snoozed = %v (err %v), want %v", gotSnooze, err, tc.wantSnooze)
			}
			if tc.wantSnooze {
				if snooze.Duration != debounce.IdleWindow {
					t.Errorf("snooze duration = %v, want %v", snooze.Duration, debounce.IdleWindow)
				}
				if spy.calls != 0 {
					t.Error("a snoozed pass must not call the extractor")
				}
				if per.calls != 0 {
					t.Error("a snoozed pass must not persist")
				}
				return
			}
			if err != nil {
				t.Fatalf("process path err = %v, want nil", err)
			}
			if tc.readiness.EventCount == 0 {
				// Drained: nothing past the checkpoint, so neither the model nor the store is touched.
				if spy.calls != 0 || per.calls != 0 {
					t.Errorf("drained run should do no work: extractor=%d persister=%d, want 0/0", spy.calls, per.calls)
				}
				return
			}
			if spy.calls != 1 {
				t.Errorf("extractor calls = %d, want 1 (processed)", spy.calls)
			}
			if per.calls != 1 {
				t.Errorf("persister calls = %d, want 1 (processed)", per.calls)
			}
		})
	}
}

// TestExtractRunWorker_PersistsWithProvenance proves the worker resolves each candidate's provenance
// from the event it was distilled from (source event id + agent) and advances the checkpoint to the
// highest seq read.
func TestExtractRunWorker_PersistsWithProvenance(t *testing.T) {
	srcEventID := pgUUID(t, "11111111-1111-1111-1111-111111111111")
	src := &fakeSource{
		events:    []db.Event{{ID: srcEventID, Seq: 5, AgentID: "planner", Payload: []byte(`{"memory":"deploy done"}`)}},
		readiness: ready(1),
	}
	spy := &spyExtractor{result: ext.ExtractResult{
		Memories: []ext.CandidateMemory{{Kind: "semantic", Content: "deploy done", SourceSeq: 5}},
	}}
	per := &spyPersister{}
	w := jobs.NewExtractRunWorker(src, spy, per, jobs.DefaultDebounce())
	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}

	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if per.calls != 1 {
		t.Fatalf("persister calls = %d, want 1", per.calls)
	}
	if per.last.CoveredSeq != 5 {
		t.Errorf("covered_seq = %d, want 5", per.last.CoveredSeq)
	}
	if len(per.last.Memories) != 1 {
		t.Fatalf("persisted memories = %d, want 1", len(per.last.Memories))
	}
	m := per.last.Memories[0]
	if m.Kind != "semantic" || m.Content != "deploy done" {
		t.Errorf("memory = {%q,%q}, want {semantic, deploy done}", m.Kind, m.Content)
	}
	if m.CreatedByAgent != "planner" {
		t.Errorf("created_by_agent = %q, want planner (from the source event)", m.CreatedByAgent)
	}
	if m.SourceEventID != srcEventID {
		t.Errorf("source_event_id = %v, want the source event's id %v", m.SourceEventID, srcEventID)
	}
}

// TestExtractRunWorker_PersistsClaimsAndEntities proves the worker forwards entities as-is and
// resolves each claim's provenance (source event) from its seq, drops a claim naming a seq outside
// the window, and sorts claims by SourceSeq so the persister applies last-write-wins deterministically.
func TestExtractRunWorker_PersistsClaimsAndEntities(t *testing.T) {
	ev1ID := pgUUID(t, "aaaaaaaa-0000-0000-0000-000000000001")
	ev2ID := pgUUID(t, "aaaaaaaa-0000-0000-0000-000000000002")
	src := &fakeSource{
		events: []db.Event{
			{ID: ev1ID, Seq: 1, AgentID: "planner", Payload: []byte(`{"memory":"m"}`)},
			{ID: ev2ID, Seq: 2, AgentID: "tester", Payload: []byte(`{"claim":{}}`)},
		},
		readiness: ready(2),
	}
	spy := &spyExtractor{result: ext.ExtractResult{
		Memories: []ext.CandidateMemory{{Kind: "semantic", Content: "m", SourceSeq: 1}},
		Entities: []ext.EntityMention{{Name: "auth", Type: "service", Aliases: []string{"auth-svc"}}},
		Claims: []ext.CandidateClaim{
			// Out of seq order on purpose; the worker must sort them to seq [1,2] before persisting.
			{Entity: "auth", Predicate: "status", Value: []byte(`"up"`), SourceSeq: 2},
			{Entity: "auth", Predicate: "status", Value: []byte(`"down"`), SourceSeq: 1},
			// Names a seq outside the window -> dropped.
			{Entity: "ghost", Predicate: "x", Value: []byte(`1`), SourceSeq: 999},
			// No value (malformed) -> dropped rather than crash the NOT NULL jsonb insert.
			{Entity: "novalue", Predicate: "p", Value: nil, SourceSeq: 1},
		},
	}}
	per := &spyPersister{}
	w := jobs.NewExtractRunWorker(src, spy, per, jobs.DefaultDebounce())
	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}

	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if per.calls != 1 {
		t.Fatalf("persister calls = %d, want 1", per.calls)
	}
	in := per.last

	// Entities forwarded as-is.
	if len(in.Entities) != 1 || in.Entities[0].Name != "auth" || in.Entities[0].Type != "service" {
		t.Fatalf("entities = %+v, want one {auth, service}", in.Entities)
	}
	if len(in.Entities[0].Aliases) != 1 || in.Entities[0].Aliases[0] != "auth-svc" {
		t.Errorf("entity aliases = %v, want [auth-svc]", in.Entities[0].Aliases)
	}

	// Memory carries its SourceSeq so the persister can link a same-event claim to it.
	if len(in.Memories) != 1 || in.Memories[0].SourceSeq != 1 {
		t.Fatalf("memories = %+v, want one with SourceSeq 1", in.Memories)
	}

	// The out-of-window and valueless claims are dropped; the remaining two are sorted by SourceSeq
	// (1 then 2) with provenance resolved to the source event.
	if len(in.Claims) != 2 {
		t.Fatalf("claims = %d, want 2 (the seq-999 and no-value claims are dropped)", len(in.Claims))
	}
	if in.Claims[0].SourceSeq != 1 || in.Claims[1].SourceSeq != 2 {
		t.Errorf("claim order = [%d,%d], want [1,2] (sorted for LWW)", in.Claims[0].SourceSeq, in.Claims[1].SourceSeq)
	}
	if string(in.Claims[0].Value) != `"down"` || string(in.Claims[1].Value) != `"up"` {
		t.Errorf("claim values = [%s,%s], want [\"down\",\"up\"] in seq order", in.Claims[0].Value, in.Claims[1].Value)
	}
	if in.Claims[0].SourceEventID != ev1ID || in.Claims[1].SourceEventID != ev2ID {
		t.Errorf("claim provenance = [%v,%v], want [event1, event2]", in.Claims[0].SourceEventID, in.Claims[1].SourceEventID)
	}
}

// TestExtractRunWorker_ClaimValueGuard proves the value guard's exact boundary: a claim with no value
// or a non-well-formed JSON value is dropped (so it cannot abort the NOT NULL jsonb insert and strand
// the coalesced pass), while a JSON `null` literal — non-empty and valid — is kept.
func TestExtractRunWorker_ClaimValueGuard(t *testing.T) {
	ev := pgUUID(t, "bbbbbbbb-0000-0000-0000-000000000001")
	src := &fakeSource{
		events:    []db.Event{{ID: ev, Seq: 1, AgentID: "a", Payload: []byte(`{}`)}},
		readiness: ready(1),
	}
	spy := &spyExtractor{result: ext.ExtractResult{
		Claims: []ext.CandidateClaim{
			{Entity: "e", Predicate: "empty", Value: nil, SourceSeq: 1},             // dropped: no value
			{Entity: "e", Predicate: "bad", Value: []byte(`up`), SourceSeq: 1},      // dropped: invalid JSON
			{Entity: "e", Predicate: "nullok", Value: []byte(`null`), SourceSeq: 1}, // kept: JSON null is valid
		},
	}}
	per := &spyPersister{}
	w := jobs.NewExtractRunWorker(src, spy, per, jobs.DefaultDebounce())
	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}

	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if per.calls != 1 {
		t.Fatalf("persister calls = %d, want 1", per.calls)
	}
	if len(per.last.Claims) != 1 {
		t.Fatalf("persisted claims = %d, want 1 (only the JSON-null claim survives)", len(per.last.Claims))
	}
	if per.last.Claims[0].Predicate != "nullok" || string(per.last.Claims[0].Value) != "null" {
		t.Errorf("kept claim = {%q, %s}, want {nullok, null}", per.last.Claims[0].Predicate, per.last.Claims[0].Value)
	}
}

// TestExtractRunWorker_DropsOutOfWindowCandidate proves a candidate naming a seq outside the window
// (a misbehaving extractor) is dropped rather than stored without provenance — but the checkpoint
// still advances, so the pass does not loop on the same events.
func TestExtractRunWorker_DropsOutOfWindowCandidate(t *testing.T) {
	src := &fakeSource{
		events:    []db.Event{{ID: pgUUID(t, "22222222-2222-2222-2222-222222222222"), Seq: 1, AgentID: "a", Payload: []byte(`{"memory":"x"}`)}},
		readiness: ready(1),
	}
	spy := &spyExtractor{result: ext.ExtractResult{
		Memories: []ext.CandidateMemory{{Kind: "semantic", Content: "ghost", SourceSeq: 999}},
	}}
	per := &spyPersister{}
	w := jobs.NewExtractRunWorker(src, spy, per, jobs.DefaultDebounce())
	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}

	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if per.calls != 1 {
		t.Fatalf("persister calls = %d, want 1 (the checkpoint must still advance)", per.calls)
	}
	if len(per.last.Memories) != 0 {
		t.Errorf("out-of-window candidate must be dropped, got %d memories", len(per.last.Memories))
	}
	if per.last.CoveredSeq != 1 {
		t.Errorf("covered_seq = %d, want 1 (advances even when the candidate is dropped)", per.last.CoveredSeq)
	}
}

// TestExtractRunWorker_DrainsTail proves that when events arrive while a pass runs (readiness still
// shows work past the just-advanced checkpoint), the worker snoozes to drain them rather than
// completing — but only after processing and persisting the current window once.
func TestExtractRunWorker_DrainsTail(t *testing.T) {
	src := &fakeSource{
		events:        []db.Event{{ID: pgUUID(t, "33333333-3333-3333-3333-333333333333"), Seq: 1, AgentID: "a", Payload: []byte(`{"memory":"x"}`)}},
		readiness:     db.RunExtractionReadinessRow{EventCount: 20, IdleSeconds: 0.1}, // cap reached -> process now
		tailReadiness: db.RunExtractionReadinessRow{EventCount: 3, IdleSeconds: 0.1},  // arrived during the pass
	}
	spy := &spyExtractor{}
	per := &spyPersister{}
	w := jobs.NewExtractRunWorker(src, spy, per, jobs.DefaultDebounce())
	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}

	err := w.Work(context.Background(), job)
	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) {
		t.Fatalf("want a tail-drain snooze, got err %v", err)
	}
	if snooze.Duration != jobs.DefaultDebounce().IdleWindow {
		t.Errorf("snooze duration = %v, want %v", snooze.Duration, jobs.DefaultDebounce().IdleWindow)
	}
	if spy.calls != 1 {
		t.Errorf("the pass still processed once before snoozing; extractor calls=%d want 1", spy.calls)
	}
	if per.calls != 1 {
		t.Errorf("the pass persisted once before snoozing; persister calls=%d want 1", per.calls)
	}
}

func TestExtractRunWorker_ExtractorErrorPropagates(t *testing.T) {
	// A fixture_error event makes the real FixtureExtractor fail; the worker must surface the error
	// so River retries the pass rather than dropping it — and must NOT persist or advance the
	// checkpoint, so the events are reprocessed on the retry.
	src := &fakeSource{
		events:    []db.Event{{Seq: 1, AgentID: "a", Payload: []byte(`{"fixture_error":"unavailable"}`)}},
		readiness: ready(1),
	}
	per := &spyPersister{}
	w := jobs.NewExtractRunWorker(src, ext.FixtureExtractor{}, per, jobs.DefaultDebounce())
	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}
	if err := w.Work(context.Background(), job); err == nil {
		t.Error("Work should surface the extractor error so the job retries")
	}
	if per.calls != 0 {
		t.Error("a failed extraction must not persist or advance the checkpoint")
	}
}

func TestExtractRunWorker_BadUUIDFails(t *testing.T) {
	w := jobs.NewExtractRunWorker(&fakeSource{}, &spyExtractor{}, &spyPersister{}, jobs.DefaultDebounce())
	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: "not-a-uuid", RunID: uuid.NewString()}}
	if err := w.Work(context.Background(), job); err == nil {
		t.Error("Work with a malformed project_id should error")
	}
}

func economyState() db.GetRunExtractionStateRow {
	return db.GetRunExtractionStateRow{ExtractionMode: "economy"}
}

func pendingBatchState(handle string, coveredSeq int64) db.GetRunExtractionStateRow {
	return db.GetRunExtractionStateRow{ExtractionBatchID: &handle, ExtractionBatchCoveredSeq: &coveredSeq}
}

func TestExtractRunWorker_EconomySubmitsBatch(t *testing.T) {
	src := &fakeSource{
		events: []db.Event{
			{Seq: 1, AgentID: "a", Payload: []byte(`{"memory":"keep"}`)},
			{Seq: 2, AgentID: "a", Payload: []byte(`{"kind":"tool_log"}`)}, // gated, and the highest seq read
		},
		readiness: ready(2),
		state:     economyState(),
	}
	batch := &spyBatchExtractor{handle: "batch_1"}
	per := &spyPersister{}
	w := jobs.NewExtractRunWorker(src, batch, per, jobs.DefaultDebounce())
	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}

	err := w.Work(context.Background(), job)
	// Economy submit defers collection: the pass snoozes to poll and must not persist yet.
	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) {
		t.Fatalf("economy submit should snooze to poll, got err %v", err)
	}
	if snooze.Duration != jobs.DefaultDebounce().BatchPoll {
		t.Errorf("submit snooze = %v, want the batch poll interval %v", snooze.Duration, jobs.DefaultDebounce().BatchPoll)
	}
	if batch.submitCalls != 1 {
		t.Fatalf("SubmitBatch calls = %d, want 1", batch.submitCalls)
	}
	// Only the ungated event reaches the batch; the checkpoint recorded is the highest seq READ (2).
	if len(batch.submitGot.Events) != 1 || batch.submitGot.Events[0].Seq != 1 {
		t.Errorf("submitted window = %+v, want just seq 1 (tool_log gated)", batch.submitGot.Events)
	}
	if per.batchCalls != 1 || per.batchHandle != "batch_1" || per.batchCoveredSeq != 2 {
		t.Errorf("SetRunBatch = {calls:%d handle:%q covered:%d}, want {1 batch_1 2}", per.batchCalls, per.batchHandle, per.batchCoveredSeq)
	}
	if per.calls != 0 {
		t.Error("economy submit must not persist yet (collection happens later)")
	}
	if batch.collectCalls != 0 {
		t.Error("the submit phase must not collect")
	}
}

func TestExtractRunWorker_EconomyAllGatedAdvancesCheckpoint(t *testing.T) {
	src := &fakeSource{
		events: []db.Event{
			{Seq: 1, AgentID: "a", Payload: []byte(`{"kind":"tool_log"}`)},
			{Seq: 2, AgentID: "a", Payload: []byte(`{"kind":"tool_log"}`)},
		},
		readiness: ready(2),
		state:     economyState(),
	}
	batch := &spyBatchExtractor{handle: "unused"}
	per := &spyPersister{}
	w := jobs.NewExtractRunWorker(src, batch, per, jobs.DefaultDebounce())
	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work: %v", err)
	}
	// An all-gated window has nothing to submit; it advances the checkpoint synchronously instead.
	if batch.submitCalls != 0 || per.batchCalls != 0 {
		t.Errorf("all-gated economy window must not submit a batch (submit=%d setBatch=%d)", batch.submitCalls, per.batchCalls)
	}
	if per.calls != 1 || per.last.CoveredSeq != 2 {
		t.Errorf("checkpoint should advance to 2 synchronously; persist calls=%d covered=%d", per.calls, per.last.CoveredSeq)
	}
	if len(per.last.Memories) != 0 {
		t.Errorf("an all-gated window yields no memories, got %d", len(per.last.Memories))
	}
}

func TestExtractRunWorker_EconomyFallsBackWithoutBatchCapability(t *testing.T) {
	src := &fakeSource{
		events:    []db.Event{{Seq: 1, AgentID: "a", Payload: []byte(`{"memory":"keep"}`)}},
		readiness: ready(1),
		state:     economyState(),
	}
	spy := &spyExtractor{} // a plain Extractor, NOT batch-capable
	per := &spyPersister{}
	w := jobs.NewExtractRunWorker(src, spy, per, jobs.DefaultDebounce())
	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work: %v", err)
	}
	// Economy mode with a non-batch extractor falls back to a synchronous pass rather than stalling.
	if spy.calls != 1 {
		t.Errorf("expected a synchronous Extract fallback, extractor calls=%d", spy.calls)
	}
	if per.calls != 1 || per.last.CoveredSeq != 1 {
		t.Errorf("the fallback should persist + advance; persist calls=%d covered=%d", per.calls, per.last.CoveredSeq)
	}
	if per.batchCalls != 0 {
		t.Error("a non-batch extractor cannot submit a batch")
	}
}

func TestExtractRunWorker_CollectSnoozesUntilDone(t *testing.T) {
	src := &fakeSource{state: pendingBatchState("batch_9", 5)}
	batch := &spyBatchExtractor{doneAfter: 1} // the first collect returns not-done
	per := &spyPersister{}
	w := jobs.NewExtractRunWorker(src, batch, per, jobs.DefaultDebounce())
	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}

	err := w.Work(context.Background(), job)
	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) {
		t.Fatalf("a not-ready batch should snooze to poll, got err %v", err)
	}
	if snooze.Duration != jobs.DefaultDebounce().BatchPoll {
		t.Errorf("poll snooze = %v, want %v", snooze.Duration, jobs.DefaultDebounce().BatchPoll)
	}
	if batch.collectCalls != 1 || len(batch.collectGot) != 1 || batch.collectGot[0] != "batch_9" {
		t.Errorf("CollectBatch should run once with the pending handle, got calls=%d got=%v", batch.collectCalls, batch.collectGot)
	}
	if batch.submitCalls != 0 {
		t.Error("the collect phase must not submit")
	}
	if per.calls != 0 {
		t.Error("a not-ready batch must not persist")
	}
}

func TestExtractRunWorker_CollectPersistsWhenDone(t *testing.T) {
	src := &fakeSource{
		// Re-read for provenance: the events the batch covered (seq <= 2). The zero-value readiness
		// leaves the tail drained, so the pass completes.
		events: []db.Event{
			{Seq: 1, AgentID: "planner", Payload: []byte(`{"memory":"x"}`)},
			{Seq: 2, AgentID: "coder", Payload: []byte(`{"memory":"y"}`)},
		},
		state: pendingBatchState("batch_9", 2),
	}
	batch := &spyBatchExtractor{
		doneAfter: 0, // immediately ready
		result: ext.ExtractResult{Memories: []ext.CandidateMemory{
			{Kind: "semantic", Content: "distilled", SourceSeq: 1},
		}},
	}
	per := &spyPersister{}
	w := jobs.NewExtractRunWorker(src, batch, per, jobs.DefaultDebounce())
	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if batch.collectCalls != 1 {
		t.Fatalf("CollectBatch calls = %d, want 1", batch.collectCalls)
	}
	if per.calls != 1 {
		t.Fatalf("a collected batch should persist once, persist calls=%d", per.calls)
	}
	// The checkpoint advances to the submit-time seq the batch covered, not beyond.
	if per.last.CoveredSeq != 2 {
		t.Errorf("covered_seq = %d, want 2 (the batch's submit-time seq)", per.last.CoveredSeq)
	}
	// The result's memory resolved its provenance from the re-read event at seq 1.
	if len(per.last.Memories) != 1 || per.last.Memories[0].Content != "distilled" ||
		per.last.Memories[0].SourceSeq != 1 || per.last.Memories[0].CreatedByAgent != "planner" {
		t.Errorf("persisted memories = %+v, want one distilled memory at seq 1 by planner", per.last.Memories)
	}
}

func TestExtractRunWorker_CollectWithoutBatchCapabilityErrors(t *testing.T) {
	// A pending batch but a non-batch extractor (the provider configuration changed): fail loudly
	// without persisting, so the run stays put until a batch-capable provider is restored.
	src := &fakeSource{state: pendingBatchState("batch_9", 2)}
	per := &spyPersister{}
	w := jobs.NewExtractRunWorker(src, &spyExtractor{}, per, jobs.DefaultDebounce())
	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}
	if err := w.Work(context.Background(), job); err == nil {
		t.Error("a pending batch with a non-batch extractor should error")
	}
	if per.calls != 0 {
		t.Error("must not persist or advance when it cannot collect")
	}
}
