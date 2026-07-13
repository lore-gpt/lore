package jobs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/jobs"
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
}

func (f *fakeSource) RunExtractionReadiness(_ context.Context, _ db.RunExtractionReadinessParams) (db.RunExtractionReadinessRow, error) {
	f.readinessCalls++
	if f.readinessCalls == 1 {
		return f.readiness, nil
	}
	return f.tailReadiness, nil
}

func (f *fakeSource) ListRunEvents(_ context.Context, arg db.ListRunEventsParams) ([]db.Event, error) {
	f.gotArg = arg
	return f.events, nil
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
}

func (p *spyPersister) Persist(_ context.Context, in jobs.PersistInput) error {
	p.calls++
	p.last = in
	return nil
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
