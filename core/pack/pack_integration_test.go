//go:build integration

package pack

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/retrieval"
	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
)

const (
	paradeDBImage = "paradedb/paradedb:0.24.2-pg17"
	testModel     = "fixture-embed-v1@64"
)

// migratedStore starts a ParadeDB container, applies the store migrations, and returns an open store.
func migratedStore(ctx context.Context, t *testing.T) *store.Store {
	t.Helper()
	ctr, err := tcpostgres.Run(ctx, paradeDBImage,
		tcpostgres.WithDatabase("lore"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start paradedb: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(ctr) })
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	if err := store.RunMigrations(ctx, dsn); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	st, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// seedProject creates an org + project + partitions and sets its active model.
func seedProject(ctx context.Context, t *testing.T, st *store.Store, activeModel string) pgtype.UUID {
	t.Helper()
	q := db.New(st.Pool)
	org, err := q.InsertOrganization(ctx, "acme")
	if err != nil {
		t.Fatalf("insert org: %v", err)
	}
	proj, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "p"})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if err := store.CreateProjectPartitions(ctx, st.Pool, proj.ID); err != nil {
		t.Fatalf("create partitions: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE projects SET active_model_id = $2 WHERE id = $1`, proj.ID, activeModel); err != nil {
		t.Fatalf("set active model: %v", err)
	}
	return proj.ID
}

// insertMemKind inserts a live memory of the given kind (with its embedding under testModel) and returns its id.
func insertMemKind(ctx context.Context, t *testing.T, st *store.Store, projectID pgtype.UUID, kind, content string, scopes []string) pgtype.UUID {
	t.Helper()
	if scopes == nil {
		scopes = []string{}
	}
	var id pgtype.UUID
	if err := st.Pool.QueryRow(ctx,
		`INSERT INTO memories (project_id, kind, content, scope_keys, trust_tier) VALUES ($1,$2,$3,$4,'normal') RETURNING id`,
		projectID, kind, content, scopes).Scan(&id); err != nil {
		t.Fatalf("insert %s memory: %v", kind, err)
	}
	vecs, err := (ext.FixtureEmbedder{}).Embed(ctx, []string{content})
	if err != nil {
		t.Fatalf("embed %q: %v", content, err)
	}
	if _, err := db.New(st.Pool).UpsertEmbedding(ctx, db.UpsertEmbeddingParams{
		ProjectID: projectID, MemoryID: id, ModelID: testModel, Vec: pgvector.NewVector(vecs[0]),
	}); err != nil {
		t.Fatalf("upsert embedding %q: %v", content, err)
	}
	return id
}

// seedRun inserts a run and returns its id.
func seedRun(ctx context.Context, t *testing.T, st *store.Store, projectID pgtype.UUID) pgtype.UUID {
	t.Helper()
	run, err := db.New(st.Pool).InsertRun(ctx, projectID)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return run.ID
}

// insertEvent inserts an event (assigning its per-run seq) and returns the seq.
func insertEvent(ctx context.Context, t *testing.T, st *store.Store, runID pgtype.UUID, agentID, payload string) int64 {
	t.Helper()
	ev, err := db.New(st.Pool).InsertEvent(ctx, db.InsertEventParams{RunID: runID, AgentID: agentID, Payload: []byte(payload)})
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
	return ev.Seq
}

// insertWorkingFact seeds one working fact for a run. Production writes these through the ingest
// transaction; a pack test only needs the row, so it writes directly.
func insertWorkingFact(ctx context.Context, t *testing.T, st *store.Store, projectID, runID pgtype.UUID, entity, predicate, value string, seq int64) {
	t.Helper()
	if _, err := db.New(st.Pool).UpsertWorkingFact(ctx, db.UpsertWorkingFactParams{
		ProjectID: projectID, RunID: runID, Entity: entity, Predicate: predicate,
		Value: []byte(value), Seq: seq, AgentID: "a2",
	}); err != nil {
		t.Fatalf("insert working fact %s.%s: %v", entity, predicate, err)
	}
}

// setCovered advances a run's extraction checkpoint directly (the pack reads it; extraction owns it in prod).
func setCovered(ctx context.Context, t *testing.T, st *store.Store, runID pgtype.UUID, covered int64) {
	t.Helper()
	if _, err := st.Pool.Exec(ctx, `UPDATE runs SET covered_seq = $2 WHERE id = $1`, runID, covered); err != nil {
		t.Fatalf("set covered_seq: %v", err)
	}
}

// newTestHybrid builds a hybrid retriever over the fixture embedder (offline, deterministic).
func newTestHybrid() *retrieval.Hybrid {
	return retrieval.NewHybrid(retrieval.New(), ext.FixtureEmbedder{})
}

// runBuild builds a pack inside a tenant transaction.
func runBuild(ctx context.Context, t *testing.T, st *store.Store, p *Pack, projectID, runID pgtype.UUID, req Request) Result {
	t.Helper()
	var res Result
	if err := st.WithProject(ctx, projectID, func(tx pgx.Tx) error {
		var e error
		res, e = p.Build(ctx, tx, projectID, runID, req)
		return e
	}); err != nil {
		t.Fatalf("build pack: %v", err)
	}
	return res
}

// rawTailSeqs extracts the seq numbers of the rendered raw-tail lines in text order, so a test can assert the
// tail's ORDER (not merely each event's presence) — a reorder, reverse, or duplicate mutant fails an ordering
// assertion but survives a presence check.
func rawTailSeqs(text string) []int64 {
	const marker = "- [seq "
	var seqs []int64
	for _, line := range strings.Split(text, "\n") {
		i := strings.Index(line, marker)
		if i < 0 {
			continue
		}
		rest := line[i+len(marker):]
		if j := strings.IndexByte(rest, ' '); j >= 0 {
			if n, err := strconv.ParseInt(rest[:j], 10, 64); err == nil {
				seqs = append(seqs, n)
			}
		}
	}
	return seqs
}

// workingSeqs extracts the seq of each rendered working-section line in text order, so a test can assert the
// section's ORDER rather than mere presence — a reversed or missing sort survives a presence check.
func workingSeqs(text string) []int64 {
	const marker = "[run seq "
	var seqs []int64
	for _, line := range strings.Split(text, "\n") {
		i := strings.Index(line, marker)
		if i < 0 {
			continue
		}
		rest := line[i+len(marker):]
		if j := strings.IndexByte(rest, ' '); j >= 0 {
			if n, err := strconv.ParseInt(rest[:j], 10, 64); err == nil {
				seqs = append(seqs, n)
			}
		}
	}
	return seqs
}

func equalInt64(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPackAssemblesDistilledWorkingAndRawTail proves the read-your-writes core: a pack fuses distilled
// memories, the working section, and a raw tail of not-yet-distilled events — so a peer's write at a seq
// past the extraction checkpoint is visible (raw) even though it has not been distilled.
func TestPackAssemblesDistilledWorkingAndRawTail(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj := seedProject(ctx, t, st, testModel)
	run := seedRun(ctx, t, st, proj)

	semID := insertMemKind(ctx, t, st, proj, sectionSemantic, "the auth service uses bearer tokens", nil)
	insertMemKind(ctx, t, st, proj, sectionEpisodic, "the deploy service ran yesterday", nil)

	insertEvent(ctx, t, st, run, "a1", `{"k":1}`)
	insertEvent(ctx, t, st, run, "a1", `{"k":2}`)
	setCovered(ctx, t, st, run, 2)
	insertEvent(ctx, t, st, run, "a2", `{"note":"seq3 raw write"}`)
	s4 := insertEvent(ctx, t, st, run, "a2", `{"note":"seq4 raw write"}`)

	insertWorkingFact(ctx, t, st, proj, run, "task", "status", `"in_progress"`, s4)
	p := New(newTestHybrid())

	res := runBuild(ctx, t, st, p, proj, run, Request{Query: "service", MinSeq: s4, Limit: 10})

	if res.WorkingSource != workingLive {
		t.Errorf("WorkingSource = %q, want live", res.WorkingSource)
	}
	if res.CoveredSeq != 2 {
		t.Errorf("CoveredSeq = %d, want 2", res.CoveredSeq)
	}
	// The not-yet-distilled writes are visible in the raw tail.
	if !strings.Contains(res.Text, "seq3 raw write") || !strings.Contains(res.Text, "seq4 raw write") {
		t.Errorf("raw tail missing the uncovered writes:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "auth service uses bearer tokens") {
		t.Errorf("distilled semantic memory missing:\n%s", res.Text)
	}
	found := false
	for _, s := range res.Sources {
		if s.ID == semID {
			found = true
		}
	}
	if !found {
		t.Errorf("semantic memory not in sources: %+v", res.Sources)
	}
	if !strings.Contains(res.Text, `task.status = "in_progress"`) {
		t.Errorf("live working fact missing:\n%s", res.Text)
	}
}

// TestPackRawTailCapExemptWindow proves the guarantee at the heart of the read-your-writes contract: the window
// (covered_seq, min_seq] is ALWAYS included in full, exempt from the beyond-window cap. With many uncovered
// events, a small cap, and min_seq at the OLD end, the min_seq event must still be in the pack — an oldest-first
// cap would have silently dropped it and broken the contract.
func TestPackRawTailCapExemptWindow(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj := seedProject(ctx, t, st, testModel)
	run := seedRun(ctx, t, st, proj)

	for i := 0; i < 12; i++ {
		insertEvent(ctx, t, st, run, "a", `{"i":`+strconv.Itoa(i)+`}`)
	}
	// covered_seq stays 0: all 12 events are uncovered.

	p := New(newTestHybrid(), WithRawTailMax(5))
	res := runBuild(ctx, t, st, p, proj, run, Request{Query: "none", MinSeq: 3, Limit: 10})

	// The (0,3] window is cap-exempt: seq 1,2,3 must all be present despite the cap of 5 over 12 uncovered.
	for _, seq := range []int64{1, 2, 3} {
		if !strings.Contains(res.Text, "[seq "+strconv.FormatInt(seq, 10)+" ·") {
			t.Errorf("guaranteed-window event seq %d missing (the cap broke read-your-writes):\n%s", seq, res.Text)
		}
	}
	// The newest events past the window are present (newest-first cap keeps 8..12).
	if !strings.Contains(res.Text, "[seq 12 ·") || !strings.Contains(res.Text, "[seq 8 ·") {
		t.Errorf("newest uncovered events missing:\n%s", res.Text)
	}
	// A middle event past the window but outside the newest-5 (seq 5) is dropped.
	if strings.Contains(res.Text, "[seq 5 ·") {
		t.Errorf("seq 5 should have been dropped by the beyond-window cap:\n%s", res.Text)
	}
	if !res.Truncated {
		t.Errorf("Truncated = false, want true (the cap dropped uncovered events)")
	}
	// Pin the exact tail: the cap-exempt window {1,2,3} then the newest-5 {8..12}, strictly ascending, with
	// 4..7 dropped. This one assertion pins ordering, window inclusion, the cap, and the exclusion together —
	// a reorder/reverse or a dropped-window-event mutant fails here.
	if got, want := rawTailSeqs(res.Text), []int64{1, 2, 3, 8, 9, 10, 11, 12}; !equalInt64(got, want) {
		t.Errorf("raw tail seqs = %v, want %v (ascending; cap-exempt window then newest-5)", got, want)
	}
}

// TestPackWorkingSectionOutlivesTheCheckpoint is the reason this whole lane exists.
//
// A working fact used to be visible only twice: from a cache, and from the raw tail until extraction
// distilled past it. Once the checkpoint advanced, a deployment without a cache had no working section at
// all — the fact was still stored, but not on the surface an agent reads. Here the checkpoint is advanced
// past every event, so the raw tail is empty, and the section must still be there.
//
// The stray working-kind memory is the control: the pack used to build its "durable" working section from
// those, and nothing has ever written one. It must not be rendered as a working section, and must not become
// a source, because working facts have their own home now.
func TestPackWorkingSectionOutlivesTheCheckpoint(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj := seedProject(ctx, t, st, testModel)
	run := seedRun(ctx, t, st, proj)

	strayID := insertMemKind(ctx, t, st, proj, sectionWorking, "task status is stray_working_memory", nil)

	seq := insertEvent(ctx, t, st, run, "a2", `{"kind":"state","entity":"task","predicate":"status","value":"shipped"}`)
	insertWorkingFact(ctx, t, st, proj, run, "task", "status", `"shipped"`, seq)
	setCovered(ctx, t, st, run, seq) // distilled: the event has left the raw tail

	res := runBuild(ctx, t, st, New(newTestHybrid()), proj, run, Request{Query: "status", Limit: 10})

	if len(rawTailSeqs(res.Text)) != 0 {
		t.Fatalf("raw tail should be empty at a caught-up checkpoint, got %v — the assertion below would be vacuous", rawTailSeqs(res.Text))
	}
	if !strings.Contains(res.Text, `task.status = "shipped"`) {
		t.Errorf("working fact missing after the checkpoint passed it:\n%s", res.Text)
	}
	if res.WorkingSource != workingLive {
		t.Errorf("WorkingSource = %q, want %q", res.WorkingSource, workingLive)
	}

	// The control: a working-kind memory is not the working section and is not a source.
	if strings.Contains(res.Text, "stray_working_memory") {
		t.Errorf("a working-kind memory must not render as the working section:\n%s", res.Text)
	}
	for _, s := range res.Sources {
		if s.ID == strayID || s.Section == sectionWorking {
			t.Errorf("a working-kind memory reached the source list: %+v", s)
		}
	}
}

// TestPackWorkingSectionIsRunScoped proves the property that decided the design. Two runs in ONE project
// hold different values for the same subject; each pack must show its own run's value.
//
// A project-scoped working store would make one run's write hide the other's, and the loser would have no way
// to see its own fact once the checkpoint moved past the event. That is exactly why the working section is not
// built from the project-scoped claims table.
func TestPackWorkingSectionIsRunScoped(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj := seedProject(ctx, t, st, testModel)
	runA, runB := seedRun(ctx, t, st, proj), seedRun(ctx, t, st, proj)

	insertWorkingFact(ctx, t, st, proj, runA, "task", "owner", `"alice"`, 1)
	insertWorkingFact(ctx, t, st, proj, runB, "task", "owner", `"bob"`, 1)

	p := New(newTestHybrid())
	a := runBuild(ctx, t, st, p, proj, runA, Request{Query: "owner", Limit: 10})
	b := runBuild(ctx, t, st, p, proj, runB, Request{Query: "owner", Limit: 10})

	if !strings.Contains(a.Text, `"alice"`) || strings.Contains(a.Text, `"bob"`) {
		t.Errorf("run A must see only its own value:\n%s", a.Text)
	}
	if !strings.Contains(b.Text, `"bob"`) || strings.Contains(b.Text, `"alice"`) {
		t.Errorf("run B must see only its own value:\n%s", b.Text)
	}
}

// TestPackWorkingSectionOrderedFreshestFirst pins the render order as an exact sequence. Insertion order and
// seq order are deliberately different, so a missing or reversed sort cannot pass, and a presence-only check
// would not have caught either.
func TestPackWorkingSectionOrderedFreshestFirst(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj := seedProject(ctx, t, st, testModel)
	run := seedRun(ctx, t, st, proj)

	insertWorkingFact(ctx, t, st, proj, run, "a", "p", `"first"`, 1)
	insertWorkingFact(ctx, t, st, proj, run, "b", "p", `"newest"`, 5)
	insertWorkingFact(ctx, t, st, proj, run, "c", "p", `"middle"`, 3)

	res := runBuild(ctx, t, st, New(newTestHybrid()), proj, run, Request{Query: "p", Limit: 10})

	if got, want := workingSeqs(res.Text), []int64{5, 3, 1}; !equalInt64(got, want) {
		t.Errorf("working section seqs = %v, want %v (freshest first)", got, want)
	}
}

// TestPackFreshnessAndCaughtUp proves the single freshness definition: zero when the run is fully caught up (no
// raw tail), positive once an uncovered event has waited (with a raw tail present).
func TestPackFreshnessAndCaughtUp(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj := seedProject(ctx, t, st, testModel)
	run := seedRun(ctx, t, st, proj)

	insertEvent(ctx, t, st, run, "a", `{}`)
	s2 := insertEvent(ctx, t, st, run, "a", `{}`)
	setCovered(ctx, t, st, run, s2)

	p := New(newTestHybrid())
	caught := runBuild(ctx, t, st, p, proj, run, Request{Query: "x", MinSeq: s2, Limit: 10})
	if caught.FreshnessLagMs != 0 {
		t.Errorf("caught-up freshness = %d, want 0", caught.FreshnessLagMs)
	}
	if strings.Contains(caught.Text, "## Recent activity") {
		t.Errorf("caught-up pack must have no raw tail:\n%s", caught.Text)
	}

	insertEvent(ctx, t, st, run, "a", `{"late":true}`)
	time.Sleep(25 * time.Millisecond)
	stale := runBuild(ctx, t, st, p, proj, run, Request{Query: "x", MinSeq: s2, Limit: 10})
	if stale.FreshnessLagMs <= 0 {
		t.Errorf("stale freshness = %d, want > 0", stale.FreshnessLagMs)
	}
	if !strings.Contains(stale.Text, "## Recent activity") {
		t.Errorf("stale pack must include the raw tail:\n%s", stale.Text)
	}
}

// TestPackLogWrittenInTransaction proves the trace is written on the pack's own transaction: a committed build
// leaves exactly one row whose memory_ids match the pack's source order and whose tokens_saved/pack_hash are
// NULL (L1), and a build whose transaction rolls back leaves NO row (the trace is atomic with the reads).
func TestPackLogWrittenInTransaction(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj := seedProject(ctx, t, st, testModel)
	run := seedRun(ctx, t, st, proj)
	insertMemKind(ctx, t, st, proj, sectionSemantic, "auth service tokens", nil)
	insertMemKind(ctx, t, st, proj, sectionEpisodic, "service deploy log", nil)
	// A working fact alongside them: it is rendered but is not a memory, so it must appear in neither the
	// source list nor the trace. The section check over res.Sources is what enforces the first; the trace
	// follows because memory_ids is written from that same list, and the count/order assertions below pin
	// that correspondence.
	insertWorkingFact(ctx, t, st, proj, run, "service", "status", `"green"`, 1)

	p := New(newTestHybrid())
	res := runBuild(ctx, t, st, p, proj, run, Request{Query: "service", Limit: 10})

	if len(res.Sources) < 2 {
		t.Fatalf("precondition: want the distilled memories as sources, got %+v", res.Sources)
	}
	if !strings.Contains(res.Text, `service.status = "green"`) {
		t.Fatalf("precondition: the working fact must be rendered, else the trace assertion is vacuous:\n%s", res.Text)
	}
	for _, s := range res.Sources {
		if s.Section == sectionWorking {
			t.Errorf("a working entry reached the source list, and therefore the trace: %+v", s)
		}
	}

	var query string
	var coveredSeq int64
	var memIDs []pgtype.UUID
	var tokensSaved, packedTokens, estSource *int32
	var packHash []byte
	if err := st.Pool.QueryRow(ctx,
		`SELECT query, covered_seq, memory_ids, tokens_saved, packed_tokens, est_source_tokens, pack_hash
		 FROM pack_logs WHERE run_id = $1`, run).
		Scan(&query, &coveredSeq, &memIDs, &tokensSaved, &packedTokens, &estSource, &packHash); err != nil {
		t.Fatalf("read pack_logs: %v", err)
	}
	if query != "service" || coveredSeq != res.CoveredSeq {
		t.Errorf("trace fields wrong: query=%q covered=%d (pack covered=%d)", query, coveredSeq, res.CoveredSeq)
	}
	if len(memIDs) != len(res.Sources) {
		t.Fatalf("memory_ids count %d != sources %d", len(memIDs), len(res.Sources))
	}
	for i := range memIDs {
		if memIDs[i] != res.Sources[i].ID {
			t.Errorf("memory_ids[%d] != sources[%d] — trace order must equal pack order", i, i)
		}
	}
	if tokensSaved != nil {
		t.Errorf("tokens_saved = %d, want NULL in L1", *tokensSaved)
	}
	if packHash != nil {
		t.Errorf("pack_hash = %v, want NULL in L1", packHash)
	}
	if packedTokens == nil || estSource == nil {
		t.Errorf("raw token ingredients must be written (packed=%v est_source=%v)", packedTokens, estSource)
	}

	// Reverse direction: a rolled-back build leaves no trace row (the insert is inside the pack transaction).
	run2 := seedRun(ctx, t, st, proj)
	sentinel := errors.New("force rollback")
	err := st.WithProject(ctx, proj, func(tx pgx.Tx) error {
		if _, e := p.Build(ctx, tx, proj, run2, Request{Query: "service", Limit: 10}); e != nil {
			return e
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
	var n int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM pack_logs WHERE run_id = $1`, run2).Scan(&n); err != nil {
		t.Fatalf("count pack_logs: %v", err)
	}
	if n != 0 {
		t.Errorf("pack_logs row persisted despite rollback: %d (the trace insert is not in the pack transaction)", n)
	}
}

// TestPackDeterministicBytes proves the L1 determinism guarantee: on a caught-up run (freshness a constant 0,
// no raw tail) two builds of the same query over the same data produce byte-identical packs — the bucketing,
// per-section sort, and rendering are a stable function of the inputs despite Go's randomised map iteration.
func TestPackDeterministicBytes(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj := seedProject(ctx, t, st, testModel)
	run := seedRun(ctx, t, st, proj)

	insertMemKind(ctx, t, st, proj, sectionSemantic, "auth service alpha", nil)
	insertMemKind(ctx, t, st, proj, sectionSemantic, "auth service beta", nil)
	insertMemKind(ctx, t, st, proj, sectionEpisodic, "service episode gamma", nil)
	insertMemKind(ctx, t, st, proj, sectionProcedural, "service procedure delta", nil)
	// Two working facts, so the working section is part of what is being pinned rather than absent. Note this
	// test only proves two builds agree; the ORDER within the section is pinned by
	// TestPackWorkingSectionOrderedFreshestFirst and, for the tie-break, by the unit test on sortEntries.
	insertWorkingFact(ctx, t, st, proj, run, "service", "owner", `"alice"`, 1)
	insertWorkingFact(ctx, t, st, proj, run, "auth", "owner", `"bob"`, 1)

	p := New(newTestHybrid())
	a := runBuild(ctx, t, st, p, proj, run, Request{Query: "service", Limit: 10})
	b := runBuild(ctx, t, st, p, proj, run, Request{Query: "service", Limit: 10})
	if a.Text != b.Text {
		t.Errorf("pack is not byte-deterministic across builds:\n--- a ---\n%s\n--- b ---\n%s", a.Text, b.Text)
	}
	if a.FreshnessLagMs != 0 {
		t.Errorf("expected caught-up freshness 0, got %d", a.FreshnessLagMs)
	}
	if !strings.Contains(a.Text, "## Working memory") {
		t.Errorf("precondition: the working section must be part of what is being pinned:\n%s", a.Text)
	}
}

// TestPackBudgetExemptsWorkingAndRawTail proves the coarse token budget governs ONLY the distilled recall: a
// tiny budget drops the distilled memory (reported truncated, no distilled source), but the live working
// section and the raw tail — read-your-writes correctness content — are rendered in full regardless.
func TestPackBudgetExemptsWorkingAndRawTail(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj := seedProject(ctx, t, st, testModel)
	run := seedRun(ctx, t, st, proj)

	// A large distilled memory (~65 tokens) that a 1-token budget cannot fit.
	big := "the auth service " + strings.Repeat("token ", 40)
	insertMemKind(ctx, t, st, proj, sectionSemantic, big, nil)

	// An uncovered event (raw tail, covered_seq stays 0) and a working fact.
	insertEvent(ctx, t, st, run, "a", `{"note":"uncovered_raw_event"}`)
	insertWorkingFact(ctx, t, st, proj, run, "task", "status", `"live_kept"`, 1)

	p := New(newTestHybrid())
	res := runBuild(ctx, t, st, p, proj, run, Request{Query: "service", MinSeq: 1, Limit: 10, TokenBudget: 1})

	// The distilled memory is dropped by the budget (reported truncated, no distilled source, no rendered line).
	if !res.Truncated {
		t.Errorf("Truncated = false, want true (the budget dropped the distilled memory)")
	}
	if len(res.Sources) != 0 {
		t.Errorf("distilled sources = %d, want 0 (the budget dropped the only memory): %+v", len(res.Sources), res.Sources)
	}
	if strings.Contains(res.Text, "relevance") {
		t.Errorf("a distilled memory line survived the budget:\n%s", res.Text)
	}
	// The live working fact and the raw tail are budget-exempt and fully present.
	if !strings.Contains(res.Text, `"live_kept"`) {
		t.Errorf("live working fact must be budget-exempt but is missing:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "uncovered_raw_event") {
		t.Errorf("raw tail must be budget-exempt but is missing:\n%s", res.Text)
	}
}
