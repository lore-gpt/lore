package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/jobs"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/core/workmem"
)

var errFakeCache = errors.New("cache blip")

// fakeWorkmem is a workmem.Store with a fixed Mode that records the keys it was asked to Set (and can fail
// the write), so a routing test can assert whether the hot lane was written and whether a durable fallback
// was produced.
type fakeWorkmem struct {
	mode workmem.Mode
	err  error
	sets []workmem.Key
	vals []workmem.Value
}

func (f *fakeWorkmem) Set(_ context.Context, k workmem.Key, v workmem.Value) error {
	f.sets = append(f.sets, k) // record the attempt (key + value) regardless of outcome
	f.vals = append(f.vals, v)
	return f.err
}
func (*fakeWorkmem) Get(context.Context, workmem.Key) (workmem.Value, bool, error) {
	return workmem.Value{}, false, nil
}
func (*fakeWorkmem) GetAll(context.Context, string, string) ([]workmem.Entry, error) { return nil, nil }
func (f *fakeWorkmem) Mode() workmem.Mode                                            { return f.mode }
func (*fakeWorkmem) Close()                                                          {}

const stateEventPayload = `{"kind":"state","entity":"auth","predicate":"status","value":"up"}`

// TestExtractRunWorker_RoutesStateFacts proves kind:"state" events are kept out of the extraction window
// and routed by working-memory health: a healthy stripe takes the hot fact (no durable claim); a
// disabled/degraded stripe — or a healthy stripe whose write fails — preserves it as a durable claim. Either
// way the checkpoint advances past the event and the model never sees it.
func TestExtractRunWorker_RoutesStateFacts(t *testing.T) {
	cases := []struct {
		name             string
		mode             workmem.Mode
		setErr           error
		wantSetAttempts  int
		wantDurableClaim bool
	}{
		{"healthy takes the hot fact, no claim", workmem.Healthy, nil, 1, false},
		{"healthy but the write fails falls back to a durable claim", workmem.Healthy, errFakeCache, 1, true},
		{"degraded writes a durable claim, no hot-lane write", workmem.Degraded, nil, 0, true},
		{"disabled writes a durable claim, no hot-lane write", workmem.Disabled, nil, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeSource{
				events:    []db.Event{{Seq: 1, AgentID: "planner", Payload: []byte(stateEventPayload)}},
				readiness: ready(1),
			}
			spy := &spyExtractor{}
			per := &spyPersister{}
			wm := &fakeWorkmem{mode: tc.mode, err: tc.setErr}
			w := jobs.NewExtractRunWorker(src, spy, per, jobs.DefaultDebounce(), jobs.WithWorkmemStore(wm))

			job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}
			if err := w.Work(context.Background(), job); err != nil {
				t.Fatalf("Work: %v", err)
			}

			if spy.calls != 0 {
				t.Errorf("extractor calls = %d, want 0 (a state event is never distilled)", spy.calls)
			}
			if len(wm.sets) != tc.wantSetAttempts {
				t.Errorf("hot-lane write attempts = %d, want %d", len(wm.sets), tc.wantSetAttempts)
			}
			if per.calls != 1 {
				t.Fatalf("persister calls = %d, want 1", per.calls)
			}
			if per.last.CoveredSeq != 1 {
				t.Errorf("covered_seq = %d, want 1 (the state event is consumed)", per.last.CoveredSeq)
			}
			gotClaim := len(per.last.Claims) == 1
			if gotClaim != tc.wantDurableClaim {
				t.Fatalf("durable claims = %d, want durable=%v", len(per.last.Claims), tc.wantDurableClaim)
			}
			if tc.wantDurableClaim {
				c := per.last.Claims[0]
				if c.Entity != "auth" || c.Predicate != "status" || string(c.Value) != `"up"` || c.SourceSeq != 1 {
					t.Errorf("state claim = {%q,%q,%s,seq %d}, want {auth,status,\"up\",1}", c.Entity, c.Predicate, c.Value, c.SourceSeq)
				}
			}
			if tc.wantSetAttempts > 0 {
				if wm.sets[0].Entity != "auth" || wm.sets[0].Predicate != "status" {
					t.Errorf("hot-lane key = {%q,%q}, want {auth,status}", wm.sets[0].Entity, wm.sets[0].Predicate)
				}
				// The hot-lane write carries the fact's value and the event's freshness/provenance.
				if string(wm.vals[0].Value) != `"up"` || wm.vals[0].Seq != 1 || wm.vals[0].Agent != "planner" {
					t.Errorf("hot-lane value = {%s, seq %d, %s}, want {\"up\", 1, planner}", wm.vals[0].Value, wm.vals[0].Seq, wm.vals[0].Agent)
				}
			}
		})
	}
}

// TestExtractRunWorker_MalformedStateEventDropped proves a kind:"state" event that fails validation is
// dropped (never distilled, never persisted) rather than crashing the pass. The server validates at
// ingestion, so this only guards a non-standard or pre-validation event; the checkpoint still advances.
func TestExtractRunWorker_MalformedStateEventDropped(t *testing.T) {
	src := &fakeSource{
		events:    []db.Event{{Seq: 1, AgentID: "a", Payload: []byte(`{"kind":"state","predicate":"status","value":"up"}`)}}, // no entity
		readiness: ready(1),
	}
	spy := &spyExtractor{}
	per := &spyPersister{}
	wm := &fakeWorkmem{mode: workmem.Degraded}
	w := jobs.NewExtractRunWorker(src, spy, per, jobs.DefaultDebounce(), jobs.WithWorkmemStore(wm))

	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if spy.calls != 0 {
		t.Errorf("extractor calls = %d, want 0 (malformed state event not distilled)", spy.calls)
	}
	if len(per.last.Claims) != 0 {
		t.Errorf("durable claims = %d, want 0 (malformed state event dropped)", len(per.last.Claims))
	}
	if per.last.CoveredSeq != 1 {
		t.Errorf("covered_seq = %d, want 1 (still consumed)", per.last.CoveredSeq)
	}
}

// TestExtractRunWorker_MixedWindowRoutesStateAndExtractsRest proves a window with both a regular event and
// a state event sends only the regular one to the model and routes the state fact separately, with the
// checkpoint at the highest seq read.
func TestExtractRunWorker_MixedWindowRoutesStateAndExtractsRest(t *testing.T) {
	src := &fakeSource{
		events: []db.Event{
			{Seq: 1, AgentID: "a", Payload: []byte(`{"memory":"keep"}`)},
			{Seq: 2, AgentID: "b", Payload: []byte(stateEventPayload)},
		},
		readiness: ready(2),
	}
	spy := &spyExtractor{}
	per := &spyPersister{}
	wm := &fakeWorkmem{mode: workmem.Disabled} // durable-claim path
	w := jobs.NewExtractRunWorker(src, spy, per, jobs.DefaultDebounce(), jobs.WithWorkmemStore(wm))

	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if spy.calls != 1 || len(spy.got.Events) != 1 || spy.got.Events[0].Seq != 1 {
		t.Fatalf("extraction window = %d events (calls %d), want the single non-state event seq 1", len(spy.got.Events), spy.calls)
	}
	if len(per.last.Claims) != 1 || per.last.Claims[0].SourceSeq != 2 {
		t.Fatalf("durable claims = %v, want one state claim at seq 2", per.last.Claims)
	}
	if per.last.CoveredSeq != 2 {
		t.Errorf("covered_seq = %d, want 2", per.last.CoveredSeq)
	}
}

// TestExtractRunWorker_MergesStateAndExtractedClaimsInSeqOrder proves the durable state claims and the
// extractor's claims are merged and re-sorted ascending by SourceSeq, so per-subject supersession stays
// last-write-wins regardless of which path produced a claim — the re-sort of the combined slice, not just
// the extractor's claims.
func TestExtractRunWorker_MergesStateAndExtractedClaimsInSeqOrder(t *testing.T) {
	src := &fakeSource{
		events: []db.Event{
			{Seq: 2, AgentID: "a", Payload: []byte(stateEventPayload)},     // state fact -> a durable claim at seq 2
			{Seq: 3, AgentID: "b", Payload: []byte(`{"claim":"present"}`)}, // a non-state event so the window is non-empty
		},
		readiness: ready(2),
	}
	// The extractor returns a claim at a HIGHER seq (3) than the state event (2); after append the slice is
	// [seq3, seq2], so only the re-sort makes it [seq2, seq3].
	spy := &spyExtractor{result: ext.ExtractResult{Claims: []ext.CandidateClaim{
		{Entity: "db", Predicate: "ver", Value: json.RawMessage(`"2"`), SourceSeq: 3},
	}}}
	per := &spyPersister{}
	wm := &fakeWorkmem{mode: workmem.Disabled} // durable-claim path for the state fact
	w := jobs.NewExtractRunWorker(src, spy, per, jobs.DefaultDebounce(), jobs.WithWorkmemStore(wm))

	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(per.last.Claims) != 2 {
		t.Fatalf("claims = %d, want 2 (state + extracted merged)", len(per.last.Claims))
	}
	if per.last.Claims[0].SourceSeq != 2 || per.last.Claims[0].Entity != "auth" {
		t.Errorf("claims[0] = {seq %d, %q}, want the state claim {seq 2, auth} (ascending seq order)", per.last.Claims[0].SourceSeq, per.last.Claims[0].Entity)
	}
	if per.last.Claims[1].SourceSeq != 3 || per.last.Claims[1].Entity != "db" {
		t.Errorf("claims[1] = {seq %d, %q}, want the extracted claim {seq 3, db}", per.last.Claims[1].SourceSeq, per.last.Claims[1].Entity)
	}
}

// TestExtractRunWorker_CollectRoutesStateFacts proves the economy collect phase routes state facts too:
// they were excluded from the submitted batch, so the collect-time pass re-reads them and (here, with a
// degraded stripe) preserves them as durable claims alongside the batch's distilled result.
func TestExtractRunWorker_CollectRoutesStateFacts(t *testing.T) {
	src := &fakeSource{
		events: []db.Event{
			{Seq: 1, AgentID: "planner", Payload: []byte(`{"memory":"x"}`)},
			{Seq: 2, AgentID: "coder", Payload: []byte(stateEventPayload)},
		},
		state: pendingBatchState("batch_1", 2),
	}
	batch := &spyBatchExtractor{doneAfter: 0} // immediately ready, empty distilled result
	per := &spyPersister{}
	wm := &fakeWorkmem{mode: workmem.Degraded}
	w := jobs.NewExtractRunWorker(src, batch, per, jobs.DefaultDebounce(), jobs.WithWorkmemStore(wm))

	job := &river.Job[jobs.ExtractRunArgs]{Args: jobs.ExtractRunArgs{ProjectID: uuid.NewString(), RunID: uuid.NewString()}}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if per.calls != 1 {
		t.Fatalf("collect should persist once, got %d", per.calls)
	}
	if len(per.last.Claims) != 1 || per.last.Claims[0].SourceSeq != 2 || per.last.Claims[0].Entity != "auth" {
		t.Fatalf("collect-time state routing = %v, want one durable claim for the state fact at seq 2", per.last.Claims)
	}
}
