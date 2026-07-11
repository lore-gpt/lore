//go:build integration

package store_test

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/pressly/goose/v3"

	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/core/store/migrations"
)

// TestMigration0005Embeddings proves, against a real ParadeDB, that migration 0005
// lands the dimension-free multi-model embeddings table, the inline entities.embedding
// column, and projects.active_model_id; that vectors round-trip through the sqlc
// pgvector mapping (including per-(memory, model) upsert and multi-model coexistence);
// that the memory and tenancy foreign keys hold; and that the migration is reversible.
func TestMigration0005Embeddings(t *testing.T) {
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

	if !tableExists(ctx, t, st.Pool, "embeddings") {
		t.Error("table \"embeddings\" missing after 0005")
	}
	if !columnExists(ctx, t, st.Pool, "entities", "embedding") {
		t.Error("migration 0005 should add entities.embedding")
	}
	if !columnExists(ctx, t, st.Pool, "projects", "active_model_id") {
		t.Error("migration 0005 should add projects.active_model_id")
	}

	q := db.New(st.Pool)
	org, err := q.InsertOrganization(ctx, "acme")
	if err != nil {
		t.Fatalf("insert organization: %v", err)
	}
	proj, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "platform"})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if proj.ActiveModelID != nil {
		t.Errorf("new project active_model_id = %q, want nil (no model chosen yet)", *proj.ActiveModelID)
	}
	mem := insertMemory(ctx, t, st.Pool, proj.ID)

	// --- a vector round-trips through the sqlc pgvector mapping. ---
	vecA := []float32{0.1, 0.2, 0.3}
	if _, err := q.UpsertEmbedding(ctx, db.UpsertEmbeddingParams{
		ProjectID: proj.ID, MemoryID: mem, ModelID: "model-a", Vec: pgvector.NewVector(vecA),
	}); err != nil {
		t.Fatalf("upsert embedding (model-a): %v", err)
	}
	got, err := q.GetEmbedding(ctx, db.GetEmbeddingParams{MemoryID: mem, ModelID: "model-a"})
	if err != nil {
		t.Fatalf("get embedding (model-a): %v", err)
	}
	if !slices.Equal(got.Vec.Slice(), vecA) {
		t.Errorf("model-a vec = %v, want %v", got.Vec.Slice(), vecA)
	}

	// A second model for the same memory coexists (dimension-free: different length ok).
	vecB := []float32{1, 2, 3, 4}
	if _, err := q.UpsertEmbedding(ctx, db.UpsertEmbeddingParams{
		ProjectID: proj.ID, MemoryID: mem, ModelID: "model-b", Vec: pgvector.NewVector(vecB),
	}); err != nil {
		t.Fatalf("upsert embedding (model-b): %v", err)
	}
	if got, err := q.GetEmbedding(ctx, db.GetEmbeddingParams{MemoryID: mem, ModelID: "model-b"}); err != nil {
		t.Fatalf("get embedding (model-b): %v", err)
	} else if !slices.Equal(got.Vec.Slice(), vecB) {
		t.Errorf("model-b vec = %v, want %v", got.Vec.Slice(), vecB)
	}

	// Upsert on the (memory, model) key updates the vector in place — no duplicate row.
	vecA2 := []float32{0.7, 0.8, 0.9}
	if _, err := q.UpsertEmbedding(ctx, db.UpsertEmbeddingParams{
		ProjectID: proj.ID, MemoryID: mem, ModelID: "model-a", Vec: pgvector.NewVector(vecA2),
	}); err != nil {
		t.Fatalf("re-upsert embedding (model-a): %v", err)
	}
	if got, err := q.GetEmbedding(ctx, db.GetEmbeddingParams{MemoryID: mem, ModelID: "model-a"}); err != nil {
		t.Fatalf("get embedding (model-a, updated): %v", err)
	} else if !slices.Equal(got.Vec.Slice(), vecA2) {
		t.Errorf("model-a vec after re-upsert = %v, want %v", got.Vec.Slice(), vecA2)
	}
	var rowCount int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM embeddings WHERE memory_id = $1`, mem).Scan(&rowCount); err != nil {
		t.Fatalf("count embeddings: %v", err)
	}
	if rowCount != 2 {
		t.Errorf("embeddings for memory = %d, want 2 (one per model, upsert not insert)", rowCount)
	}

	// The same model holds vectors for many memories (model is project-level, not
	// memory-unique): a second memory embeds under model-a and both coexist.
	mem2 := insertMemory(ctx, t, st.Pool, proj.ID)
	vecC := []float32{0.4, 0.5, 0.6}
	if _, err := q.UpsertEmbedding(ctx, db.UpsertEmbeddingParams{
		ProjectID: proj.ID, MemoryID: mem2, ModelID: "model-a", Vec: pgvector.NewVector(vecC),
	}); err != nil {
		t.Fatalf("upsert embedding (mem2, model-a): %v", err)
	}
	if got, err := q.GetEmbedding(ctx, db.GetEmbeddingParams{MemoryID: mem2, ModelID: "model-a"}); err != nil {
		t.Fatalf("get embedding (mem2, model-a): %v", err)
	} else if !slices.Equal(got.Vec.Slice(), vecC) {
		t.Errorf("mem2 model-a vec = %v, want %v", got.Vec.Slice(), vecC)
	}

	// --- foreign keys hold. ---
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO embeddings (project_id, memory_id, model_id, vec) VALUES ($1, $2, 'm', '[1,2,3]')`,
		proj.ID, randomUUID(ctx, t, st.Pool)); pgErrCode(err) != "23503" {
		t.Errorf("embedding for unknown memory should raise 23503, got %q", pgErrCode(err))
	}
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO embeddings (project_id, memory_id, model_id, vec) VALUES ($1, $2, 'm', '[1,2,3]')`,
		randomUUID(ctx, t, st.Pool), mem); pgErrCode(err) != "23503" {
		t.Errorf("embedding for unknown project should raise 23503, got %q", pgErrCode(err))
	}

	// --- entities.embedding round-trips inline. ---
	entVec := []float32{0.5, 0.6}
	var entID pgtype.UUID
	if err := st.Pool.QueryRow(ctx,
		`INSERT INTO entities (project_id, name, type, embedding) VALUES ($1, 'auth-svc', 'service', $2)
		 RETURNING id`,
		proj.ID, pgvector.NewVector(entVec)).Scan(&entID); err != nil {
		t.Fatalf("insert entity with embedding: %v", err)
	}
	var entGot pgvector.Vector
	if err := st.Pool.QueryRow(ctx,
		`SELECT embedding FROM entities WHERE id = $1`, entID).Scan(&entGot); err != nil {
		t.Fatalf("read entity embedding: %v", err)
	}
	if !slices.Equal(entGot.Slice(), entVec) {
		t.Errorf("entity embedding = %v, want %v", entGot.Slice(), entVec)
	}
	// A NULL embedding (the normal state until one is computed) scans cleanly to nil.
	// The registered pgvector codec handles the NULL; pgvector.Vector's own Scan cannot.
	var entNoEmb pgtype.UUID
	if err := st.Pool.QueryRow(ctx,
		`INSERT INTO entities (project_id, name, type) VALUES ($1, 'no-vec', 'service') RETURNING id`,
		proj.ID).Scan(&entNoEmb); err != nil {
		t.Fatalf("insert entity without embedding: %v", err)
	}
	var nullEmb *pgvector.Vector
	if err := st.Pool.QueryRow(ctx,
		`SELECT embedding FROM entities WHERE id = $1`, entNoEmb).Scan(&nullEmb); err != nil {
		t.Fatalf("scan NULL entity embedding (codec must handle NULL): %v", err)
	}
	if nullEmb != nil {
		t.Errorf("NULL embedding scanned to %v, want nil", nullEmb)
	}

	// --- projects.active_model_id round-trips. ---
	if _, err := st.Pool.Exec(ctx,
		`UPDATE projects SET active_model_id = 'model-a' WHERE id = $1`, proj.ID); err != nil {
		t.Fatalf("set active_model_id: %v", err)
	}
	var active *string
	if err := st.Pool.QueryRow(ctx,
		`SELECT active_model_id FROM projects WHERE id = $1`, proj.ID).Scan(&active); err != nil {
		t.Fatalf("read active_model_id: %v", err)
	}
	if active == nil || *active != "model-a" {
		t.Errorf("active_model_id = %v, want %q", active, "model-a")
	}

	// Migration 0005 is cleanly reversible. Target it by version (down to 0004, back up
	// through 0005) so the check stays correct as later migrations stack on top.
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open sql.DB for goose: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		t.Fatalf("create goose provider: %v", err)
	}
	if _, err := provider.DownTo(ctx, 4); err != nil {
		t.Fatalf("goose down to 0004 (revert 0005): %v", err)
	}
	if tableExists(ctx, t, st.Pool, "embeddings") {
		t.Error("down 0005 should drop the embeddings table")
	}
	if columnExists(ctx, t, st.Pool, "entities", "embedding") {
		t.Error("down 0005 should drop entities.embedding")
	}
	if columnExists(ctx, t, st.Pool, "projects", "active_model_id") {
		t.Error("down 0005 should drop projects.active_model_id")
	}
	if _, err := provider.UpTo(ctx, 5); err != nil {
		t.Fatalf("goose up to 0005 (reapply 0005): %v", err)
	}
	if !tableExists(ctx, t, st.Pool, "embeddings") {
		t.Error("up 0005 should restore the embeddings table")
	}
	if !columnExists(ctx, t, st.Pool, "entities", "embedding") {
		t.Error("up 0005 should restore entities.embedding")
	}
	if !columnExists(ctx, t, st.Pool, "projects", "active_model_id") {
		t.Error("up 0005 should restore projects.active_model_id")
	}
}

// columnExists reports whether table.column exists in the public schema.
func columnExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table, column string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2)`,
		table, column).Scan(&exists); err != nil {
		t.Fatalf("query column %s.%s: %v", table, column, err)
	}
	return exists
}
