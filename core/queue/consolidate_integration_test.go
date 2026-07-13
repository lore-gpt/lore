//go:build integration

package queue_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/jobs"
	"github.com/lore-gpt/lore/core/store/db"
)

// TestAcquireEntityLocksSerializesByEntity pins the advisory lock's actual job — the mechanism the
// concurrency-safety story rests on — directly, so a regression that no-ops it or mis-keys it is caught
// (the persister-level tests below would still pass with the lock removed, because the checkpoint CAS and
// the unique indexes independently force one committer per run). It asserts two properties: a second
// acquire of the SAME entity blocks while the first transaction holds it, and a DIFFERENT entity acquires
// freely (per-entity parallelism is preserved, not a global lock).
func TestAcquireEntityLocksSerializesByEntity(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, _ := seedProjectRun(ctx, t, st)

	lock := func(tx pgx.Tx, name string) error {
		return db.New(tx).AcquireEntityLocks(ctx, db.AcquireEntityLocksParams{ProjectID: proj.ID, EntityNames: []string{name}})
	}

	// tx1 takes and holds the lock on "shared".
	tx1, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	defer func() { _ = tx1.Rollback(ctx) }()
	if err := lock(tx1, "shared"); err != nil {
		t.Fatalf("tx1 acquire: %v", err)
	}

	// A second acquire of the SAME entity must block until tx1 releases.
	same := make(chan error, 1)
	go func() {
		tx2, err := st.Pool.Begin(ctx)
		if err != nil {
			same <- err
			return
		}
		defer func() { _ = tx2.Rollback(ctx) }()
		same <- lock(tx2, "shared")
	}()
	select {
	case err := <-same:
		t.Fatalf("a second acquire of the same entity should block while the first holds it; it returned %v", err)
	case <-time.After(500 * time.Millisecond):
		// Still blocked — correct.
	}

	// A DIFFERENT entity must acquire freely (locks are per-entity, not global).
	other := make(chan error, 1)
	go func() {
		tx3, err := st.Pool.Begin(ctx)
		if err != nil {
			other <- err
			return
		}
		defer func() { _ = tx3.Rollback(ctx) }()
		other <- lock(tx3, "unrelated")
	}()
	select {
	case err := <-other:
		if err != nil {
			t.Errorf("a disjoint entity should acquire freely, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a disjoint entity lock blocked on an unrelated one — the lock is too coarse (global, not per-entity)")
	}

	// Releasing tx1 lets the blocked same-entity acquire proceed.
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("commit tx1: %v", err)
	}
	select {
	case err := <-same:
		if err != nil {
			t.Errorf("the second same-entity acquire should succeed once the holder releases, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second same-entity acquire did not proceed after the holder released")
	}
}

// TestPGPersisterDedupsIdenticalMemory proves exact-content dedup: two candidate memories whose content
// is identical after normalization (they differ only in casing and trailing punctuation) collapse to a
// single stored memory. The duplicate merges into the first — its version is bumped and the merge is
// recorded in memory_versions (with the re-observing agent and a reason) — rather than inserting a second
// row. The first-observed raw content is the one retained.
func TestPGPersisterDedupsIdenticalMemory(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, run := seedProjectRun(ctx, t, st)
	p := jobs.NewPGPersister(st, ext.LWW{})

	in := jobs.PersistInput{
		ProjectID:          proj.ID,
		RunID:              run.ID,
		ExpectedCoveredSeq: 0,
		CoveredSeq:         2,
		Memories: []jobs.MemoryWrite{
			{Kind: "semantic", Content: "Auth is done.", CreatedByAgent: "planner", SourceSeq: 1},
			{Kind: "semantic", Content: "auth   is done", CreatedByAgent: "tester", SourceSeq: 2},
		},
		// A claim from the SAME event as the merging memory: it must link to the RETAINED memory (the one
		// the duplicate merged into), not a fresh row.
		Claims: []jobs.ClaimWrite{{Entity: "auth", Predicate: "state", Value: []byte(`"done"`), SourceSeq: 2}},
	}
	if err := p.Persist(ctx, in); err != nil {
		t.Fatalf("persist: %v", err)
	}

	// Exactly one memory: the second restatement merged into the first rather than inserting.
	var count int64
	var memID pgtype.UUID
	var content string
	var version int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) OVER (), id, content, version FROM memories WHERE project_id = $1 LIMIT 1`, proj.ID).
		Scan(&count, &memID, &content, &version); err != nil {
		t.Fatalf("read memory: %v", err)
	}
	if count != 1 {
		t.Fatalf("memories = %d, want 1 (the identical restatement merges)", count)
	}
	if content != "Auth is done." {
		t.Errorf("content = %q, want the first-observed raw %q", content, "Auth is done.")
	}
	if version != 2 {
		t.Errorf("version = %d, want 2 (the merge bumped it)", version)
	}

	// The merge is recorded as a version snapshotting the memory's retained content, with the
	// re-observing agent and a reason.
	var vCount int
	var vContent string
	var vReason, vChangedBy *string
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) OVER (), content, reason, changed_by FROM memory_versions WHERE project_id = $1 LIMIT 1`, proj.ID).
		Scan(&vCount, &vContent, &vReason, &vChangedBy); err != nil {
		t.Fatalf("read memory_versions: %v", err)
	}
	if vCount != 1 {
		t.Fatalf("memory_versions rows = %d, want 1 (the merge)", vCount)
	}
	if vContent != "Auth is done." {
		t.Errorf("version content = %q, want the retained live content %q (not the incoming variant)", vContent, "Auth is done.")
	}
	if vReason == nil || *vReason == "" {
		t.Error("merge version should carry a reason")
	}
	if vChangedBy == nil || *vChangedBy != "tester" {
		t.Errorf("merge changed_by = %v, want the re-observing agent tester", vChangedBy)
	}

	var covered int64
	if err := st.Pool.QueryRow(ctx, `SELECT covered_seq FROM runs WHERE id = $1`, run.ID).Scan(&covered); err != nil {
		t.Fatalf("read covered_seq: %v", err)
	}
	if covered != 2 {
		t.Errorf("covered_seq = %d, want 2", covered)
	}

	// The same-event claim links to the RETAINED memory (the one the duplicate merged into), not a
	// dropped/duplicate row — proving provenance survives a dedup merge.
	var claimMemory pgtype.UUID
	if err := st.Pool.QueryRow(ctx, `SELECT memory_id FROM claims WHERE project_id = $1`, proj.ID).Scan(&claimMemory); err != nil {
		t.Fatalf("read claim: %v", err)
	}
	if claimMemory != memID {
		t.Errorf("claim.memory_id = %v, want the retained memory %v (the merge target)", claimMemory, memID)
	}
}

// TestPGPersisterAdvancesEmptyWindow proves an all-gated window (no memories, entities, or claims) still
// advances the checkpoint: the pass persists nothing but must move covered_seq so the archived events are
// never re-read. A mutant that skips the advance when there is nothing to write would strand the run.
func TestPGPersisterAdvancesEmptyWindow(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, run := seedProjectRun(ctx, t, st)
	p := jobs.NewPGPersister(st, ext.LWW{})

	if err := p.Persist(ctx, jobs.PersistInput{ProjectID: proj.ID, RunID: run.ID, ExpectedCoveredSeq: 0, CoveredSeq: 4}); err != nil {
		t.Fatalf("persist empty window: %v", err)
	}
	var mem, covered int64
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM memories WHERE project_id = $1`, proj.ID).Scan(&mem); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT covered_seq FROM runs WHERE id = $1`, run.ID).Scan(&covered); err != nil {
		t.Fatalf("read covered_seq: %v", err)
	}
	if mem != 0 || covered != 4 {
		t.Errorf("empty window: memories=%d covered_seq=%d, want 0 memories and covered_seq 4", mem, covered)
	}
}

// TestPGPersisterDedupScopedByEntityContext proves dedup stays inside an entity bucket: the same content
// under a DIFFERENT entity context is a separate memory (no false merge across contexts), while the same
// content under the SAME entity context merges. Three passes over one project, each mentioning one entity:
// alpha, then beta (same text, different context → a second row), then alpha again (same context → merges
// into the first).
func TestPGPersisterDedupScopedByEntityContext(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, run := seedProjectRun(ctx, t, st)
	p := jobs.NewPGPersister(st, ext.LWW{})

	const content = "shared insight"
	pass := func(expected, covered int64, entity string) error {
		return p.Persist(ctx, jobs.PersistInput{
			ProjectID:          proj.ID,
			RunID:              run.ID,
			ExpectedCoveredSeq: expected,
			CoveredSeq:         covered,
			Memories:           []jobs.MemoryWrite{{Kind: "semantic", Content: content, CreatedByAgent: "a", SourceSeq: covered}},
			Entities:           []jobs.EntityWrite{{Name: entity, Type: "service"}},
		})
	}

	if err := pass(0, 1, "alpha"); err != nil {
		t.Fatalf("pass alpha: %v", err)
	}
	if err := pass(1, 2, "beta"); err != nil { // same text, different entity context → separate row
		t.Fatalf("pass beta: %v", err)
	}
	if err := pass(2, 3, "alpha"); err != nil { // same text, same context as pass 1 → merges into it
		t.Fatalf("pass alpha again: %v", err)
	}

	// Two memories: alpha's (now version 2, the third pass merged in) and beta's (version 1, distinct
	// context). The identical text was NOT collapsed across the alpha/beta contexts.
	var total int64
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM memories WHERE project_id = $1`, proj.ID).Scan(&total); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if total != 2 {
		t.Fatalf("memories = %d, want 2 (alpha and beta contexts stay separate)", total)
	}
	var maxVersion, minVersion int
	if err := st.Pool.QueryRow(ctx, `SELECT max(version), min(version) FROM memories WHERE project_id = $1`, proj.ID).
		Scan(&maxVersion, &minVersion); err != nil {
		t.Fatalf("read versions: %v", err)
	}
	if maxVersion != 2 || minVersion != 1 {
		t.Errorf("versions = (min %d, max %d), want min 1 (beta, distinct) and max 2 (alpha, merged)", minVersion, maxVersion)
	}
}

// TestPGPersisterConcurrentDoubleDelivery proves the checkpoint compare-and-swap's exactly-once property:
// two passes delivering the same window for one run leave exactly one set of rows and advance the
// checkpoint once. Exactly one pass wins; the other's writes (its memory and claim) roll back when its
// CAS matches no row. It then proves a post-commit retry of the same window is a clean no-op for the same
// reason. (The advisory lock's own serialisation is pinned separately by
// TestAcquireEntityLocksSerializesByEntity — here the per-run CAS and the active-subject unique index
// already force one committer, so this test does not by itself exercise the lock.)
func TestPGPersisterConcurrentDoubleDelivery(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, run := seedProjectRun(ctx, t, st)
	p := jobs.NewPGPersister(st, ext.LWW{})

	mkInput := func() jobs.PersistInput {
		return jobs.PersistInput{
			ProjectID:          proj.ID,
			RunID:              run.ID,
			ExpectedCoveredSeq: 0,
			CoveredSeq:         1,
			Memories:           []jobs.MemoryWrite{{Kind: "semantic", Content: "shared fact", CreatedByAgent: "a", SourceSeq: 1}},
			Claims:             []jobs.ClaimWrite{{Entity: "auth", Predicate: "state", Value: []byte(`"up"`), SourceSeq: 1}},
		}
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = p.Persist(ctx, mkInput())
		}(i)
	}
	wg.Wait()

	// Exactly one pass advanced the checkpoint (returned nil); the other hit the compare-and-swap and
	// rolled back (a non-nil conflict).
	wins := 0
	for _, e := range errs {
		if e == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("exactly one pass should win; %d succeeded (errs = %v)", wins, errs)
	}

	assertSingleSet := func(stage string) {
		t.Helper()
		var memCount, claimTotal, claimSuperseded, versionRows int64
		var memVersion int
		if err := st.Pool.QueryRow(ctx,
			`SELECT count(*), coalesce(max(version), 0) FROM memories WHERE project_id = $1`, proj.ID).
			Scan(&memCount, &memVersion); err != nil {
			t.Fatalf("%s: count memories: %v", stage, err)
		}
		if memCount != 1 {
			t.Errorf("%s: memories = %d, want 1 (no duplicate)", stage, memCount)
		}
		if memVersion != 1 {
			t.Errorf("%s: memory version = %d, want 1 (the loser's merge/bump rolled back)", stage, memVersion)
		}
		if err := st.Pool.QueryRow(ctx,
			`SELECT count(*), count(*) FILTER (WHERE superseded_by IS NOT NULL) FROM claims WHERE project_id = $1`, proj.ID).
			Scan(&claimTotal, &claimSuperseded); err != nil {
			t.Fatalf("%s: count claims: %v", stage, err)
		}
		if claimTotal != 1 || claimSuperseded != 0 {
			t.Errorf("%s: claims = %d (superseded %d), want 1 active, 0 superseded (no broken chain)", stage, claimTotal, claimSuperseded)
		}
		if err := st.Pool.QueryRow(ctx,
			`SELECT count(*) FROM memory_versions WHERE project_id = $1`, proj.ID).Scan(&versionRows); err != nil {
			t.Fatalf("%s: count memory_versions: %v", stage, err)
		}
		if versionRows != 0 {
			t.Errorf("%s: memory_versions rows = %d, want 0 (no merge happened, the loser rolled back)", stage, versionRows)
		}
		var covered int64
		if err := st.Pool.QueryRow(ctx, `SELECT covered_seq FROM runs WHERE id = $1`, run.ID).Scan(&covered); err != nil {
			t.Fatalf("%s: read covered_seq: %v", stage, err)
		}
		if covered != 1 {
			t.Errorf("%s: covered_seq = %d, want 1 (advanced once)", stage, covered)
		}
	}
	assertSingleSet("after concurrent double-delivery")

	// Post-commit retry: replaying the same window (expected 0, but the checkpoint is now 1) is a clean
	// no-op — the compare-and-swap matches no row, so nothing is written and nothing double-advances.
	if err := p.Persist(ctx, mkInput()); err == nil {
		t.Error("a post-commit retry of the same window should conflict, not re-apply")
	}
	assertSingleSet("after post-commit retry")
}

// TestPGPersisterCrashRedoSingleSet proves a pass that fails partway leaves nothing behind (the whole
// transaction rolls back) and that redoing it writes exactly one set: the checkpoint advances only with
// a committed pass, so a mid-pass crash never half-writes and its redo is exactly-once.
func TestPGPersisterCrashRedoSingleSet(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, run := seedProjectRun(ctx, t, st)
	p := jobs.NewPGPersister(st, ext.LWW{})

	// A pass whose second memory has an invalid kind fails at that insert, after the first memory was
	// written — standing in for a crash partway through the transaction.
	crashy := jobs.PersistInput{
		ProjectID:          proj.ID,
		RunID:              run.ID,
		ExpectedCoveredSeq: 0,
		CoveredSeq:         1,
		Memories: []jobs.MemoryWrite{
			{Kind: "semantic", Content: "good", CreatedByAgent: "a", SourceSeq: 1},
			{Kind: "not-a-valid-kind", Content: "bad", CreatedByAgent: "a", SourceSeq: 1},
		},
	}
	if err := p.Persist(ctx, crashy); err == nil {
		t.Fatal("a pass with an invalid memory kind should fail")
	}

	assertCounts := func(stage string, wantMem, wantCovered int64) {
		t.Helper()
		var mem, covered int64
		if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM memories WHERE project_id = $1`, proj.ID).Scan(&mem); err != nil {
			t.Fatalf("%s: count memories: %v", stage, err)
		}
		if err := st.Pool.QueryRow(ctx, `SELECT covered_seq FROM runs WHERE id = $1`, run.ID).Scan(&covered); err != nil {
			t.Fatalf("%s: read covered_seq: %v", stage, err)
		}
		if mem != wantMem || covered != wantCovered {
			t.Errorf("%s: memories=%d covered_seq=%d, want memories=%d covered_seq=%d", stage, mem, covered, wantMem, wantCovered)
		}
	}
	// The failed pass wrote nothing and did not advance the checkpoint.
	assertCounts("after crash", 0, 0)

	// Redo with a clean window: exactly one memory, checkpoint advanced once.
	clean := jobs.PersistInput{
		ProjectID:          proj.ID,
		RunID:              run.ID,
		ExpectedCoveredSeq: 0,
		CoveredSeq:         1,
		Memories:           []jobs.MemoryWrite{{Kind: "semantic", Content: "good", CreatedByAgent: "a", SourceSeq: 1}},
	}
	if err := p.Persist(ctx, clean); err != nil {
		t.Fatalf("redo persist: %v", err)
	}
	assertCounts("after redo", 1, 1)
}
