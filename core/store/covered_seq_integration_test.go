//go:build integration

package store_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/core/store/migrations"
)

// TestMigration0010RunsCoveredSeq proves migration 0010: runs gains a covered_seq extraction
// checkpoint defaulting to 0, AdvanceCoveredSeq moves it forward monotonically (a backward or
// duplicate advance is a no-op), and the column is cleanly reversible.
func TestMigration0010RunsCoveredSeq(t *testing.T) {
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

	if !columnExists(ctx, t, st.Pool, "runs", "covered_seq") {
		t.Fatal("runs should have gained covered_seq after 0010")
	}

	q := db.New(st.Pool)
	org, err := q.InsertOrganization(ctx, "acme")
	if err != nil {
		t.Fatalf("insert org: %v", err)
	}
	proj, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "a"})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	run, err := q.InsertRun(ctx, proj.ID)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	readCovered := func() int64 {
		t.Helper()
		var n int64
		if err := st.Pool.QueryRow(ctx, `SELECT covered_seq FROM runs WHERE id = $1`, run.ID).Scan(&n); err != nil {
			t.Fatalf("read covered_seq: %v", err)
		}
		return n
	}
	advance := func(to int64) int64 {
		t.Helper()
		n, err := q.AdvanceCoveredSeq(ctx, db.AdvanceCoveredSeqParams{ProjectID: proj.ID, RunID: run.ID, CoveredSeq: to})
		if err != nil {
			t.Fatalf("advance covered_seq to %d: %v", to, err)
		}
		return n
	}

	// Fresh runs start at 0 (nothing consumed).
	if got := readCovered(); got != 0 {
		t.Errorf("new run covered_seq = %d, want 0", got)
	}
	// Forward advance updates the row.
	if rows := advance(3); rows != 1 {
		t.Errorf("advance to 3 affected %d rows, want 1", rows)
	}
	if got := readCovered(); got != 3 {
		t.Errorf("covered_seq = %d, want 3", got)
	}
	// A backward advance is a no-op (monotonic guard): 0 rows, value unchanged.
	if rows := advance(2); rows != 0 {
		t.Errorf("backward advance to 2 affected %d rows, want 0 (monotonic)", rows)
	}
	if got := readCovered(); got != 3 {
		t.Errorf("covered_seq after backward advance = %d, want 3 (unchanged)", got)
	}
	// A duplicate advance to the current value is also a no-op.
	if rows := advance(3); rows != 0 {
		t.Errorf("duplicate advance to 3 affected %d rows, want 0", rows)
	}
	// A further forward advance moves it again.
	if rows := advance(5); rows != 1 {
		t.Errorf("advance to 5 affected %d rows, want 1", rows)
	}
	if got := readCovered(); got != 5 {
		t.Errorf("covered_seq = %d, want 5", got)
	}

	// Reversibility: Down 0010 drops the column, Up 0010 restores it.
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open sql.DB for goose: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		t.Fatalf("create goose provider: %v", err)
	}
	if _, err := provider.DownTo(ctx, 9); err != nil {
		t.Fatalf("down to 0009 (revert 0010): %v", err)
	}
	if columnExists(ctx, t, st.Pool, "runs", "covered_seq") {
		t.Error("down 0010 should drop runs.covered_seq")
	}
	if _, err := provider.UpTo(ctx, 10); err != nil {
		t.Fatalf("up to 0010 (reapply): %v", err)
	}
	if !columnExists(ctx, t, st.Pool, "runs", "covered_seq") {
		t.Error("up 0010 should restore runs.covered_seq")
	}
}
