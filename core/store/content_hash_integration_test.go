//go:build integration

package store_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/migrations"
)

// TestMigration0013MemoryContentHash proves migration 0013: memories gains a nullable content_hash
// column and a partial index over live rows for the dedup probe, and both are cleanly reversible.
func TestMigration0013MemoryContentHash(t *testing.T) {
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

	// indexExists reports whether the named (partitioned) index is present on the parent table.
	indexExists := func(name string) bool {
		t.Helper()
		var n int
		if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relname = $1`, name).Scan(&n); err != nil {
			t.Fatalf("check index %s: %v", name, err)
		}
		return n > 0
	}

	if !columnExists(ctx, t, st.Pool, "memories", "content_hash") {
		t.Error("memories should have gained content_hash after 0013")
	}
	if !indexExists("memories_content_hash_idx") {
		t.Error("0013 should create memories_content_hash_idx")
	}

	// Reversibility: Down 0013 drops the column and its index, Up 0013 restores both.
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open sql.DB for goose: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		t.Fatalf("create goose provider: %v", err)
	}
	if _, err := provider.DownTo(ctx, 12); err != nil {
		t.Fatalf("down to 0012 (revert 0013): %v", err)
	}
	if columnExists(ctx, t, st.Pool, "memories", "content_hash") {
		t.Error("down 0013 should drop memories.content_hash")
	}
	if indexExists("memories_content_hash_idx") {
		t.Error("down 0013 should drop memories_content_hash_idx")
	}
	if _, err := provider.UpTo(ctx, 13); err != nil {
		t.Fatalf("up to 0013 (reapply): %v", err)
	}
	if !columnExists(ctx, t, st.Pool, "memories", "content_hash") {
		t.Error("up 0013 should restore memories.content_hash")
	}
	if !indexExists("memories_content_hash_idx") {
		t.Error("up 0013 should restore memories_content_hash_idx")
	}
}
