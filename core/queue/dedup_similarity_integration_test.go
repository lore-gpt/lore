//go:build integration

package queue_test

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/jobs"
	"github.com/lore-gpt/lore/core/store/db"
)

// persistLog is the merge-funnel tally one "consolidation pass persisted" log line reports.
type persistLog struct{ inserted, exactMerged, nearMerged, grayZone int64 }

// logCapture is an slog.Handler that records the funnel tally from each consolidation pass, so a test can
// assert grey-band telemetry that has no DB-observable effect (a grey insert and a plain insert are
// otherwise identical rows).
type logCapture struct {
	mu   sync.Mutex
	logs []persistLog
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	if r.Message != "consolidation pass persisted" {
		return nil
	}
	var p persistLog
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "memories_inserted":
			p.inserted = a.Value.Int64()
		case "memories_exact_merged":
			p.exactMerged = a.Value.Int64()
		case "memories_near_merged":
			p.nearMerged = a.Value.Int64()
		case "memories_gray_zone":
			p.grayZone = a.Value.Int64()
		}
		return true
	})
	c.mu.Lock()
	c.logs = append(c.logs, p)
	c.mu.Unlock()
	return nil
}
func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

// captureConsolidationLogs redirects the default logger to a capture for the duration of the test.
func captureConsolidationLogs(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(c))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return c
}

// scriptedEmbedder returns a caller-chosen vector per content, so a test can engineer exact cosine
// distances between memories. The FixtureEmbedder hashes its input, so it cannot produce a controlled
// near-duplicate pair (two distinct texts with a high cosine); this can.
type scriptedEmbedder struct {
	dim     int
	vectors map[string][]float32
}

func (s scriptedEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, ok := s.vectors[t]
		if !ok {
			return nil, fmt.Errorf("scriptedEmbedder: no vector configured for %q", t)
		}
		out[i] = v
	}
	return out, nil
}
func (s scriptedEmbedder) Dim() int      { return s.dim }
func (scriptedEmbedder) ModelID() string { return "scripted-test" }

// TestPGPersisterNearDuplicateSupersedes proves the near-duplicate merge: a candidate whose embedding is
// above the merge threshold to a live memory in the same entity bucket does NOT insert a second row —
// the incoming memory supersedes the stored one (last-write-wins). The live content, fingerprint,
// provenance, and embedding become the incoming memory's; the OLD content is preserved in memory_versions
// with a reason naming the cosine score; and a same-event claim links to the surviving memory.
func TestPGPersisterNearDuplicateSupersedes(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, run := seedProjectRun(ctx, t, st)
	q := db.New(st.Pool)

	const oldContent = "auth login is broken"
	const newContent = "auth sign-in is failing"
	emb := scriptedEmbedder{dim: 4, vectors: map[string][]float32{
		oldContent: {1, 0, 0, 0},
		newContent: {0.98, 0.199, 0, 0}, // cosine ~0.98 with the first → distance ~0.02, a near duplicate
	}}
	p := jobs.NewPGPersister(st, ext.LWW{}, emb)

	ev1, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "planner", Payload: []byte(`{"m":1}`)})
	if err != nil {
		t.Fatalf("insert event 1: %v", err)
	}
	ev2, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "tester", Payload: []byte(`{"m":2}`)})
	if err != nil {
		t.Fatalf("insert event 2: %v", err)
	}

	// Pass 1: the original memory about "auth".
	if err := p.Persist(ctx, jobs.PersistInput{
		ProjectID: proj.ID, RunID: run.ID, ExpectedCoveredSeq: 0, CoveredSeq: ev1.Seq,
		Memories: []jobs.MemoryWrite{{Kind: "semantic", Content: oldContent, CreatedByAgent: "planner", SourceEventID: ev1.ID, SourceSeq: ev1.Seq}},
		Entities: []jobs.EntityWrite{{Name: "auth", Type: "service"}},
	}); err != nil {
		t.Fatalf("persist pass 1: %v", err)
	}

	// Pass 2: a near-duplicate restatement, same entity context, plus a claim from the same event.
	if err := p.Persist(ctx, jobs.PersistInput{
		ProjectID: proj.ID, RunID: run.ID, ExpectedCoveredSeq: ev1.Seq, CoveredSeq: ev2.Seq,
		Memories: []jobs.MemoryWrite{{Kind: "semantic", Content: newContent, CreatedByAgent: "tester", SourceEventID: ev2.ID, SourceSeq: ev2.Seq}},
		Entities: []jobs.EntityWrite{{Name: "auth", Type: "service"}},
		Claims:   []jobs.ClaimWrite{{Entity: "auth", Predicate: "state", Value: []byte(`"failing"`), SourceEventID: ev2.ID, SourceSeq: ev2.Seq}},
	}); err != nil {
		t.Fatalf("persist pass 2: %v", err)
	}

	// Exactly one memory: the near-duplicate merged into the existing row.
	var count int64
	var memID, srcEvent pgtype.UUID
	var content string
	var version int
	var agent *string
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) OVER (), id, content, version, source_event_id, created_by_agent FROM memories WHERE project_id = $1 LIMIT 1`,
		proj.ID).Scan(&count, &memID, &content, &version, &srcEvent, &agent); err != nil {
		t.Fatalf("read memory: %v", err)
	}
	if count != 1 {
		t.Fatalf("memories = %d, want 1 (the near duplicate merged, not a second row)", count)
	}
	if content != newContent {
		t.Errorf("live content = %q, want the incoming (superseding) content %q", content, newContent)
	}
	if version != 2 {
		t.Errorf("version = %d, want 2 (the near-merge bumped it)", version)
	}
	if srcEvent != ev2.ID {
		t.Error("source_event_id did not move to the incoming event")
	}
	if agent == nil || *agent != "tester" {
		t.Errorf("created_by_agent = %v, want the incoming agent tester", agent)
	}

	// The OLD content is preserved in memory_versions with a reason naming the near-duplicate + cosine.
	var vContent string
	var vReason *string
	if err := st.Pool.QueryRow(ctx,
		`SELECT content, reason FROM memory_versions WHERE project_id = $1 AND memory_id = $2`, proj.ID, memID).
		Scan(&vContent, &vReason); err != nil {
		t.Fatalf("read memory_versions: %v", err)
	}
	if vContent != oldContent {
		t.Errorf("version snapshot content = %q, want the superseded OLD content %q", vContent, oldContent)
	}
	if vReason == nil || !strings.Contains(*vReason, "near-duplicate") || !strings.Contains(*vReason, "cosine") || !strings.Contains(*vReason, "superseded") {
		t.Errorf("reason = %v, want it to name the near-duplicate, its cosine, and the supersession", vReason)
	}

	// The embedding was re-stored as the INCOMING vector (first component ~0.98, not the old 1.0).
	e, err := q.GetEmbedding(ctx, db.GetEmbeddingParams{ProjectID: proj.ID, MemoryID: memID, ModelID: emb.ModelID()})
	if err != nil {
		t.Fatalf("read embedding: %v", err)
	}
	vec := e.Vec.Slice()
	if len(vec) != emb.Dim() {
		t.Fatalf("embedding dim = %d, want %d", len(vec), emb.Dim())
	}
	if math.Abs(float64(vec[0])-0.98) > 0.01 {
		t.Errorf("embedding[0] = %v, want ~0.98 (re-stored as the incoming vector, not the old 1.0)", vec[0])
	}

	// The same-event claim links to the surviving (merged) memory.
	var claimMemory pgtype.UUID
	if err := st.Pool.QueryRow(ctx, `SELECT memory_id FROM claims WHERE project_id = $1`, proj.ID).Scan(&claimMemory); err != nil {
		t.Fatalf("read claim: %v", err)
	}
	if claimMemory != memID {
		t.Errorf("claim.memory_id = %v, want the surviving memory %v", claimMemory, memID)
	}
}

// TestPGPersisterGreyZoneKeepsSeparate proves the grey band is not auto-merged AND is classified as
// telemetry only: a candidate in [greyLowerBound, mergeThreshold) stays a SEPARATE memory (no version bump,
// no version row) but its pass reports memories_gray_zone=1, while a candidate BELOW the grey band is an
// ordinary insert reporting memories_gray_zone=0. Because a grey insert and a plain insert are identical
// rows, the classification — and both grey boundaries — are pinned via the pass's structured log; a
// threshold mutant that collapses or widens the grey band would otherwise ship silently.
func TestPGPersisterGreyZoneKeepsSeparate(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, run := seedProjectRun(ctx, t, st)
	q := db.New(st.Pool)
	logs := captureConsolidationLogs(t)

	const first = "auth login is broken"
	const grey = "auth is occasionally slow"
	const below = "the deploy pipeline is green"
	emb := scriptedEmbedder{dim: 4, vectors: map[string][]float32{
		first: {1, 0, 0, 0},
		grey:  {0.88, 0.475, 0, 0}, // cosine ~0.88 → distance ~0.12, inside the grey band [0.85, 0.92)
		below: {0, 0, 1, 0},        // orthogonal to BOTH seeded members → distance ~1.0, below grey → an ordinary insert
	}}
	p := jobs.NewPGPersister(st, ext.LWW{}, emb)

	ev := func(i int) db.Event {
		e, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte(fmt.Sprintf(`{"m":%d}`, i))})
		if err != nil {
			t.Fatalf("insert event %d: %v", i, err)
		}
		return e
	}
	e1, e2, e3 := ev(1), ev(2), ev(3)

	pass := func(expected, covered int64, content string, e db.Event) {
		t.Helper()
		if err := p.Persist(ctx, jobs.PersistInput{
			ProjectID: proj.ID, RunID: run.ID, ExpectedCoveredSeq: expected, CoveredSeq: covered,
			Memories: []jobs.MemoryWrite{{Kind: "semantic", Content: content, CreatedByAgent: "a", SourceEventID: e.ID, SourceSeq: e.Seq}},
			Entities: []jobs.EntityWrite{{Name: "auth", Type: "service"}},
		}); err != nil {
			t.Fatalf("persist %q: %v", content, err)
		}
	}
	pass(0, e1.Seq, first, e1)      // insert
	pass(e1.Seq, e2.Seq, grey, e2)  // grey band → separate, telemetry only
	pass(e2.Seq, e3.Seq, below, e3) // below grey → ordinary insert

	// Three separate memories, none merged: no near-merge changed any row state.
	var memCount, maxVersion, versionRows int64
	if err := st.Pool.QueryRow(ctx, `SELECT count(*), coalesce(max(version),0) FROM memories WHERE project_id = $1`, proj.ID).
		Scan(&memCount, &maxVersion); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if memCount != 3 {
		t.Errorf("memories = %d, want 3 (grey-band and below-grey both stay separate in L1)", memCount)
	}
	if maxVersion != 1 {
		t.Errorf("max version = %d, want 1 (no near-merge bumped a neighbour)", maxVersion)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM memory_versions WHERE project_id = $1`, proj.ID).Scan(&versionRows); err != nil {
		t.Fatalf("count memory_versions: %v", err)
	}
	if versionRows != 0 {
		t.Errorf("memory_versions rows = %d, want 0 (neither grey nor below-grey changes a row)", versionRows)
	}

	// Classification pinned via per-pass funnel telemetry: grey → gray_zone=1 (no near-merge); below-grey →
	// gray_zone=0 (ordinary insert); the first pass → gray_zone=0, one insert.
	if len(logs.logs) != 3 {
		t.Fatalf("captured %d consolidation-pass logs, want 3", len(logs.logs))
	}
	if logs.logs[0].grayZone != 0 || logs.logs[0].inserted != 1 {
		t.Errorf("first insert telemetry = %+v, want gray_zone=0 inserted=1", logs.logs[0])
	}
	if logs.logs[1].grayZone != 1 || logs.logs[1].nearMerged != 0 {
		t.Errorf("grey pass telemetry = %+v, want gray_zone=1 near_merged=0", logs.logs[1])
	}
	if logs.logs[2].grayZone != 0 {
		t.Errorf("below-grey pass gray_zone = %d, want 0 (an ordinary insert, not grey)", logs.logs[2].grayZone)
	}
}

// TestPGPersisterNearMergePicksNearestInBucket proves stage 2 merges into the CLOSEST bucket member, not
// just any: with a distant and a near member both live in one entity bucket, a candidate near the second
// merges into THAT one and leaves the distant member untouched. A broken min-distance selection (e.g.
// ORDER BY distance DESC) would target the distant member — which is beyond the merge threshold, so it
// would insert a third memory instead, failing the count.
func TestPGPersisterNearMergePicksNearestInBucket(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, run := seedProjectRun(ctx, t, st)
	q := db.New(st.Pool)

	const distant = "auth is completely down"
	const near = "auth login is broken"
	const candidate = "auth sign-in is failing"
	emb := scriptedEmbedder{dim: 4, vectors: map[string][]float32{
		distant:   {0, 1, 0, 0},        // cosine ~0.199 to the candidate → far (distance ~0.80)
		near:      {1, 0, 0, 0},        // cosine ~0.98 to the candidate → the nearest
		candidate: {0.98, 0.199, 0, 0}, // merges into `near`, not `distant`
	}}
	p := jobs.NewPGPersister(st, ext.LWW{}, emb)

	ev := func(i int) db.Event {
		e, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte(fmt.Sprintf(`{"m":%d}`, i))})
		if err != nil {
			t.Fatalf("insert event %d: %v", i, err)
		}
		return e
	}
	e1, e2, e3 := ev(1), ev(2), ev(3)

	pass := func(expected, covered int64, content string, e db.Event) {
		t.Helper()
		if err := p.Persist(ctx, jobs.PersistInput{
			ProjectID: proj.ID, RunID: run.ID, ExpectedCoveredSeq: expected, CoveredSeq: covered,
			Memories: []jobs.MemoryWrite{{Kind: "semantic", Content: content, CreatedByAgent: "a", SourceEventID: e.ID, SourceSeq: e.Seq}},
			Entities: []jobs.EntityWrite{{Name: "auth", Type: "service"}},
		}); err != nil {
			t.Fatalf("persist %q: %v", content, err)
		}
	}
	pass(0, e1.Seq, distant, e1)        // seed the distant member
	pass(e1.Seq, e2.Seq, near, e2)      // seed the near member (far from distant → a separate insert)
	pass(e2.Seq, e3.Seq, candidate, e3) // near-merges into `near`

	// Two memories: the candidate merged into the NEAR member — not a third row, not the distant one.
	var memCount int64
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM memories WHERE project_id = $1`, proj.ID).Scan(&memCount); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if memCount != 2 {
		t.Fatalf("memories = %d, want 2 (candidate merged into the nearest member; a wrong selection would insert a third)", memCount)
	}
	// The near member now holds the candidate content at version 2; the distant member is untouched (v1).
	var nearVersion, distantVersion int
	if err := st.Pool.QueryRow(ctx, `SELECT version FROM memories WHERE project_id = $1 AND content = $2`, proj.ID, candidate).Scan(&nearVersion); err != nil {
		t.Fatalf("read near member (should now hold the candidate content): %v", err)
	}
	if nearVersion != 2 {
		t.Errorf("nearest member version = %d, want 2 (it absorbed the candidate)", nearVersion)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT version FROM memories WHERE project_id = $1 AND content = $2`, proj.ID, distant).Scan(&distantVersion); err != nil {
		t.Fatalf("read distant member (should be untouched): %v", err)
	}
	if distantVersion != 1 {
		t.Errorf("distant member version = %d, want 1 (untouched — it was not the nearest)", distantVersion)
	}
}

// TestPGPersisterVersionHistoryConvention pins the memory_versions convention across TWO consecutive
// near-merges: memory_versions[K] holds the content that was live at version K (retired by K+1) with the
// reason it was superseded, the live row is the highest version, and the first supersede creates the first
// history row. Two near-merges into one memory (A → B → C) must leave history [(1, A), (2, B)] and a live
// row (3, C).
func TestPGPersisterVersionHistoryConvention(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj, run := seedProjectRun(ctx, t, st)
	q := db.New(st.Pool)

	const cA = "auth login is broken"
	const cB = "auth sign-in is failing"
	const cC = "auth authentication is down"
	emb := scriptedEmbedder{dim: 4, vectors: map[string][]float32{
		cA: {1, 0, 0, 0},
		cB: {0.98, 0.199, 0, 0}, // near A
		cC: {0.96, 0.28, 0, 0},  // near B (and A) → each folds into the same row
	}}
	p := jobs.NewPGPersister(st, ext.LWW{}, emb)

	ev := func(i int) db.Event {
		e, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: run.ID, AgentID: "a", Payload: []byte(fmt.Sprintf(`{"m":%d}`, i))})
		if err != nil {
			t.Fatalf("insert event %d: %v", i, err)
		}
		return e
	}
	e1, e2, e3 := ev(1), ev(2), ev(3)
	pass := func(expected, covered int64, content string, e db.Event) {
		t.Helper()
		if err := p.Persist(ctx, jobs.PersistInput{
			ProjectID: proj.ID, RunID: run.ID, ExpectedCoveredSeq: expected, CoveredSeq: covered,
			Memories: []jobs.MemoryWrite{{Kind: "semantic", Content: content, CreatedByAgent: "a", SourceEventID: e.ID, SourceSeq: e.Seq}},
			Entities: []jobs.EntityWrite{{Name: "auth", Type: "service"}},
		}); err != nil {
			t.Fatalf("persist %q: %v", content, err)
		}
	}
	pass(0, e1.Seq, cA, e1)      // insert A (v1)
	pass(e1.Seq, e2.Seq, cB, e2) // near-merge B into A (v1→v2); history gains (1, A)
	pass(e2.Seq, e3.Seq, cC, e3) // near-merge C into A (v2→v3); history gains (2, B)

	// One live memory at version 3 holding the newest content.
	var memCount int64
	var liveContent string
	var liveVersion int
	var memID pgtype.UUID
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) OVER (), id, content, version FROM memories WHERE project_id = $1 LIMIT 1`, proj.ID).
		Scan(&memCount, &memID, &liveContent, &liveVersion); err != nil {
		t.Fatalf("read memory: %v", err)
	}
	if memCount != 1 {
		t.Fatalf("memories = %d, want 1 (both near-merges folded into one row)", memCount)
	}
	if liveContent != cC || liveVersion != 3 {
		t.Errorf("live = (v%d, %q), want (v3, %q)", liveVersion, liveContent, cC)
	}

	// History: version K holds the content that was live at version K, in order, each with a reason.
	rows, err := st.Pool.Query(ctx, `SELECT version, content, reason FROM memory_versions WHERE project_id = $1 AND memory_id = $2 ORDER BY version`, proj.ID, memID)
	if err != nil {
		t.Fatalf("read memory_versions: %v", err)
	}
	defer rows.Close()
	type ver struct {
		version int
		content string
		reason  *string
	}
	var history []ver
	for rows.Next() {
		var v ver
		if err := rows.Scan(&v.version, &v.content, &v.reason); err != nil {
			t.Fatalf("scan version: %v", err)
		}
		history = append(history, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate versions: %v", err)
	}
	want := []struct {
		version int
		content string
	}{{1, cA}, {2, cB}}
	if len(history) != len(want) {
		t.Fatalf("memory_versions = %d rows, want 2 (versions 1 and 2)", len(history))
	}
	for i, w := range want {
		if history[i].version != w.version || history[i].content != w.content {
			t.Errorf("history[%d] = (v%d, %q), want (v%d, %q)", i, history[i].version, history[i].content, w.version, w.content)
		}
		if history[i].reason == nil || *history[i].reason == "" {
			t.Errorf("history[%d] (v%d) has no reason; each superseded version must record why it was retired", i, history[i].version)
		}
	}
}
