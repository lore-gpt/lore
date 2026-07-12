//go:build integration

package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"

	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
)

// TestVectorIndexPerPartitionHNSW proves, against a real ParadeDB, that PgVectorIndex
// builds one HNSW index per embeddings partition over the dimensionless vec column (via a
// fixed-dimension expression cast); that the build is idempotent; that the cast pins the
// partition to a single model dimension; and that dropping a tenant's partition takes its
// vector index with it.
func TestVectorIndexPerPartitionHNSW(t *testing.T) {
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
	for _, p := range []pgtype.UUID{projA.ID, projB.ID} {
		if err := store.CreateProjectPartitions(ctx, st.Pool, p); err != nil {
			t.Fatalf("create partitions: %v", err)
		}
	}

	const dim = 8
	// A memory + a dim-8 embedding in each tenant partition, so the index builds over real
	// rows (the backfill path) rather than an empty table.
	memA := insertMemory(ctx, t, st.Pool, projA.ID)
	memB := insertMemory(ctx, t, st.Pool, projB.ID)
	if _, err := q.UpsertEmbedding(ctx, db.UpsertEmbeddingParams{
		ProjectID: projA.ID, MemoryID: memA, ModelID: "m", Vec: unitVector(dim, 0),
	}); err != nil {
		t.Fatalf("seed embedding a: %v", err)
	}
	if _, err := q.UpsertEmbedding(ctx, db.UpsertEmbeddingParams{
		ProjectID: projB.ID, MemoryID: memB, ModelID: "m", Vec: unitVector(dim, 1),
	}); err != nil {
		t.Fatalf("seed embedding b: %v", err)
	}

	vi := store.NewPgVectorIndex(st.Pool)
	if err := vi.EnsureIndex(ctx, projA.ID, dim); err != nil {
		t.Fatalf("ensure index a: %v", err)
	}
	if err := vi.EnsureIndex(ctx, projB.ID, dim); err != nil {
		t.Fatalf("ensure index b: %v", err)
	}

	// Each embeddings partition carries its OWN valid HNSW index, cast to the model dimension.
	for _, p := range []pgtype.UUID{projA.ID, projB.ID} {
		leaf := "embeddings_p_" + partSuffix(p)
		def, valid, ok := hnswIndex(ctx, t, st.Pool, leaf)
		if !ok {
			t.Errorf("%s has no hnsw index", leaf)
			continue
		}
		if !valid {
			t.Errorf("%s hnsw index is INVALID (build did not complete)", leaf)
		}
		for _, want := range []string{"USING hnsw", "vector(8)", "vector_cosine_ops"} {
			if !strings.Contains(def, want) {
				t.Errorf("%s index def %q missing %q", leaf, def, want)
			}
		}
	}

	// Idempotent: a second EnsureIndex is a no-op, and the partition still has exactly one
	// hnsw index (IF NOT EXISTS did not stack a duplicate).
	if err := vi.EnsureIndex(ctx, projA.ID, dim); err != nil {
		t.Fatalf("re-ensure index a should be a no-op: %v", err)
	}
	if n := hnswIndexCount(ctx, t, st.Pool, "embeddings_p_"+partSuffix(projA.ID)); n != 1 {
		t.Errorf("project a hnsw index count = %d, want 1", n)
	}

	// The fixed-dimension cast pins the partition to one model's dimension: a differently
	// sized vector can no longer be written, because the index expression rejects it.
	memA2 := insertMemory(ctx, t, st.Pool, projA.ID)
	if _, err := q.UpsertEmbedding(ctx, db.UpsertEmbeddingParams{
		ProjectID: projA.ID, MemoryID: memA2, ModelID: "m", Vec: pgvector.NewVector([]float32{1, 2, 3}),
	}); err == nil {
		t.Error("a 3-d vector should be rejected by the vector(8) index expression")
	}

	// Dropping a tenant's partition takes its vector index with it (no orphaned index).
	if err := store.DropProjectPartitions(ctx, st.Pool, projB.ID); err != nil {
		t.Fatalf("drop partitions for b: %v", err)
	}
	if _, _, ok := hnswIndex(ctx, t, st.Pool, "embeddings_p_"+partSuffix(projB.ID)); ok {
		t.Error("dropping b's partition should have removed its hnsw index")
	}
}

// TestVectorIndexSelfHealsInvalidIndex proves the self-heal branch: a CONCURRENTLY build
// that fails midway (here, a wrong-dimension row makes the cast fail) leaves an INVALID
// index behind, which a plain CREATE INDEX IF NOT EXISTS would skip past forever. EnsureIndex
// must detect and drop that corpse, then rebuild a valid index once the data is consistent.
func TestVectorIndexSelfHealsInvalidIndex(t *testing.T) {
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

	q := db.New(st.Pool)
	org, err := q.InsertOrganization(ctx, "acme")
	if err != nil {
		t.Fatalf("insert organization: %v", err)
	}
	proj, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "a"})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if err := store.CreateProjectPartitions(ctx, st.Pool, proj.ID); err != nil {
		t.Fatalf("create partitions: %v", err)
	}
	leaf := "embeddings_p_" + partSuffix(proj.ID)

	// Two rows of different dimensions: the column is dimensionless, so both inserts succeed
	// while there is no index. The 3-d row is the landmine the vector(8) build will step on.
	good := insertMemory(ctx, t, st.Pool, proj.ID)
	bad := insertMemory(ctx, t, st.Pool, proj.ID)
	if _, err := q.UpsertEmbedding(ctx, db.UpsertEmbeddingParams{
		ProjectID: proj.ID, MemoryID: good, ModelID: "m", Vec: unitVector(8, 0),
	}); err != nil {
		t.Fatalf("seed good embedding: %v", err)
	}
	if _, err := q.UpsertEmbedding(ctx, db.UpsertEmbeddingParams{
		ProjectID: proj.ID, MemoryID: bad, ModelID: "m", Vec: pgvector.NewVector([]float32{1, 2, 3}),
	}); err != nil {
		t.Fatalf("seed bad embedding: %v", err)
	}

	// The build fails on the 3-d row, and Postgres leaves an invalid index behind.
	vi := store.NewPgVectorIndex(st.Pool)
	if err := vi.EnsureIndex(ctx, proj.ID, 8); err == nil {
		t.Fatal("EnsureIndex should fail while a wrong-dimension row is present")
	}
	if _, valid, ok := hnswIndex(ctx, t, st.Pool, leaf); !ok || valid {
		t.Fatalf("failed build should leave an INVALID index (ok=%v valid=%v)", ok, valid)
	}

	// Make the data consistent, then EnsureIndex again: without the self-heal, IF NOT EXISTS
	// would see the invalid index and skip, leaving searches on a sequential scan forever.
	if _, err := st.Pool.Exec(ctx, `DELETE FROM memories WHERE id = $1`, bad); err != nil {
		t.Fatalf("remove wrong-dimension row: %v", err)
	}
	if err := vi.EnsureIndex(ctx, proj.ID, 8); err != nil {
		t.Fatalf("EnsureIndex should self-heal once data is consistent: %v", err)
	}
	if _, valid, ok := hnswIndex(ctx, t, st.Pool, leaf); !ok || !valid {
		t.Errorf("self-heal should leave one VALID index (ok=%v valid=%v)", ok, valid)
	}
	if n := hnswIndexCount(ctx, t, st.Pool, leaf); n != 1 {
		t.Errorf("hnsw index count after self-heal = %d, want 1", n)
	}
}

// unitVector returns a dim-length vector that is 1 at position at and 0 elsewhere — a
// non-zero vector (cosine distance is undefined for the zero vector).
func unitVector(dim, at int) pgvector.Vector {
	v := make([]float32, dim)
	v[at] = 1
	return pgvector.NewVector(v)
}

// hnswIndex returns the definition and validity of a partition's HNSW index (named
// <leaf>_vec_hnsw), and whether such an index exists at all.
func hnswIndex(ctx context.Context, t *testing.T, pool *pgxpool.Pool, leaf string) (def string, valid, ok bool) {
	t.Helper()
	err := pool.QueryRow(ctx, `
		SELECT pg_get_indexdef(i.indexrelid), i.indisvalid
		FROM pg_index i
		JOIN pg_class idx ON idx.oid = i.indexrelid
		JOIN pg_class tbl ON tbl.oid = i.indrelid
		WHERE tbl.relname = $1 AND idx.relname = $1 || '_vec_hnsw'`, leaf).Scan(&def, &valid)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, false
	}
	if err != nil {
		t.Fatalf("query hnsw index for %q: %v", leaf, err)
	}
	return def, valid, true
}

// hnswIndexCount counts the HNSW indexes on a partition leaf, so a duplicate build would
// be caught.
func hnswIndexCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, leaf string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM pg_index i
		JOIN pg_class tbl ON tbl.oid = i.indrelid
		WHERE tbl.relname = $1 AND i.indexrelid IN (
			SELECT oid FROM pg_class WHERE relam = (SELECT oid FROM pg_am WHERE amname = 'hnsw'))`,
		leaf).Scan(&n); err != nil {
		t.Fatalf("count hnsw indexes for %q: %v", leaf, err)
	}
	return n
}
