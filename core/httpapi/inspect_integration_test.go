//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/lore-gpt/lore/core/httpapi"
	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
)

// TestInspectEndpoints drives the memory/run inspection surface against a real ParadeDB: browse + filters +
// keyset pagination, lexical search (with NO active model — zero embedding), get, version history, soft-delete
// (tombstone + audit row + idempotent 404 + drop from reads), run trace over pack_logs, cross-tenant 404s on
// every route, and the still-stubbed create/patch/policy ops at 501.
func TestInspectEndpoints(t *testing.T) {
	ctx := context.Background()
	st := inspectStore(ctx, t)
	handler := httpapi.New(httpapi.Config{Pool: st.Pool, DB: st, Tenant: st, Version: "test"}).Handler()

	projA, runA := seedProjectRun(ctx, t, st.Pool)
	seedPartitions(ctx, t, st, projA)
	keyA, _ := provisionKey(ctx, t, st.Pool, projA)

	// One event in runA so a run-scoped memory can be distinguished from the un-linked ones.
	evA := seedEvent(ctx, t, st.Pool, runA, "planner")
	m1 := seedMemory(ctx, t, st.Pool, projA, "semantic", "the authentication service was deployed", "planner", evA)
	m2 := seedMemory(ctx, t, st.Pool, projA, "episodic", "the database migration finished cleanly", "worker", pgtype.UUID{})
	m3 := seedMemory(ctx, t, st.Pool, projA, "procedural", "restart the cache on a failure", "ops", pgtype.UUID{})

	// --- Browse: all three, has_more false. ---
	all := decodeList(t, getReq(handler, keyA, "/v1/memories?limit=10"))
	if len(all.Memories) != 3 || all.HasMore {
		t.Fatalf("browse all: got %d memories has_more=%v, want 3 / false", len(all.Memories), all.HasMore)
	}
	if !hasIDs(all.Memories, m1, m2, m3) {
		t.Errorf("browse all missing a seeded memory: %+v", memIDs(all.Memories))
	}

	// --- Keyset pagination: limit 2 → a next_cursor; the cursor page returns the rest with no overlap. ---
	p1 := decodeList(t, getReq(handler, keyA, "/v1/memories?limit=2"))
	if len(p1.Memories) != 2 || !p1.HasMore || p1.NextCursor == nil {
		t.Fatalf("page 1: got %d / has_more=%v / cursor=%v, want 2 / true / set", len(p1.Memories), p1.HasMore, p1.NextCursor)
	}
	p2 := decodeList(t, getReq(handler, keyA, "/v1/memories?limit=2&cursor="+*p1.NextCursor))
	if len(p2.Memories) != 1 || p2.HasMore {
		t.Fatalf("page 2: got %d / has_more=%v, want 1 / false", len(p2.Memories), p2.HasMore)
	}
	seen := append(memIDs(p1.Memories), memIDs(p2.Memories)...)
	if !distinct(seen) || len(seen) != 3 {
		t.Errorf("paginated ids = %v, want the 3 distinct seeded ids (no skip, no overlap)", seen)
	}

	// --- Filter by kind. ---
	sem := decodeList(t, getReq(handler, keyA, "/v1/memories?kind=semantic"))
	if len(sem.Memories) != 1 || sem.Memories[0].ID != m1 {
		t.Errorf("kind=semantic: got %v, want [%s]", memIDs(sem.Memories), m1)
	}

	// --- Filter by run: only the memory distilled from an event in runA. ---
	byRun := decodeList(t, getReq(handler, keyA, "/v1/memories?run_id="+runA))
	if len(byRun.Memories) != 1 || byRun.Memories[0].ID != m1 {
		t.Errorf("run_id filter: got %v, want [%s] (only the run-linked memory)", memIDs(byRun.Memories), m1)
	}

	// --- Lexical search: matches by content, ranked, with NO active embedding model pinned. ---
	srch := decodeList(t, getReq(handler, keyA, "/v1/memories?q=migration"))
	if len(srch.Memories) != 1 || srch.Memories[0].ID != m2 {
		t.Errorf("q=migration: got %v, want [%s] (lexical match, zero embedding)", memIDs(srch.Memories), m2)
	}
	if got := getReq(handler, keyA, "/v1/memories?q=deployed"); decodeList(t, got).Memories[0].ID != m1 {
		t.Errorf("q=deployed did not rank the auth memory first")
	}

	// --- Column filters: trust_tier and review_status narrow to the matching subset, and AND with kind. ---
	mq := seedMemoryRaw(ctx, t, st.Pool, projA, "semantic", "quarantined content", "quarantine", "auto_approved")
	mp := seedMemoryRaw(ctx, t, st.Pool, projA, "episodic", "pending review content", "normal", "pending")
	if q := decodeList(t, getReq(handler, keyA, "/v1/memories?trust_tier=quarantine")); len(q.Memories) != 1 || q.Memories[0].ID != mq {
		t.Errorf("trust_tier=quarantine: got %v, want [%s]", memIDs(q.Memories), mq)
	}
	if p := decodeList(t, getReq(handler, keyA, "/v1/memories?review_status=pending")); len(p.Memories) != 1 || p.Memories[0].ID != mp {
		t.Errorf("review_status=pending: got %v, want [%s]", memIDs(p.Memories), mp)
	}
	if c := decodeList(t, getReq(handler, keyA, "/v1/memories?kind=episodic&review_status=pending")); len(c.Memories) != 1 || c.Memories[0].ID != mp {
		t.Errorf("kind+review_status AND: got %v, want [%s]", memIDs(c.Memories), mp)
	}
	if c := decodeList(t, getReq(handler, keyA, "/v1/memories?kind=semantic&review_status=pending")); len(c.Memories) != 0 {
		t.Errorf("kind=semantic&review_status=pending: got %v, want [] (filters AND, so no row matches both)", memIDs(c.Memories))
	}

	// --- A malformed cursor is a 400, not a silent full scan (on both keyset-paginated routes). ---
	assertErr(t, getReq(handler, keyA, "/v1/memories?cursor=not-base64!!"), http.StatusBadRequest, "invalid_cursor")
	assertErr(t, getReq(handler, keyA, "/v1/runs/"+runA+"/trace?cursor=not-base64!!"), http.StatusBadRequest, "invalid_cursor")

	// --- Get one, unknown, and malformed. ---
	gotM := decodeMemory(t, getReq(handler, keyA, "/v1/memories/"+m1))
	if gotM.ID != m1 || !strings.Contains(gotM.Content, "authentication") || gotM.CreatedByAgent != "planner" {
		t.Errorf("get memory = %+v, want m1 with its content/agent", gotM)
	}
	assertErr(t, getReq(handler, keyA, "/v1/memories/"+uuid.NewString()), http.StatusNotFound, "not_found")
	assertErr(t, getReq(handler, keyA, "/v1/memories/not-a-uuid"), http.StatusBadRequest, "invalid_id")

	// --- Version history: empty for a v1 memory, one entry after a supersede is recorded, 404 for unknown. ---
	if v := decodeVersions(t, getReq(handler, keyA, "/v1/memories/"+m1+"/versions")); len(v.Versions) != 0 {
		t.Errorf("versions of a v1 memory = %d, want 0", len(v.Versions))
	}
	seedVersion(ctx, t, st.Pool, projA, m1, 1, "the auth service was deploying", "near-duplicate cosine 0.94")
	v := decodeVersions(t, getReq(handler, keyA, "/v1/memories/"+m1+"/versions"))
	if len(v.Versions) != 1 || v.Versions[0].Version != 1 || v.Versions[0].Reason == nil {
		t.Errorf("versions after a supersede = %+v, want one entry with a reason", v.Versions)
	}
	assertErr(t, getReq(handler, keyA, "/v1/memories/"+uuid.NewString()+"/versions"), http.StatusNotFound, "not_found")

	// --- Soft-delete: a TOMBSTONE (row + history RETAINED), invisible to get/list, idempotent, and audited. ---
	m4 := seedMemory(ctx, t, st.Pool, projA, "semantic", "temporary note to delete", "planner", pgtype.UUID{})
	seedVersion(ctx, t, st.Pool, projA, m4, 1, "the original note", "superseded before deletion")
	if rr := delReq(handler, keyA, "/v1/memories/"+m4); rr.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204 (body %q)", rr.Code, rr.Body.String())
	}
	assertErr(t, getReq(handler, keyA, "/v1/memories/"+m4), http.StatusNotFound, "not_found") // the live head is gone
	if listAfter := decodeList(t, getReq(handler, keyA, "/v1/memories?limit=50")); containsID(listAfter.Memories, m4) {
		t.Error("a soft-deleted memory still appears in the list")
	}
	// Soft, not hard: the physical row is RETAINED with its validity window stamped, so its version history still
	// resolves. A hard DELETE (or an ON DELETE cascade) would fail BOTH of these while passing every check above.
	if exists, validToSet := memoryRowState(ctx, t, st.Pool, projA, m4); !exists || !validToSet {
		t.Errorf("after delete: row exists=%v valid_to_set=%v, want true/true (a tombstone, not a removal)", exists, validToSet)
	}
	if dv := decodeVersions(t, getReq(handler, keyA, "/v1/memories/"+m4+"/versions")); len(dv.Versions) != 1 {
		t.Errorf("a deleted memory's version history = %d entries, want 1 (history outlives the tombstone)", len(dv.Versions))
	}
	assertErr(t, delReq(handler, keyA, "/v1/memories/"+m4), http.StatusNotFound, "not_found") // idempotent per live row
	// The audit trail records exactly one deletion, targeting the deleted memory.
	if got := auditCount(ctx, t, st.Pool, projA, "memory.delete"); got != 1 {
		t.Errorf("audit_log memory.delete rows = %d, want 1", got)
	}
	if target, actor := auditRow(ctx, t, st.Pool, projA, "memory.delete"); target != m4 || actor != "api" {
		t.Errorf("audit row = target %q actor %q, want %q / api", target, actor, m4)
	}

	// --- Run trace: pack_logs entries, unknown run 404, an empty run 200-empty. ---
	seedPackLog(ctx, t, st.Pool, projA, runA, "auth query", []string{m1})
	seedPackLog(ctx, t, st.Pool, projA, runA, "second query", []string{m1, m2})
	tr := decodeTrace(t, getReq(handler, keyA, "/v1/runs/"+runA+"/trace"))
	if len(tr.Packs) != 2 {
		t.Fatalf("run trace = %d packs, want 2", len(tr.Packs))
	}
	if len(tr.Packs[0].MemoryIDs) == 0 {
		t.Error("run trace entry carries no memory_ids")
	}
	assertErr(t, getReq(handler, keyA, "/v1/runs/"+uuid.NewString()+"/trace"), http.StatusNotFound, "not_found")
	_, emptyRun := seedProjectRunIn(ctx, t, st.Pool, projA) // a run in projA with no packs
	if e := decodeTrace(t, getReq(handler, keyA, "/v1/runs/"+emptyRun+"/trace")); len(e.Packs) != 0 || e.HasMore {
		t.Errorf("empty run trace = %d packs has_more=%v, want 0 / false", len(e.Packs), e.HasMore)
	}

	// --- Pagination edges (isolated project): the exact has_more boundary + the id tie-break under equal created_at. ---
	projT, _ := seedProjectRun(ctx, t, st.Pool)
	seedPartitions(ctx, t, st, projT)
	keyT, _ := provisionKey(ctx, t, st.Pool, projT)
	tie := seedTwoSameTime(ctx, t, st.Pool, projT) // two memories that share one created_at
	// Exact boundary: limit == total → the over-fetch returns exactly limit rows, so has_more is false, no cursor.
	if full := decodeList(t, getReq(handler, keyT, "/v1/memories?limit=2")); len(full.Memories) != 2 || full.HasMore || full.NextCursor != nil {
		t.Errorf("limit==count boundary: got %d has_more=%v cursor=%v, want 2 / false / nil", len(full.Memories), full.HasMore, full.NextCursor)
	}
	// Tie-break: with equal created_at the keyset must walk the id — paging limit=1 returns both, no skip/dup.
	t1 := decodeList(t, getReq(handler, keyT, "/v1/memories?limit=1"))
	if len(t1.Memories) != 1 || !t1.HasMore || t1.NextCursor == nil {
		t.Fatalf("tie page 1: got %d has_more=%v cursor=%v, want 1 / true / set", len(t1.Memories), t1.HasMore, t1.NextCursor)
	}
	t2 := decodeList(t, getReq(handler, keyT, "/v1/memories?limit=1&cursor="+*t1.NextCursor))
	if len(t2.Memories) != 1 || t2.HasMore {
		t.Fatalf("tie page 2: got %d has_more=%v, want 1 / false", len(t2.Memories), t2.HasMore)
	}
	paged := []string{t1.Memories[0].ID, t2.Memories[0].ID}
	if !distinct(paged) || !containsAll(paged, tie) {
		t.Errorf("tie pagination ids = %v, want the two seeded ids exactly once (id tie-break under equal created_at)", paged)
	}

	// --- Cross-tenant: project B's key sees none of project A's memories or runs — the SAME 404. ---
	projB, _ := seedProjectRun(ctx, t, st.Pool)
	seedPartitions(ctx, t, st, projB)
	keyB, _ := provisionKey(ctx, t, st.Pool, projB)
	assertErr(t, getReq(handler, keyB, "/v1/memories/"+m1), http.StatusNotFound, "not_found")
	assertErr(t, getReq(handler, keyB, "/v1/memories/"+m1+"/versions"), http.StatusNotFound, "not_found")
	assertErr(t, delReq(handler, keyB, "/v1/memories/"+m1), http.StatusNotFound, "not_found")
	assertErr(t, getReq(handler, keyB, "/v1/runs/"+runA+"/trace"), http.StatusNotFound, "not_found")
	// B's failed delete must NOT have tombstoned A's memory.
	if decodeMemory(t, getReq(handler, keyA, "/v1/memories/"+m1)).ID != m1 {
		t.Error("cross-tenant delete tombstoned another project's memory")
	}
	if listB := decodeList(t, getReq(handler, keyB, "/v1/memories")); len(listB.Memories) != 0 {
		t.Errorf("project B lists %d of project A's memories, want 0", len(listB.Memories))
	}

	// --- Still-stubbed ops answer 501 (behind auth, so a valid key reaches them). ---
	if rr := postReq(handler, keyA, "/v1/memories"); rr.Code != http.StatusNotImplemented {
		t.Errorf("POST /v1/memories = %d, want 501", rr.Code)
	}
	if rr := getReq(handler, keyA, "/v1/policies"); rr.Code != http.StatusNotImplemented {
		t.Errorf("GET /v1/policies = %d, want 501", rr.Code)
	}
}

// TestInspectSearchUsesFTSIndex is the anti-drift guard for the ?q leg: the search query's tsvector predicate
// (hand-copied from inspect.sql — sqlc consts are unexported cross-package) must ride the expression FTS GIN
// index, not fall to a sequential scan, which is what a drift between this predicate and the index expression
// would cause. It bulk-loads a selective corpus + ANALYZE so the index is genuinely the cheaper plan, then, with
// sequential scans penalized, EXPLAINs the predicate and asserts a bitmap index scan (the GIN access class).
func TestInspectSearchUsesFTSIndex(t *testing.T) {
	ctx := context.Background()
	st := inspectStore(ctx, t)
	projA, _ := seedProjectRun(ctx, t, st.Pool)
	seedPartitions(ctx, t, st, projA)

	// A selective term in a large corpus so the planner genuinely prefers the index over a scan.
	if _, err := st.Pool.Exec(ctx, `
		INSERT INTO memories (project_id, kind, content, scope_keys, trust_tier)
		SELECT $1, 'semantic', 'filler document number ' || g, ARRAY['run:r1'], 'normal' FROM generate_series(1, 3000) g`,
		mustUUID(t, projA)); err != nil {
		t.Fatalf("bulk seed: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `INSERT INTO memories (project_id, kind, content, scope_keys, trust_tier)
		VALUES ($1,'semantic','uniquezebraterm here',ARRAY['run:r1'],'normal')`, mustUUID(t, projA)); err != nil {
		t.Fatalf("insert selective row: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `ANALYZE memories`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
		t.Fatalf("disable seqscan: %v", err)
	}
	rows, err := tx.Query(ctx, `EXPLAIN
		SELECT m.id FROM memories m
		WHERE m.project_id = $1 AND m.superseded_by IS NULL AND m.valid_to IS NULL
		  AND to_tsvector('english', m.content) @@ websearch_to_tsquery('english', 'uniquezebraterm')`,
		mustUUID(t, projA))
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	if !strings.Contains(strings.ToLower(plan.String()), "bitmap index scan") {
		t.Errorf("the ?q predicate did not use the FTS GIN index (expression drift from the index?):\n%s", plan.String())
	}
}

// --- helpers ---

func inspectStore(ctx context.Context, t *testing.T) *store.Store {
	t.Helper()
	ctr, err := tcpostgres.Run(ctx, paradeDBImage,
		tcpostgres.WithDatabase("lore"), tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		t.Fatalf("start paradedb: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			t.Logf("terminate: %v", err)
		}
	})
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	if err := store.RunMigrations(ctx, dsn); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	st, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func seedPartitions(ctx context.Context, t *testing.T, st *store.Store, projectID string) {
	t.Helper()
	if err := store.CreateProjectPartitions(ctx, st.Pool, mustUUID(t, projectID)); err != nil {
		t.Fatalf("create partitions: %v", err)
	}
}

// seedProjectRunIn creates another run in an existing project and returns it.
func seedProjectRunIn(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID string) (string, string) {
	t.Helper()
	run, err := db.New(pool).InsertRun(ctx, mustUUID(t, projectID))
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return projectID, uuid.UUID(run.ID.Bytes).String()
}

func seedEvent(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID, agent string) pgtype.UUID {
	t.Helper()
	ev, err := db.New(pool).InsertEvent(ctx, db.InsertEventParams{
		RunID: mustUUID(t, runID), AgentID: agent, Payload: []byte(`{"memory":"x"}`),
	})
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
	return ev.ID
}

func seedMemory(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID, kind, content, agent string, src pgtype.UUID) string {
	t.Helper()
	a := agent
	id, err := db.New(pool).InsertMemory(ctx, db.InsertMemoryParams{
		ProjectID: mustUUID(t, projectID), Kind: kind, Content: content, CreatedByAgent: &a, SourceEventID: src,
	})
	if err != nil {
		t.Fatalf("insert memory: %v", err)
	}
	return uuid.UUID(id.Bytes).String()
}

func seedVersion(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID, memoryID string, version int32, content, reason string) {
	t.Helper()
	r := reason
	if _, err := db.New(pool).InsertMemoryVersion(ctx, db.InsertMemoryVersionParams{
		ProjectID: mustUUID(t, projectID), MemoryID: mustUUID(t, memoryID), Version: version, Content: content, Reason: &r,
	}); err != nil {
		t.Fatalf("insert memory version: %v", err)
	}
}

func seedPackLog(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID, runID, query string, memoryIDs []string) {
	t.Helper()
	ids := make([]pgtype.UUID, len(memoryIDs))
	for i, m := range memoryIDs {
		ids[i] = mustUUID(t, m)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO pack_logs (project_id, run_id, query, memory_ids) VALUES ($1, $2, $3, $4)`,
		mustUUID(t, projectID), mustUUID(t, runID), query, ids); err != nil {
		t.Fatalf("insert pack_log: %v", err)
	}
}

func auditCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID, action string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE project_id = $1 AND action = $2`,
		mustUUID(t, projectID), action).Scan(&n); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	return n
}

// memoryRowState reports whether a memory row physically exists in the project and whether its validity window
// is closed (valid_to set) — the direct check that a soft delete tombstoned the row rather than removing it.
func memoryRowState(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID, id string) (exists, validToSet bool) {
	t.Helper()
	if err := pool.QueryRow(ctx,
		`SELECT count(*) = 1, coalesce(bool_or(valid_to IS NOT NULL), false) FROM memories WHERE project_id = $1 AND id = $2`,
		mustUUID(t, projectID), mustUUID(t, id)).Scan(&exists, &validToSet); err != nil {
		t.Fatalf("read memory row state: %v", err)
	}
	return exists, validToSet
}

// auditRow returns the target and actor of the most recent audit_log row for an action, so a mis-targeted or
// mis-attributed audit entry is caught.
func auditRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID, action string) (target, actor string) {
	t.Helper()
	var tgt *string
	if err := pool.QueryRow(ctx,
		`SELECT target, actor FROM audit_log WHERE project_id = $1 AND action = $2 ORDER BY created_at DESC LIMIT 1`,
		mustUUID(t, projectID), action).Scan(&tgt, &actor); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if tgt != nil {
		target = *tgt
	}
	return target, actor
}

// seedMemoryRaw inserts a memory with explicit trust_tier and review_status (which InsertMemory takes as schema
// defaults), so the list column filters can be exercised.
func seedMemoryRaw(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID, kind, content, trustTier, reviewStatus string) string {
	t.Helper()
	var id pgtype.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO memories (project_id, kind, content, trust_tier, review_status, created_by_agent)
		 VALUES ($1, $2, $3, $4, $5, 'seed') RETURNING id`,
		mustUUID(t, projectID), kind, content, trustTier, reviewStatus).Scan(&id); err != nil {
		t.Fatalf("seed memory raw: %v", err)
	}
	return uuid.UUID(id.Bytes).String()
}

// seedTwoSameTime inserts two memories that share one created_at, so the keyset id tie-break can be exercised.
func seedTwoSameTime(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID string) []string {
	t.Helper()
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rows, err := pool.Query(ctx,
		`INSERT INTO memories (project_id, kind, content, created_at, created_by_agent)
		 VALUES ($1, 'semantic', 'tie alpha', $2, 'seed'), ($1, 'semantic', 'tie bravo', $2, 'seed') RETURNING id`,
		mustUUID(t, projectID), ts)
	if err != nil {
		t.Fatalf("seed two same-time: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id pgtype.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan id: %v", err)
		}
		ids = append(ids, uuid.UUID(id.Bytes).String())
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("seeded %d rows, want 2", len(ids))
	}
	return ids
}

func authReq(handler http.Handler, method, key, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func getReq(handler http.Handler, key, path string) *httptest.ResponseRecorder {
	return authReq(handler, http.MethodGet, key, path)
}
func delReq(handler http.Handler, key, path string) *httptest.ResponseRecorder {
	return authReq(handler, http.MethodDelete, key, path)
}
func postReq(handler http.Handler, key, path string) *httptest.ResponseRecorder {
	return authReq(handler, http.MethodPost, key, path)
}

func decodeList(t *testing.T, rr *httptest.ResponseRecorder) httpapi.MemoryListResponse {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (body %q)", rr.Code, rr.Body.String())
	}
	var out httpapi.MemoryListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	return out
}

func decodeMemory(t *testing.T, rr *httptest.ResponseRecorder) httpapi.Memory {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200 (body %q)", rr.Code, rr.Body.String())
	}
	var out httpapi.Memory
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode memory: %v", err)
	}
	return out
}

func decodeVersions(t *testing.T, rr *httptest.ResponseRecorder) httpapi.MemoryVersionListResponse {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("versions status = %d, want 200 (body %q)", rr.Code, rr.Body.String())
	}
	var out httpapi.MemoryVersionListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode versions: %v", err)
	}
	return out
}

func decodeTrace(t *testing.T, rr *httptest.ResponseRecorder) httpapi.RunTraceResponse {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("trace status = %d, want 200 (body %q)", rr.Code, rr.Body.String())
	}
	var out httpapi.RunTraceResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode trace: %v", err)
	}
	return out
}

func memIDs(ms []httpapi.Memory) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func containsID(ms []httpapi.Memory, id string) bool {
	for _, m := range ms {
		if m.ID == id {
			return true
		}
	}
	return false
}

func hasIDs(ms []httpapi.Memory, ids ...string) bool {
	for _, id := range ids {
		if !containsID(ms, id) {
			return false
		}
	}
	return true
}

func distinct(ss []string) bool {
	seen := map[string]bool{}
	for _, s := range ss {
		if seen[s] {
			return false
		}
		seen[s] = true
	}
	return true
}

func containsAll(got, want []string) bool {
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}
