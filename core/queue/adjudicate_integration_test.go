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
	p := jobs.NewPGPersister(st, ext.LWW{}, ext.FixtureEmbedder{})

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
	p := jobs.NewPGPersister(st, ext.FieldMerge{}, ext.FixtureEmbedder{})

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
	p := jobs.NewPGPersister(st, ext.LWW{}, ext.FixtureEmbedder{})

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

// TestPGPersisterClaimConflictAcrossPasses proves the persister's overlay of active claims is populated
// from the database, not only from in-pass inserts: a subject asserted in one pass is superseded by a
// later, separate pass. The second pass has no in-pass insert to see, so it must find the active claim
// through the batched GetActiveClaimsByEntities read, adjudicate, and supersede it. A regression that
// dropped that read would insert a second active row for the subject and trip the partial-unique index.
func TestPGPersisterClaimConflictAcrossPasses(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, run := seedProjectRun(ctx, t, st)
	q := db.New(st.Pool)
	p := jobs.NewPGPersister(st, ext.LWW{}, ext.FixtureEmbedder{})

	ev1, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte(`{"m":1}`)})
	if err != nil {
		t.Fatalf("insert event 1: %v", err)
	}
	if err := p.Persist(ctx, jobs.PersistInput{
		ProjectID:          proj.ID,
		RunID:              run.ID,
		ExpectedCoveredSeq: 0,
		CoveredSeq:         ev1.Seq,
		Claims:             []jobs.ClaimWrite{{Entity: "auth", Predicate: "state", Value: []byte(`"active"`), SourceEventID: ev1.ID, SourceSeq: ev1.Seq}},
	}); err != nil {
		t.Fatalf("persist pass 1: %v", err)
	}

	ev2, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte(`{"m":2}`)})
	if err != nil {
		t.Fatalf("insert event 2: %v", err)
	}
	if err := p.Persist(ctx, jobs.PersistInput{
		ProjectID:          proj.ID,
		RunID:              run.ID,
		ExpectedCoveredSeq: ev1.Seq,
		CoveredSeq:         ev2.Seq,
		Claims:             []jobs.ClaimWrite{{Entity: "auth", Predicate: "state", Value: []byte(`"done"`), SourceEventID: ev2.ID, SourceSeq: ev2.Seq}},
	}); err != nil {
		t.Fatalf("persist pass 2: %v", err)
	}

	// Exactly one active claim for the subject, the second pass's value — the batched read must have found
	// and superseded the first pass's claim rather than leaving two active rows.
	var activeValue []byte
	var activeCount int64
	if err := st.Pool.QueryRow(ctx,
		`SELECT c.value, count(*) OVER () FROM claims c JOIN entities e ON e.id = c.entity_id
		  WHERE c.project_id = $1 AND e.name = 'auth' AND c.predicate = 'state' AND c.superseded_by IS NULL`,
		proj.ID).Scan(&activeValue, &activeCount); err != nil {
		t.Fatalf("read active claim: %v", err)
	}
	if activeCount != 1 {
		t.Fatalf("active claims for subject = %d, want exactly 1", activeCount)
	}
	if string(activeValue) != `"done"` {
		t.Errorf("active claim value = %s, want \"done\" (the second pass supersedes the first)", activeValue)
	}

	// The first pass's claim is superseded, with a reason naming pass 2 (ev2) as the winner.
	var supersededValue []byte
	var reason *string
	if err := st.Pool.QueryRow(ctx,
		`SELECT c.value, c.resolution_reason FROM claims c JOIN entities e ON e.id = c.entity_id
		  WHERE c.project_id = $1 AND e.name = 'auth' AND c.predicate = 'state' AND c.superseded_by IS NOT NULL`,
		proj.ID).Scan(&supersededValue, &reason); err != nil {
		t.Fatalf("read superseded claim: %v", err)
	}
	if string(supersededValue) != `"active"` {
		t.Errorf("superseded value = %s, want \"active\" (the first pass's claim)", supersededValue)
	}
	if reason == nil || !strings.Contains(*reason, fmt.Sprintf("seq %d) supersedes", ev2.Seq)) {
		t.Errorf("reason = %v, want ev2 (seq %d) named as the winner before 'supersedes'", reason, ev2.Seq)
	}
}

// TestPGPersisterSameSubjectThriceInPassLWW proves the overlay carries provenance forward, not just the
// value. Three claims for one subject in a single pass supersede in seq order; the middle claim is
// superseded with a reason naming the third as winner over the SECOND — whose run/seq the overlay recorded
// when it was inserted, since the loop never re-reads the database. A regression that failed to refresh the
// overlay's provenance after an in-pass insert would misname the loser (e.g. still point at the first).
func TestPGPersisterSameSubjectThriceInPassLWW(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, run := seedProjectRun(ctx, t, st)
	q := db.New(st.Pool)
	p := jobs.NewPGPersister(st, ext.LWW{}, ext.FixtureEmbedder{})

	ev1, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte(`{"m":1}`)})
	if err != nil {
		t.Fatalf("insert event 1: %v", err)
	}
	ev2, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte(`{"m":2}`)})
	if err != nil {
		t.Fatalf("insert event 2: %v", err)
	}
	ev3, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte(`{"m":3}`)})
	if err != nil {
		t.Fatalf("insert event 3: %v", err)
	}

	if err := p.Persist(ctx, jobs.PersistInput{
		ProjectID:          proj.ID,
		RunID:              run.ID,
		ExpectedCoveredSeq: 0,
		CoveredSeq:         ev3.Seq,
		Claims: []jobs.ClaimWrite{
			{Entity: "auth", Predicate: "state", Value: []byte(`"a"`), SourceEventID: ev1.ID, SourceSeq: ev1.Seq},
			{Entity: "auth", Predicate: "state", Value: []byte(`"b"`), SourceEventID: ev2.ID, SourceSeq: ev2.Seq},
			{Entity: "auth", Predicate: "state", Value: []byte(`"c"`), SourceEventID: ev3.ID, SourceSeq: ev3.Seq},
		},
	}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	// The final active value is the last claim, and it is the only active row for the subject.
	var activeValue []byte
	var activeCount int64
	if err := st.Pool.QueryRow(ctx,
		`SELECT c.value, count(*) OVER () FROM claims c JOIN entities e ON e.id = c.entity_id
		  WHERE c.project_id = $1 AND e.name = 'auth' AND c.predicate = 'state' AND c.superseded_by IS NULL`,
		proj.ID).Scan(&activeValue, &activeCount); err != nil {
		t.Fatalf("read active claim: %v", err)
	}
	if activeCount != 1 || string(activeValue) != `"c"` {
		t.Fatalf("active claim = %s (count %d), want \"c\" (count 1)", activeValue, activeCount)
	}

	// The middle claim ("b") was superseded by the third ("c"): its reason must name ev3 as the winner and
	// ev2 as the loser. ev2's seq is the value the overlay recorded when "b" was inserted — not a re-read.
	var reason *string
	if err := st.Pool.QueryRow(ctx,
		`SELECT c.resolution_reason FROM claims c JOIN entities e ON e.id = c.entity_id
		  WHERE c.project_id = $1 AND e.name = 'auth' AND c.predicate = 'state' AND c.value = '"b"'::jsonb`,
		proj.ID).Scan(&reason); err != nil {
		t.Fatalf("read middle claim: %v", err)
	}
	if reason == nil {
		t.Fatal("middle claim should carry a resolution_reason (superseded by the third)")
	}
	if !strings.Contains(*reason, fmt.Sprintf("seq %d) supersedes", ev3.Seq)) {
		t.Errorf("reason = %q, want ev3 (seq %d) as the winner before 'supersedes'", *reason, ev3.Seq)
	}
	if after := (*reason)[strings.Index(*reason, "supersedes"):]; !strings.Contains(after, fmt.Sprintf("seq %d)", ev2.Seq)) {
		t.Errorf("reason = %q, want ev2 (seq %d) as the loser after 'supersedes'", *reason, ev2.Seq)
	}
}

// TestPGPersisterOverlayIsolatesPredicatesPerEntity proves the overlay is keyed by the full subject
// (entity, predicate), not the entity alone — so distinct predicates of one entity never contaminate each
// other, and the batched read's over-fetch (it returns every active predicate of a touched entity) is inert.
// It runs under FieldMerge so any cross-predicate bleed corrupts a value loudly. Pass 1 asserts two
// predicates of one entity independently (exercising the overlay's own in-pass updates); pass 2 touches only
// one predicate, whose batched read over-fetches the other predicate's active row, which must stay untouched.
func TestPGPersisterOverlayIsolatesPredicatesPerEntity(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, run := seedProjectRun(ctx, t, st)
	q := db.New(st.Pool)
	p := jobs.NewPGPersister(st, ext.FieldMerge{}, ext.FixtureEmbedder{})

	// assertActive checks the single active claim for (svc, predicate) holds exactly wantKeys and no more —
	// so a cross-predicate field-merge (the mis-keying mutant) shows up as an extra key. An active claim
	// never carries a resolution_reason (that lives on the superseded loser), so this always expects NULL.
	assertActive := func(predicate string, wantKeys map[string]int) {
		t.Helper()
		var value []byte
		var reason *string
		var n int64
		if err := st.Pool.QueryRow(ctx,
			`SELECT c.value, c.resolution_reason, count(*) OVER () FROM claims c JOIN entities e ON e.id = c.entity_id
			  WHERE c.project_id = $1 AND e.name = 'svc' AND c.predicate = $2 AND c.superseded_by IS NULL`,
			proj.ID, predicate).Scan(&value, &reason, &n); err != nil {
			t.Fatalf("read active (svc,%s): %v", predicate, err)
		}
		if n != 1 {
			t.Fatalf("active claims for (svc,%s) = %d, want exactly 1", predicate, n)
		}
		var got map[string]int
		if err := json.Unmarshal(value, &got); err != nil {
			t.Fatalf("(svc,%s) value is not a JSON object: %v (%s)", predicate, err, value)
		}
		if len(got) != len(wantKeys) {
			t.Errorf("(svc,%s) value = %s, want exactly keys %v (no cross-predicate bleed)", predicate, value, wantKeys)
		}
		for k, v := range wantKeys {
			if got[k] != v {
				t.Errorf("(svc,%s) key %q = %d, want %d", predicate, k, got[k], v)
			}
		}
		if reason != nil {
			t.Errorf("(svc,%s) active claim carries resolution_reason %q, want NULL", predicate, *reason)
		}
	}

	ev1, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte(`{"m":1}`)})
	if err != nil {
		t.Fatalf("insert event 1: %v", err)
	}
	ev2, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte(`{"m":2}`)})
	if err != nil {
		t.Fatalf("insert event 2: %v", err)
	}

	// Pass 1: one entity, two DISTINCT predicates, both first assertions. Correct code keys the overlay by
	// full subject so they stay separate; an entity-only key would make the second claim hit the first's
	// overlay entry, field-merge across predicates, and stamp a spurious reason.
	if err := p.Persist(ctx, jobs.PersistInput{
		ProjectID:          proj.ID,
		RunID:              run.ID,
		ExpectedCoveredSeq: 0,
		CoveredSeq:         ev2.Seq,
		Claims: []jobs.ClaimWrite{
			{Entity: "svc", Predicate: "cfg", Value: []byte(`{"x":1}`), SourceEventID: ev1.ID, SourceSeq: ev1.Seq},
			{Entity: "svc", Predicate: "status", Value: []byte(`{"y":2}`), SourceEventID: ev2.ID, SourceSeq: ev2.Seq},
		},
	}); err != nil {
		t.Fatalf("persist pass 1: %v", err)
	}
	assertActive("cfg", map[string]int{"x": 1})
	assertActive("status", map[string]int{"y": 2})

	// Pass 2: touch ONLY predicate "status" again. Its batched read over-fetches the still-active "cfg" row
	// for the same entity; the full-subject overlay never looks it up, so "cfg" stays untouched while
	// "status" field-merges with a reason.
	ev3, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte(`{"m":3}`)})
	if err != nil {
		t.Fatalf("insert event 3: %v", err)
	}
	if err := p.Persist(ctx, jobs.PersistInput{
		ProjectID:          proj.ID,
		RunID:              run.ID,
		ExpectedCoveredSeq: ev2.Seq,
		CoveredSeq:         ev3.Seq,
		Claims: []jobs.ClaimWrite{
			{Entity: "svc", Predicate: "status", Value: []byte(`{"z":3}`), SourceEventID: ev3.ID, SourceSeq: ev3.Seq},
		},
	}); err != nil {
		t.Fatalf("persist pass 2: %v", err)
	}
	assertActive("cfg", map[string]int{"x": 1})            // over-fetched, untouched
	assertActive("status", map[string]int{"y": 2, "z": 3}) // field-merged with the pre-existing "status"

	// The merge came from a real conflict against the pre-existing "status" (not a fresh insert): the old
	// "status" row is superseded with a reason. "cfg" must NOT have been superseded by the "status" pass.
	var supersededStatus, supersededCfg int64
	if err := st.Pool.QueryRow(ctx,
		`SELECT
		   count(*) FILTER (WHERE c.predicate = 'status' AND c.superseded_by IS NOT NULL AND c.resolution_reason IS NOT NULL),
		   count(*) FILTER (WHERE c.predicate = 'cfg' AND c.superseded_by IS NOT NULL)
		 FROM claims c JOIN entities e ON e.id = c.entity_id
		 WHERE c.project_id = $1 AND e.name = 'svc'`,
		proj.ID).Scan(&supersededStatus, &supersededCfg); err != nil {
		t.Fatalf("count superseded claims: %v", err)
	}
	if supersededStatus != 1 {
		t.Errorf("superseded 'status' claims with a reason = %d, want 1 (the merge came from a conflict)", supersededStatus)
	}
	if supersededCfg != 0 {
		t.Errorf("superseded 'cfg' claims = %d, want 0 (the 'status' pass must not touch 'cfg')", supersededCfg)
	}
}

// TestPGPersisterSourcelessClaimOverlayProvenance proves the overlay records NO run/seq provenance for an
// in-pass claim with no source event (the else branch of the SourceEventID.Valid guard), mirroring what the
// events left-join would yield on a re-read. A source-less first claim, then a second claim for the same
// subject: the second supersedes the first, and the recorded reason names the loser with the nil run and
// seq 0. A regression that stamped the pass's real run onto a source-less overlay entry would fail here.
func TestPGPersisterSourcelessClaimOverlayProvenance(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, run := seedProjectRun(ctx, t, st)
	q := db.New(st.Pool)
	p := jobs.NewPGPersister(st, ext.LWW{}, ext.FixtureEmbedder{})

	ev, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte(`{"m":1}`)})
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}

	// In slice (SourceSeq) order the source-less claim (no SourceEventID, seq 0) is inserted first and
	// recorded in the overlay with nil provenance; the second, event-backed claim supersedes it.
	if err := p.Persist(ctx, jobs.PersistInput{
		ProjectID:          proj.ID,
		RunID:              run.ID,
		ExpectedCoveredSeq: 0,
		CoveredSeq:         ev.Seq,
		Claims: []jobs.ClaimWrite{
			{Entity: "auth", Predicate: "state", Value: []byte(`"first"`)}, // no SourceEventID => NULL, SourceSeq 0
			{Entity: "auth", Predicate: "state", Value: []byte(`"second"`), SourceEventID: ev.ID, SourceSeq: ev.Seq},
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
	if string(activeValue) != `"second"` {
		t.Errorf("active claim value = %s, want \"second\" (last-write-wins)", activeValue)
	}

	// The superseded (first, source-less) claim's reason names the loser with the nil run and seq 0 —
	// proving the overlay carried no provenance for it (a mutant stamping in.RunID would show the real run).
	var reason *string
	if err := st.Pool.QueryRow(ctx,
		`SELECT c.resolution_reason FROM claims c JOIN entities e ON e.id = c.entity_id
		  WHERE c.project_id = $1 AND e.name = 'auth' AND c.predicate = 'state' AND c.superseded_by IS NOT NULL`,
		proj.ID).Scan(&reason); err != nil {
		t.Fatalf("read superseded claim: %v", err)
	}
	if reason == nil {
		t.Fatal("superseded source-less claim should carry a resolution_reason")
	}
	if loser := (*reason)[strings.Index(*reason, "supersedes"):]; !strings.Contains(loser, "run 00000000-0000-0000-0000-000000000000 seq 0)") {
		t.Errorf("reason = %q, want the source-less loser named with the nil run and seq 0", *reason)
	}
}
