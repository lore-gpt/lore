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

// TestMigration0020MemoryOccurredAt proves migration 0020: memories gains a nullable occurred_at column —
// nullable because a memory with no source event, and every memory distilled before the column existed, has
// no date to give it — and the change is cleanly reversible.
func TestMigration0020MemoryOccurredAt(t *testing.T) {
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

	if !columnExists(ctx, t, st.Pool, "memories", "occurred_at") {
		t.Fatal("memories should have gained occurred_at after 0020")
	}
	var dataType, nullable string
	if err := st.Pool.QueryRow(ctx,
		`SELECT data_type, is_nullable FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'memories' AND column_name = 'occurred_at'`).
		Scan(&dataType, &nullable); err != nil {
		t.Fatalf("read occurred_at definition: %v", err)
	}
	if dataType != "timestamp with time zone" || nullable != "YES" {
		t.Errorf("occurred_at is %s (nullable %s), want a nullable timestamp with time zone", dataType, nullable)
	}

	// Reversibility: Down 0020 drops the column, Up 0020 restores it.
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open sql.DB for goose: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		t.Fatalf("create goose provider: %v", err)
	}
	if _, err := provider.DownTo(ctx, 19); err != nil {
		t.Fatalf("down to 0019 (revert 0020): %v", err)
	}
	if columnExists(ctx, t, st.Pool, "memories", "occurred_at") {
		t.Error("down 0020 should drop memories.occurred_at")
	}
	if _, err := provider.UpTo(ctx, 20); err != nil {
		t.Fatalf("up to 0020 (reapply): %v", err)
	}
	if !columnExists(ctx, t, st.Pool, "memories", "occurred_at") {
		t.Error("up 0020 should restore memories.occurred_at")
	}
}
