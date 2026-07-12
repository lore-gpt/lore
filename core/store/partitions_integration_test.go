//go:build integration

package store_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/pressly/goose/v3"

	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/core/store/migrations"
)

// TestMigration0006Partitioning proves, against a real ParadeDB, that migration 0006
// turns memories and embeddings into LIST-partitioned-by-project_id tables and forces
// project_id + composite foreign keys onto the child tables; that the partition helper
// creates a partition per project (idempotently) and drops it as a tenant hard-delete;
// that with no default partition a write for an un-provisioned project fails loud; that
// rows route to and isolate within their tenant partition; and that the migration is
// cleanly reversible.
func TestMigration0006Partitioning(t *testing.T) {
	ctx := context.Background()
	dsn := startParadeDB(ctx, t)

	if err := store.RunMigrations(ctx, dsn); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	st, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	// memories and embeddings are partitioned parents; the child tables gained project_id.
	for _, tbl := range []string{"memories", "embeddings"} {
		if !isPartitioned(ctx, t, st.Pool, tbl) {
			t.Errorf("%s should be LIST-partitioned after 0006", tbl)
		}
	}
	for _, tbl := range []string{"memory_versions", "memory_scopes"} {
		if !columnExists(ctx, t, st.Pool, tbl, "project_id") {
			t.Errorf("%s should have gained project_id", tbl)
		}
	}

	q := db.New(st.Pool)
	org, err := q.InsertOrganization(ctx, "acme")
	if err != nil {
		t.Fatalf("insert organization: %v", err)
	}
	projA, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "a"})
	if err != nil {
		t.Fatalf("insert project a: %v", err)
	}
	projB, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "b"})
	if err != nil {
		t.Fatalf("insert project b: %v", err)
	}

	// No default partition: a write for a project with no partition yet fails loud. Postgres
	// reports "no partition of relation found for row" as SQLSTATE 23514 (it reuses
	// check_violation), so a future PG behavior change would surface right here.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO memories (project_id, kind, content) VALUES ($1, 'semantic', 'x')`,
		projA.ID); pgErrCode(err) != "23514" {
		t.Errorf("write before partition should raise 23514 (no partition found), got %q", pgErrCode(err))
	}

	// The helper creates a partition per project for both parents.
	if err := store.CreateProjectPartitions(ctx, st.Pool, projA.ID); err != nil {
		t.Fatalf("create partitions for a: %v", err)
	}
	if err := store.CreateProjectPartitions(ctx, st.Pool, projB.ID); err != nil {
		t.Fatalf("create partitions for b: %v", err)
	}
	// Idempotent: a second call is a no-op, not an error.
	if err := store.CreateProjectPartitions(ctx, st.Pool, projA.ID); err != nil {
		t.Fatalf("re-create partitions for a should be a no-op: %v", err)
	}
	for _, parent := range []string{"memories", "embeddings"} {
		if n := partitionCount(ctx, t, st.Pool, parent); n != 2 {
			t.Errorf("%s partitions = %d, want 2 (one per project)", parent, n)
		}
	}

	// Rows route to their tenant partition, and the composite child FKs hold.
	memA := insertMemory(ctx, t, st.Pool, projA.ID)
	memB := insertMemory(ctx, t, st.Pool, projB.ID)
	if _, err := q.UpsertEmbedding(ctx, db.UpsertEmbeddingParams{
		ProjectID: projA.ID, MemoryID: memA, ModelID: "m", Vec: pgvector.NewVector([]float32{1, 2, 3}),
	}); err != nil {
		t.Fatalf("upsert embedding for a: %v", err)
	}
	if _, err := q.InsertMemoryVersion(ctx, db.InsertMemoryVersionParams{
		ProjectID: projA.ID, MemoryID: memA, Version: 1, Content: "v1",
	}); err != nil {
		t.Fatalf("insert memory version (composite FK) for a: %v", err)
	}
	if _, err := q.InsertMemoryScope(ctx, db.InsertMemoryScopeParams{
		ProjectID: projA.ID, MemoryID: memA, ScopeType: "run", ScopeID: "r1",
	}); err != nil {
		t.Fatalf("insert memory scope (composite FK) for a: %v", err)
	}
	// Project B also gets an embedding, so dropping its partition later exercises the
	// embeddings DETACH+DROP with a row present, not just an empty leaf.
	if _, err := q.UpsertEmbedding(ctx, db.UpsertEmbeddingParams{
		ProjectID: projB.ID, MemoryID: memB, ModelID: "m", Vec: pgvector.NewVector([]float32{4, 5, 6}),
	}); err != nil {
		t.Fatalf("upsert embedding for b: %v", err)
	}

	// Each memory physically lands in its own tenant partition (routing by project_id),
	// independent of the drop-isolation check below.
	if got, want := memoryPartition(ctx, t, st.Pool, memA), "memories_p_"+partSuffix(projA.ID); got != want {
		t.Errorf("memA landed in %q, want %q", got, want)
	}
	if got, want := memoryPartition(ctx, t, st.Pool, memB), "memories_p_"+partSuffix(projB.ID); got != want {
		t.Errorf("memB landed in %q, want %q", got, want)
	}

	// The composite FK rejects a child row whose memory belongs to a DIFFERENT tenant — the
	// whole reason the foreign key is (project_id, memory_id) and not memory_id alone.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO memory_versions (project_id, memory_id, version, content) VALUES ($1, $2, 1, 'x')`,
		projB.ID, memA); pgErrCode(err) != "23503" {
		t.Errorf("cross-tenant child (project B + memory A) should raise 23503, got %q", pgErrCode(err))
	}

	var total int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM memories`).Scan(&total); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if total != 2 {
		t.Errorf("memories across partitions = %d, want 2", total)
	}

	// Dropping project B's partitions is the tenant hard-delete: B's rows vanish, A's remain.
	if err := store.DropProjectPartitions(ctx, st.Pool, projB.ID); err != nil {
		t.Fatalf("drop partitions for b: %v", err)
	}
	if n := partitionCount(ctx, t, st.Pool, "memories"); n != 1 {
		t.Errorf("memories partitions after dropping b = %d, want 1", n)
	}
	if n := partitionCount(ctx, t, st.Pool, "embeddings"); n != 1 {
		t.Errorf("embeddings partitions after dropping b = %d, want 1", n)
	}
	var haveB, haveA int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM memories WHERE id = $1`, memB).Scan(&haveB); err != nil {
		t.Fatalf("count memory b: %v", err)
	}
	if haveB != 0 {
		t.Error("dropping b's partition should remove b's memory")
	}
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM memories WHERE id = $1`, memA).Scan(&haveA); err != nil {
		t.Fatalf("count memory a: %v", err)
	}
	if haveA != 1 {
		t.Error("a's memory should survive dropping b's partition")
	}
	// Idempotent drop.
	if err := store.DropProjectPartitions(ctx, st.Pool, projB.ID); err != nil {
		t.Fatalf("re-drop partitions for b should be a no-op: %v", err)
	}

	// The composite self-FK uses ON DELETE SET NULL (superseded_by): deleting a memory that
	// another (same-tenant) memory is superseded by nulls only the pointer, leaving the
	// predecessor and its NOT NULL partition key project_id intact.
	older := insertMemory(ctx, t, st.Pool, projA.ID)
	newer := insertMemory(ctx, t, st.Pool, projA.ID)
	if _, err := st.Pool.Exec(ctx,
		`UPDATE memories SET superseded_by = $1 WHERE id = $2`, newer, older); err != nil {
		t.Fatalf("supersede older memory: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `DELETE FROM memories WHERE id = $1`, newer); err != nil {
		t.Fatalf("delete the superseding memory: %v", err)
	}
	var supersededBy pgtype.UUID
	var projectStillSet bool
	if err := st.Pool.QueryRow(ctx,
		`SELECT superseded_by, project_id IS NOT NULL FROM memories WHERE id = $1`, older).
		Scan(&supersededBy, &projectStillSet); err != nil {
		t.Fatalf("read superseded predecessor (it should survive): %v", err)
	}
	if supersededBy.Valid {
		t.Error("deleting the successor should null superseded_by on the predecessor")
	}
	if !projectStillSet {
		t.Error("ON DELETE SET NULL must not touch the NOT NULL partition key project_id")
	}

	// Migration 0006 is cleanly reversible: Down restores the plain, non-partitioned shape
	// and removes project_id from the child tables; Up restores partitioning.
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open sql.DB for goose: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		t.Fatalf("create goose provider: %v", err)
	}
	if _, err := provider.DownTo(ctx, 5); err != nil {
		t.Fatalf("goose down to 0005 (revert 0006): %v", err)
	}
	if isPartitioned(ctx, t, st.Pool, "memories") {
		t.Error("down 0006 should restore memories to a plain table")
	}
	if columnExists(ctx, t, st.Pool, "memory_versions", "project_id") {
		t.Error("down 0006 should drop memory_versions.project_id")
	}
	// The Down ran with rows present (project A still had data): its cascade-delete cleared
	// them so the plain foreign keys could be re-added. Prove that path ran, not just failed loud.
	var remaining int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM memories`).Scan(&remaining); err != nil {
		t.Fatalf("count memories after down: %v", err)
	}
	if remaining != 0 {
		t.Errorf("down 0006 should have cleared memories rows via cascade, got %d", remaining)
	}
	if _, err := provider.UpTo(ctx, 6); err != nil {
		t.Fatalf("goose up to 0006 (reapply 0006): %v", err)
	}
	if !isPartitioned(ctx, t, st.Pool, "memories") {
		t.Error("up 0006 should re-partition memories")
	}
	if !columnExists(ctx, t, st.Pool, "memory_scopes", "project_id") {
		t.Error("up 0006 should restore memory_scopes.project_id")
	}
}

// isPartitioned reports whether a table is a partitioned parent (relkind 'p').
func isPartitioned(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table string) bool {
	t.Helper()
	var kind string
	if err := pool.QueryRow(ctx,
		`SELECT relkind FROM pg_class WHERE relname = $1 AND relnamespace = 'public'::regnamespace`,
		table).Scan(&kind); err != nil {
		t.Fatalf("query relkind for %q: %v", table, err)
	}
	return kind == "p"
}

// partitionCount returns how many partitions a partitioned parent currently has.
func partitionCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, parent string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhparent WHERE c.relname = $1`,
		parent).Scan(&n); err != nil {
		t.Fatalf("count partitions of %q: %v", parent, err)
	}
	return n
}

// partSuffix mirrors the partition-name suffix the store helper derives from a project id.
func partSuffix(id pgtype.UUID) string {
	return strings.ReplaceAll(uuid.UUID(id.Bytes).String(), "-", "")
}

// memoryPartition returns the name of the partition a memory row physically lives in.
func memoryPartition(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id pgtype.UUID) string {
	t.Helper()
	var name string
	if err := pool.QueryRow(ctx,
		`SELECT tableoid::regclass::text FROM memories WHERE id = $1`, id).Scan(&name); err != nil {
		t.Fatalf("read partition of memory: %v", err)
	}
	return name
}
