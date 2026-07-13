//go:build integration

package queue_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/jobs"
	"github.com/lore-gpt/lore/core/store/db"
)

// TestPGPersisterClaimConflictLWWRecordsReason proves the default (last-write-wins) adjudicator on a claim
// conflict: the later claim's value wins, and the superseded claim is stamped with a reason naming the
// policy and the winning/losing provenance.
func TestPGPersisterClaimConflictLWWRecordsReason(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, run := seedProjectRun(ctx, t, st)
	q := db.New(st.Pool)
	p := jobs.NewPGPersister(st, ext.LWW{})

	ev1, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte(`{"m":1}`)})
	if err != nil {
		t.Fatalf("insert event 1: %v", err)
	}
	ev2, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte(`{"m":2}`)})
	if err != nil {
		t.Fatalf("insert event 2: %v", err)
	}

	if err := p.Persist(ctx, jobs.PersistInput{
		ProjectID:          proj.ID,
		RunID:              run.ID,
		ExpectedCoveredSeq: 0,
		CoveredSeq:         ev2.Seq,
		Claims: []jobs.ClaimWrite{
			{Entity: "auth", Predicate: "state", Value: []byte(`"active"`), SourceEventID: ev1.ID, SourceSeq: ev1.Seq},
			{Entity: "auth", Predicate: "state", Value: []byte(`"done"`), SourceEventID: ev2.ID, SourceSeq: ev2.Seq},
		},
	}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	// The active claim is the later value.
	var activeValue []byte
	if err := st.Pool.QueryRow(ctx,
		`SELECT c.value FROM claims c JOIN entities e ON e.id = c.entity_id
		  WHERE c.project_id = $1 AND e.name = 'auth' AND c.predicate = 'state' AND c.superseded_by IS NULL`,
		proj.ID).Scan(&activeValue); err != nil {
		t.Fatalf("read active claim: %v", err)
	}
	if string(activeValue) != `"done"` {
		t.Errorf("active claim value = %s, want \"done\" (last-write-wins)", activeValue)
	}

	// The superseded claim carries the reason: policy id + a "supersedes" audit line naming the loser seq.
	var supersededValue []byte
	var reason *string
	if err := st.Pool.QueryRow(ctx,
		`SELECT c.value, c.resolution_reason FROM claims c JOIN entities e ON e.id = c.entity_id
		  WHERE c.project_id = $1 AND e.name = 'auth' AND c.predicate = 'state' AND c.superseded_by IS NOT NULL`,
		proj.ID).Scan(&supersededValue, &reason); err != nil {
		t.Fatalf("read superseded claim: %v", err)
	}
	if string(supersededValue) != `"active"` {
		t.Errorf("superseded claim value = %s, want the earlier \"active\"", supersededValue)
	}
	if reason == nil {
		t.Fatal("superseded claim should carry a resolution_reason")
	}
	if !strings.HasPrefix(*reason, "last-write-wins:") {
		t.Errorf("reason = %q, want it to name the policy (last-write-wins:)", *reason)
	}
	if !strings.Contains(*reason, "supersedes") {
		t.Errorf("reason = %q, want the winner/loser supersedes audit line", *reason)
	}
	// Pin the winner/loser ORDER, not just the presence of numbers: the winner (incoming, seq ev2) is
	// named immediately before "supersedes" and the loser (superseded, seq ev1) after it. A transposition
	// of the winner and loser provenance — which would name the wrong claim as the victor — is caught here.
	if !strings.Contains(*reason, fmt.Sprintf("seq %d) supersedes", ev2.Seq)) {
		t.Errorf("reason = %q, want the winner's seq %d immediately before 'supersedes'", *reason, ev2.Seq)
	}
	if after := (*reason)[strings.Index(*reason, "supersedes"):]; !strings.Contains(after, fmt.Sprintf("seq %d)", ev1.Seq)) {
		t.Errorf("reason = %q, want the loser's seq %d named after 'supersedes'", *reason, ev1.Seq)
	}
}

// TestPGPersisterClaimConflictFieldMerge proves a non-default adjudicator is honoured: injecting
// FieldMerge, two object-valued claims about one subject combine (incoming fields override) instead of the
// later one simply replacing the earlier, and the recorded reason names the field-merge policy.
func TestPGPersisterClaimConflictFieldMerge(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, run := seedProjectRun(ctx, t, st)
	q := db.New(st.Pool)
	p := jobs.NewPGPersister(st, ext.FieldMerge{})

	ev1, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte(`{"m":1}`)})
	if err != nil {
		t.Fatalf("insert event 1: %v", err)
	}
	ev2, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte(`{"m":2}`)})
	if err != nil {
		t.Fatalf("insert event 2: %v", err)
	}

	if err := p.Persist(ctx, jobs.PersistInput{
		ProjectID:          proj.ID,
		RunID:              run.ID,
		ExpectedCoveredSeq: 0,
		CoveredSeq:         ev2.Seq,
		Claims: []jobs.ClaimWrite{
			{Entity: "svc", Predicate: "meta", Value: []byte(`{"a":1,"b":2}`), SourceEventID: ev1.ID, SourceSeq: ev1.Seq},
			{Entity: "svc", Predicate: "meta", Value: []byte(`{"b":3,"c":4}`), SourceEventID: ev2.ID, SourceSeq: ev2.Seq},
		},
	}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	var activeValue []byte
	var supersededReason *string
	if err := st.Pool.QueryRow(ctx,
		`SELECT c.value FROM claims c JOIN entities e ON e.id = c.entity_id
		  WHERE c.project_id = $1 AND e.name = 'svc' AND c.predicate = 'meta' AND c.superseded_by IS NULL`,
		proj.ID).Scan(&activeValue); err != nil {
		t.Fatalf("read active claim: %v", err)
	}
	var got map[string]int
	if err := json.Unmarshal(activeValue, &got); err != nil {
		t.Fatalf("active value is not a JSON object: %v (%s)", err, activeValue)
	}
	want := map[string]int{"a": 1, "b": 3, "c": 4}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("merged claim key %q = %d, want %d (field-merge, incoming overrides)", k, got[k], v)
		}
	}

	if err := st.Pool.QueryRow(ctx,
		`SELECT c.resolution_reason FROM claims c JOIN entities e ON e.id = c.entity_id
		  WHERE c.project_id = $1 AND e.name = 'svc' AND c.predicate = 'meta' AND c.superseded_by IS NOT NULL`,
		proj.ID).Scan(&supersededReason); err != nil {
		t.Fatalf("read superseded claim reason: %v", err)
	}
	if supersededReason == nil || !strings.HasPrefix(*supersededReason, "field-merge:") {
		t.Errorf("reason = %v, want it to name the field-merge policy", supersededReason)
	}
}

// TestPGPersisterFirstClaimNoConflict proves the no-conflict path: a subject's first assertion has no
// active claim to adjudicate, so it inserts cleanly and carries no resolution_reason.
func TestPGPersisterFirstClaimNoConflict(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, run := seedProjectRun(ctx, t, st)
	q := db.New(st.Pool)
	p := jobs.NewPGPersister(st, ext.LWW{})

	ev, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte(`{"m":1}`)})
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if err := p.Persist(ctx, jobs.PersistInput{
		ProjectID:          proj.ID,
		RunID:              run.ID,
		ExpectedCoveredSeq: 0,
		CoveredSeq:         ev.Seq,
		Claims:             []jobs.ClaimWrite{{Entity: "auth", Predicate: "state", Value: []byte(`"active"`), SourceEventID: ev.ID, SourceSeq: ev.Seq}},
	}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	var total int64
	var reason *string
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) OVER (), resolution_reason FROM claims WHERE project_id = $1 LIMIT 1`, proj.ID).
		Scan(&total, &reason); err != nil {
		t.Fatalf("read claim: %v", err)
	}
	if total != 1 {
		t.Errorf("claims = %d, want 1 (first assertion, no supersession)", total)
	}
	if reason != nil {
		t.Errorf("first claim resolution_reason = %q, want NULL (nothing was superseded)", *reason)
	}
}
