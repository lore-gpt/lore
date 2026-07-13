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

// TestMigration0014ClaimsResolutionReason proves migration 0014: claims gains a nullable
// resolution_reason column, cleanly reversible.
func TestMigration0014ClaimsResolutionReason(t *testing.T) {
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

	if !columnExists(ctx, t, st.Pool, "claims", "resolution_reason") {
		t.Error("claims should have gained resolution_reason after 0014")
	}

	// Reversibility: Down 0014 drops the column, Up 0014 restores it.
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open sql.DB for goose: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		t.Fatalf("create goose provider: %v", err)
	}
	if _, err := provider.DownTo(ctx, 13); err != nil {
		t.Fatalf("down to 0013 (revert 0014): %v", err)
	}
	if columnExists(ctx, t, st.Pool, "claims", "resolution_reason") {
		t.Error("down 0014 should drop claims.resolution_reason")
	}
	if _, err := provider.UpTo(ctx, 14); err != nil {
		t.Fatalf("up to 0014 (reapply): %v", err)
	}
	if !columnExists(ctx, t, st.Pool, "claims", "resolution_reason") {
		t.Error("up 0014 should restore claims.resolution_reason")
	}
}
