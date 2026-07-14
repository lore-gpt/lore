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
	"github.com/lore-gpt/lore/core/workmem"
)

const (
	paradeDBImage = "paradedb/paradedb:0.24.2-pg17"
	testModel     = "fixture-embed-v1"
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

// fakeWorkmem is a working store whose GetAll fails while it reports a mode, so the "healthy store, failed read"
// fallback can be exercised.
type fakeWorkmem struct {
	mode workmem.Mode
	err  error
}

func (fakeWorkmem) Set(context.Context, workmem.Key, workmem.Value) error { return nil }
func (fakeWorkmem) Get(context.Context, workmem.Key) (workmem.Value, bool, error) {
	return workmem.Value{}, false, nil
}
func (f fakeWorkmem) GetAll(context.Context, string, string) ([]workmem.Entry, error) {
	return nil, f.err
}
func (f fakeWorkmem) Mode() workmem.Mode { return f.mode }
func (fakeWorkmem) Close()               {}

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

// TestPackAssemblesDistilledWorkingAndRawTail proves the read-your-writes core (AC#1): a pack fuses distilled
// memories, the live working section, and a raw tail of not-yet-distilled events — so a peer's write at a seq
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

	wm := workmem.NewMemory()
	if err := wm.Set(ctx, workmem.Key{ProjectID: uuidStr(proj), RunID: uuidStr(run), Entity: "task", Predicate: "status"},
		workmem.Value{Value: []byte(`"in_progress"`), Seq: s4, Agent: "a2"}); err != nil {
		t.Fatalf("workmem set: %v", err)
	}
	p := New(newTestHybrid(), wm)

	res := runBuild(ctx, t, st, p, proj, run, Request{Query: "service", MinSeq: s4, Limit: 10})

	if res.WorkingSource != workingLive {
		t.Errorf("WorkingSource = %q, want live", res.WorkingSource)
	}
	if res.CoveredSeq != 2 {
		t.Errorf("CoveredSeq = %d, want 2", res.CoveredSeq)
	}
	// AC#1: the not-yet-distilled writes are visible in the raw tail.
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
	if strings.Contains(res.Text, "durable snapshot") {
		t.Errorf("live mode must not show a durable snapshot:\n%s", res.Text)
	}
}

// TestPackRawTailCapExemptWindow proves the guarantee at the heart of D1: the read-your-writes window
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

	p := New(newTestHybrid(), workmem.NewDisabled(), WithRawTailMax(5))
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

// TestPackModeAwareWorking proves the mode-aware working section: a Healthy live store owns it and the durable
// working memory is dropped; a Disabled store falls back to the durable working memory (info is not lost); a
// Healthy store whose read fails falls back to durable too, counted as "skipped".
func TestPackModeAwareWorking(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(ctx, t)
	proj := seedProject(ctx, t, st, testModel)
	run := seedRun(ctx, t, st, proj)

	durID := insertMemKind(ctx, t, st, proj, sectionWorking, "task status is durable_snapshot_value", nil)

	// (a) Healthy live: the live fact wins and the durable working memory is dropped entirely.
	wm := workmem.NewMemory()
	if err := wm.Set(ctx, workmem.Key{ProjectID: uuidStr(proj), RunID: uuidStr(run), Entity: "task", Predicate: "status"},
		workmem.Value{Value: []byte(`"live_value"`), Seq: 1, Agent: "a"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	live := runBuild(ctx, t, st, New(newTestHybrid(), wm), proj, run, Request{Query: "status", Limit: 10})
	if live.WorkingSource != workingLive {
		t.Errorf("live: WorkingSource = %q, want live", live.WorkingSource)
	}
	if !strings.Contains(live.Text, `"live_value"`) {
		t.Errorf("live fact missing:\n%s", live.Text)
	}
	if strings.Contains(live.Text, "durable_snapshot_value") {
		t.Errorf("Healthy live mode must DROP the durable working memory, but its content appears:\n%s", live.Text)
	}
	for _, s := range live.Sources {
		if s.ID == durID {
			t.Errorf("Healthy live mode must not list the durable working memory as a source")
		}
	}

	// (b) Disabled: the durable working memory becomes the working section and a source.
	dis := runBuild(ctx, t, st, New(newTestHybrid(), workmem.NewDisabled()), proj, run, Request{Query: "status", Limit: 10})
	if dis.WorkingSource != workingDurable {
		t.Errorf("disabled: WorkingSource = %q, want durable", dis.WorkingSource)
	}
	if !strings.Contains(dis.Text, "durable snapshot") || !strings.Contains(dis.Text, "durable_snapshot_value") {
		t.Errorf("disabled mode must show the durable working memory:\n%s", dis.Text)
	}
	foundDur := false
	for _, s := range dis.Sources {
		if s.ID == durID {
			foundDur = true
		}
	}
	if !foundDur {
		t.Errorf("disabled: durable working memory must be a source: %+v", dis.Sources)
	}

	// (c) Healthy store whose GetAll fails: falls back to durable, counted as skipped.
	skip := runBuild(ctx, t, st, New(newTestHybrid(), fakeWorkmem{mode: workmem.Healthy, err: errors.New("cache boom")}),
		proj, run, Request{Query: "status", Limit: 10})
	if skip.WorkingSource != workingSkipped {
		t.Errorf("skipped: WorkingSource = %q, want skipped", skip.WorkingSource)
	}
	if !strings.Contains(skip.Text, "durable snapshot") {
		t.Errorf("skipped mode must fall back to the durable snapshot:\n%s", skip.Text)
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

	p := New(newTestHybrid(), workmem.NewDisabled())
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
	// A durable working memory plus distilled memories: in Disabled mode the working section precedes the
	// distilled sections, so the first source — and thus memory_ids[0] — must be the working memory.
	workID := insertMemKind(ctx, t, st, proj, sectionWorking, "service status snapshot", nil)
	insertMemKind(ctx, t, st, proj, sectionSemantic, "auth service tokens", nil)
	insertMemKind(ctx, t, st, proj, sectionEpisodic, "service deploy log", nil)

	p := New(newTestHybrid(), workmem.NewDisabled())
	res := runBuild(ctx, t, st, p, proj, run, Request{Query: "service", Limit: 10})

	if len(res.Sources) < 2 || res.Sources[0].ID != workID {
		t.Fatalf("durable working memory must be the FIRST source (working section precedes distilled); got %+v", res.Sources)
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

	p := New(newTestHybrid(), workmem.NewDisabled())
	a := runBuild(ctx, t, st, p, proj, run, Request{Query: "service", Limit: 10})
	b := runBuild(ctx, t, st, p, proj, run, Request{Query: "service", Limit: 10})
	if a.Text != b.Text {
		t.Errorf("pack is not byte-deterministic across builds:\n--- a ---\n%s\n--- b ---\n%s", a.Text, b.Text)
	}
	if a.FreshnessLagMs != 0 {
		t.Errorf("expected caught-up freshness 0, got %d", a.FreshnessLagMs)
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

	// An uncovered event (raw tail, covered_seq stays 0) and a live working fact.
	insertEvent(ctx, t, st, run, "a", `{"note":"uncovered_raw_event"}`)
	wm := workmem.NewMemory()
	if err := wm.Set(ctx, workmem.Key{ProjectID: uuidStr(proj), RunID: uuidStr(run), Entity: "task", Predicate: "status"},
		workmem.Value{Value: []byte(`"live_kept"`), Seq: 1, Agent: "a"}); err != nil {
		t.Fatalf("set: %v", err)
	}

	p := New(newTestHybrid(), wm)
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
