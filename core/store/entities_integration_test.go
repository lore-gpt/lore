//go:build integration

package store_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/core/store/migrations"
)

// TestMigration0004EntitiesEntityLinks proves, against a real ParadeDB, that
// migration 0004 lands the entity node and bi-temporal edge tables; that the entity
// and tenancy foreign keys hold; that el_current_uq admits exactly one current edge
// per (project, src, predicate, dst) and lets an edge be re-stated via a deferred-FK
// close-then-insert swap; that self-supersession is rejected and the deferred self-FK
// is validated at commit; and that the migration is cleanly reversible.
func TestMigration0004EntitiesEntityLinks(t *testing.T) {
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

	for _, tbl := range []string{"entities", "entity_links"} {
		if !tableExists(ctx, t, st.Pool, tbl) {
			t.Errorf("table %q missing after 0004", tbl)
		}
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
	src := insertEntity(ctx, t, st.Pool, proj.ID, "deploy-svc", "service")
	dst := insertEntity(ctx, t, st.Pool, proj.ID, "auth-svc", "service")

	// --- foreign keys hold. ---
	// An entity in an unknown project is rejected by the tenancy FK.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO entities (project_id, name, type) VALUES ($1, 'ghost', 'service')`,
		randomUUID(ctx, t, st.Pool)); pgErrCode(err) != "23503" {
		t.Errorf("entity in unknown project should raise 23503 (foreign_key_violation), got %q", pgErrCode(err))
	}
	// An edge whose src entity does not exist is rejected by the entity FK.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO entity_links
		   (project_id, src_entity_id, dst_entity_id, predicate, valid_from, provenance, first_seen_seq, last_seen_seq)
		 VALUES ($1, $2, $3, 'depends_on', now(), '{}'::uuid[], 1, 1)`,
		proj.ID, randomUUID(ctx, t, st.Pool), dst); pgErrCode(err) != "23503" {
		t.Errorf("edge with unknown src entity should raise 23503, got %q", pgErrCode(err))
	}
	// ...and the same for a dst entity that does not exist (both FKs are exercised).
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO entity_links
		   (project_id, src_entity_id, dst_entity_id, predicate, valid_from, provenance, first_seen_seq, last_seen_seq)
		 VALUES ($1, $2, $3, 'depends_on', now(), '{}'::uuid[], 1, 1)`,
		proj.ID, src, randomUUID(ctx, t, st.Pool)); pgErrCode(err) != "23503" {
		t.Errorf("edge with unknown dst entity should raise 23503, got %q", pgErrCode(err))
	}

	// insertCurrentLink adds a current (valid_to IS NULL) depends_on edge src->dst.
	insertCurrentLink := func(seq int) (pgtype.UUID, error) {
		var id pgtype.UUID
		err := st.Pool.QueryRow(ctx,
			`INSERT INTO entity_links
			   (project_id, src_entity_id, dst_entity_id, predicate, valid_from, provenance, first_seen_seq, last_seen_seq)
			 VALUES ($1, $2, $3, 'depends_on', now(), '{}'::uuid[], $4, $4)
			 RETURNING id`,
			proj.ID, src, dst, seq).Scan(&id)
		return id, err
	}

	// --- el_current_uq: at most one current edge per (project, src, predicate, dst). ---
	first, err := insertCurrentLink(1)
	if err != nil {
		t.Fatalf("insert first edge: %v", err)
	}
	if _, err := insertCurrentLink(1); pgErrCode(err) != "23505" {
		t.Errorf("second current edge for the same tuple should raise 23505 (unique_violation), got %q", pgErrCode(err))
	}

	// predicate is part of the current-edge key, and closing an edge (valid_to alone)
	// frees its own slot independent of the supersession machinery. A different predicate
	// for the same src/dst is a distinct current edge; closing it lets a fresh current
	// edge for the same tuple open — proving el_current_uq's partial predicate directly.
	var callsEdge pgtype.UUID
	if err := st.Pool.QueryRow(ctx,
		`INSERT INTO entity_links
		   (project_id, src_entity_id, dst_entity_id, predicate, valid_from, provenance, first_seen_seq, last_seen_seq)
		 VALUES ($1, $2, $3, 'calls', now(), '{}'::uuid[], 1, 1)
		 RETURNING id`,
		proj.ID, src, dst).Scan(&callsEdge); err != nil {
		t.Fatalf("a different predicate for the same src/dst should coexist as current: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE entity_links SET valid_to = now() WHERE id = $1`, callsEdge); err != nil {
		t.Fatalf("close calls edge: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO entity_links
		   (project_id, src_entity_id, dst_entity_id, predicate, valid_from, provenance, first_seen_seq, last_seen_seq)
		 VALUES ($1, $2, $3, 'calls', now(), '{}'::uuid[], 2, 2)`,
		proj.ID, src, dst); err != nil {
		t.Fatalf("reopening a current edge after close should succeed (el_current_uq frees the slot): %v", err)
	}
	// A depends_on and a calls current edge now coexist for the same src/dst: predicate
	// is genuinely part of the current-edge key, not ignored.
	var distinctCurrent int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM entity_links
		 WHERE project_id = $1 AND src_entity_id = $2 AND dst_entity_id = $3 AND valid_to IS NULL`,
		proj.ID, src, dst).Scan(&distinctCurrent); err != nil {
		t.Fatalf("count current edges across predicates: %v", err)
	}
	if distinctCurrent != 2 {
		t.Errorf("current edges for src/dst across predicates = %d, want 2 (depends_on + calls)", distinctCurrent)
	}

	// Re-state the depends_on edge in one transaction: close the current edge (set valid_to
	// and point it at its replacement), then insert the replacement as the new current edge.
	// This demonstrates the re-state workflow succeeds; that the self-FK actually exists and
	// is deferred to commit is proven separately by the dangling-commit case further down.
	replacementID := randomUUID(ctx, t, st.Pool)
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin swap tx: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE entity_links SET valid_to = now(), superseded_by = $1 WHERE id = $2`,
		replacementID, first); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("close first edge: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO entity_links
		   (id, project_id, src_entity_id, dst_entity_id, predicate, valid_from, provenance, first_seen_seq, last_seen_seq)
		 VALUES ($1, $2, $3, $4, 'depends_on', now(), '{}'::uuid[], 2, 2)`,
		replacementID, proj.ID, src, dst); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("insert replacement edge: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit edge swap (deferred self-FK should hold at commit): %v", err)
	}

	var currentCount int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM entity_links
		 WHERE project_id = $1 AND src_entity_id = $2 AND predicate = 'depends_on'
		   AND dst_entity_id = $3 AND valid_to IS NULL`,
		proj.ID, src, dst).Scan(&currentCount); err != nil {
		t.Fatalf("count current edges: %v", err)
	}
	if currentCount != 1 {
		t.Errorf("current edges for the tuple = %d, want 1", currentCount)
	}
	var supersededBy pgtype.UUID
	if err := st.Pool.QueryRow(ctx,
		`SELECT superseded_by FROM entity_links WHERE id = $1`, first).Scan(&supersededBy); err != nil {
		t.Fatalf("read superseded_by: %v", err)
	}
	if supersededBy != replacementID {
		t.Error("closed edge should be superseded by the replacement")
	}

	// An edge superseding itself is rejected by entity_links_no_self_supersede.
	if _, err := st.Pool.Exec(ctx,
		`UPDATE entity_links SET superseded_by = id WHERE id = $1`, replacementID); pgErrCode(err) != "23514" {
		t.Errorf("self-supersede should raise 23514 (check_violation), got %q", pgErrCode(err))
	}

	// The self-FK is validated at COMMIT (deferred), not skipped: pointing superseded_by
	// at an edge id that is never inserted must fail the commit.
	danglingTx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin dangling tx: %v", err)
	}
	if _, err := danglingTx.Exec(ctx,
		`UPDATE entity_links SET superseded_by = $1 WHERE id = $2`,
		randomUUID(ctx, t, st.Pool), replacementID); err != nil {
		_ = danglingTx.Rollback(ctx)
		t.Fatalf("deferred FK should not fire on the update statement, got: %v", err)
	}
	if err := danglingTx.Commit(ctx); pgErrCode(err) != "23503" {
		t.Errorf("dangling superseded_by should fail commit with 23503 (foreign_key_violation), got %q", pgErrCode(err))
	}

	// Migration 0004 is cleanly reversible. Target it by version (down to 0003, back up
	// through 0004) so the check stays correct as later migrations stack on top.
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open sql.DB for goose: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		t.Fatalf("create goose provider: %v", err)
	}
	if _, err := provider.DownTo(ctx, 3); err != nil {
		t.Fatalf("goose down to 0003 (revert 0004): %v", err)
	}
	for _, tbl := range []string{"entities", "entity_links"} {
		if tableExists(ctx, t, st.Pool, tbl) {
			t.Errorf("down 0004 should drop the %q table", tbl)
		}
	}
	if _, err := provider.UpTo(ctx, 4); err != nil {
		t.Fatalf("goose up to 0004 (reapply 0004): %v", err)
	}
	for _, tbl := range []string{"entities", "entity_links"} {
		if !tableExists(ctx, t, st.Pool, tbl) {
			t.Errorf("up 0004 should restore the %q table", tbl)
		}
	}
}

// insertEntity inserts a minimal entity row and returns its id, for use as the src or
// dst of an edge.
func insertEntity(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID pgtype.UUID, name, typ string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO entities (project_id, name, type) VALUES ($1, $2, $3) RETURNING id`,
		projectID, name, typ).Scan(&id); err != nil {
		t.Fatalf("insert entity %q: %v", name, err)
	}
	return id
}
