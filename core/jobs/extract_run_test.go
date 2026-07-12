package jobs_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
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

// fakeLister returns a canned event set and records the query params it was called with.
type fakeLister struct {
	events []db.Event
	gotArg db.ListRunEventsParams
}

func (f *fakeLister) ListRunEvents(_ context.Context, arg db.ListRunEventsParams) ([]db.Event, error) {
	f.gotArg = arg
	return f.events, nil
}

// spyExtractor records the window it was called with.
type spyExtractor struct {
	got   ext.ExtractInput
	calls int
}

func (s *spyExtractor) Extract(_ context.Context, in ext.ExtractInput) (ext.ExtractResult, error) {
	s.got = in
	s.calls++
	return ext.ExtractResult{}, nil
}

func TestExtractRunWorker_GatesThenExtracts(t *testing.T) {
	proj := uuid.NewString()
	run := uuid.NewString()
	lister := &fakeLister{events: []db.Event{
		{Seq: 1, AgentID: "a", Payload: []byte(`{"memory":"keep"}`)},
		{Seq: 2, AgentID: "a", Payload: []byte(`{"kind":"tool_log","data":"noise"}`)}, // gated out
		{Seq: 3, AgentID: "b", Payload: []byte(`{"claim":{"entity":"e","predicate":"p","value":1}}`)},
	}}
	spy := &spyExtractor{}
	w := jobs.NewExtractRunWorker(lister, spy)

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
	// The worker scoped the read to the job's project and run (a valid UUID pair reached the lister).
	if !lister.gotArg.ProjectID.Valid || !lister.gotArg.RunID.Valid {
		t.Error("lister was not called with a valid project_id/run_id")
	}
}

func TestExtractRunWorker_ExtractorErrorPropagates(t *testing.T) {
	// A fixture_error event makes the real FixtureExtractor fail; the worker must surface the error
	// so River retries the pass rather than dropping it.
	lister := &fakeLister{events: []db.Event{
		{Seq: 1, AgentID: "a", Payload: []byte(`{"fixture_error":"unavailable"}`)},
	}}
	w := jobs.NewExtractRunWorker(lister, ext.FixtureExtractor{})
	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}
	if err := w.Work(context.Background(), job); err == nil {
		t.Error("Work should surface the extractor error so the job retries")
	}
}

func TestExtractRunWorker_BadUUIDFails(t *testing.T) {
	w := jobs.NewExtractRunWorker(&fakeLister{}, &spyExtractor{})
	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: "not-a-uuid", RunID: uuid.NewString()}}
	if err := w.Work(context.Background(), job); err == nil {
		t.Error("Work with a malformed project_id should error")
	}
}
