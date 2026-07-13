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

// TestMigration0010RunsCoveredSeq proves migration 0010's runs.covered_seq extraction checkpoint —
// defaulting to 0 and cleanly reversible — together with AdvanceCoveredSeq's compare-and-swap advance:
// the checkpoint moves only when the expected value still matches, so a pass whose starting value has
// since been advanced by a concurrent pass matches no row and does not double-advance.
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
	advance := func(expected, to int64) int64 {
		t.Helper()
		n, err := q.AdvanceCoveredSeq(ctx, db.AdvanceCoveredSeqParams{
			ProjectID: proj.ID, RunID: run.ID, ExpectedCoveredSeq: expected, NewCoveredSeq: to,
		})
		if err != nil {
			t.Fatalf("advance covered_seq %d->%d: %v", expected, to, err)
		}
		return n
	}

	// Fresh runs start at 0 (nothing consumed).
	if got := readCovered(); got != 0 {
		t.Errorf("new run covered_seq = %d, want 0", got)
	}
	// A compare-and-swap from the current value advances the checkpoint.
	if rows := advance(0, 3); rows != 1 {
		t.Errorf("advance 0->3 affected %d rows, want 1", rows)
	}
	if got := readCovered(); got != 3 {
		t.Errorf("covered_seq = %d, want 3", got)
	}
	// A stale expected value (the checkpoint moved since the pass read it) matches no row: 0 rows,
	// value unchanged. This is the guard that makes a concurrent double-delivery's loser a no-op.
	if rows := advance(2, 8); rows != 0 {
		t.Errorf("advance with stale expected 2 affected %d rows, want 0", rows)
	}
	if got := readCovered(); got != 3 {
		t.Errorf("covered_seq after stale-expected advance = %d, want 3 (unchanged)", got)
	}
	// The same guard rejects a would-be duplicate/backward advance: expected 0 no longer matches.
	if rows := advance(0, 3); rows != 0 {
		t.Errorf("advance with stale expected 0 affected %d rows, want 0", rows)
	}
	// A compare-and-swap from the true current value advances it again.
	if rows := advance(3, 5); rows != 1 {
		t.Errorf("advance 3->5 affected %d rows, want 1", rows)
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
