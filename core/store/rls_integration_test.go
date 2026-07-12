//go:build integration

package store_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/pressly/goose/v3"

	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/core/store/migrations"
)

// TestMigration0008RolesRLS proves, against a real ParadeDB, the tenant-isolation second belt:
// the lore_app role is subject to Row-Level Security, which scopes every tenant table to the
// session's lore.project_id — a different tenant's rows are invisible, an unset scope is
// fail-closed, and cross-tenant writes are rejected. It also proves the intentional owner bypass,
// that RLS reaches the partitioned tables, the composite events(project_id, run_id) foreign key,
// the WithProject helper, and reversibility.
func TestMigration0008RolesRLS(t *testing.T) {
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

	// Seed two tenants (as the superuser pool role, which bypasses RLS).
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
	memA := insertMemory(ctx, t, st.Pool, projA.ID)
	memB := insertMemory(ctx, t, st.Pool, projB.ID)
	if _, err := q.UpsertEmbedding(ctx, db.UpsertEmbeddingParams{
		ProjectID: projA.ID, MemoryID: memA, ModelID: "m", Vec: pgvector.NewVector([]float32{1, 2, 3}),
	}); err != nil {
		t.Fatalf("seed embedding a: %v", err)
	}
	if _, err := q.UpsertEmbedding(ctx, db.UpsertEmbeddingParams{
		ProjectID: projB.ID, MemoryID: memB, ModelID: "m", Vec: pgvector.NewVector([]float32{4, 5, 6}),
	}); err != nil {
		t.Fatalf("seed embedding b: %v", err)
	}
	runA, err := q.InsertRun(ctx, projA.ID)
	if err != nil {
		t.Fatalf("insert run a: %v", err)
	}
	// InsertEvent derives project_id from the run; confirm it stamps run A's project.
	evA, err := q.InsertEvent(ctx, db.InsertEventParams{RunID: runA.ID, AgentID: "a", Payload: []byte("{}")})
	if err != nil {
		t.Fatalf("insert event a: %v", err)
	}
	if uuid.UUID(evA.ProjectID.Bytes) != uuid.UUID(projA.ID.Bytes) {
		t.Errorf("event project_id = %v, want run's project %v", evA.ProjectID, projA.ID)
	}

	projAStr := uuid.UUID(projA.ID.Bytes).String()

	t.Run("roles_exist", func(t *testing.T) {
		var n int
		if err := st.Pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_roles WHERE rolname IN ('lore_app','lore_migrate','lore_readonly')`).
			Scan(&n); err != nil {
			t.Fatalf("count roles: %v", err)
		}
		if n != 3 {
			t.Errorf("role count = %d, want 3", n)
		}
	})

	// As lore_app scoped to A, only A's rows exist.
	t.Run("tenant_visibility", func(t *testing.T) {
		asAppRole(ctx, t, st.Pool, projAStr, func(tx pgx.Tx) {
			if got := count(ctx, t, tx, `SELECT count(*) FROM memories`); got != 1 {
				t.Errorf("memories visible to A = %d, want 1 (only A's)", got)
			}
			if got := count(ctx, t, tx, `SELECT count(*) FROM memories WHERE id = $1`, memB); got != 0 {
				t.Errorf("B's memory visible to A = %d, want 0", got)
			}
		})
	})

	// No scope set → no rows (fail-closed), not an error and not everything.
	t.Run("fail_closed", func(t *testing.T) {
		asAppRole(ctx, t, st.Pool, "", func(tx pgx.Tx) {
			if got := count(ctx, t, tx, `SELECT count(*) FROM memories`); got != 0 {
				t.Errorf("memories with no scope = %d, want 0 (fail-closed)", got)
			}
		})
	})

	// A write into A's scope succeeds; a write carrying B's project_id is rejected by WITH CHECK.
	t.Run("cross_tenant_write_blocked", func(t *testing.T) {
		asAppRole(ctx, t, st.Pool, projAStr, func(tx pgx.Tx) {
			if _, err := tx.Exec(ctx,
				`INSERT INTO memories (project_id, kind, content) VALUES ($1, 'semantic', 'ok')`,
				projA.ID); err != nil {
				t.Errorf("in-scope insert should succeed: %v", err)
			}
		})
		asAppRole(ctx, t, st.Pool, projAStr, func(tx pgx.Tx) {
			if _, err := tx.Exec(ctx,
				`INSERT INTO memories (project_id, kind, content) VALUES ($1, 'semantic', 'x')`,
				projB.ID); pgErrCode(err) != "42501" {
				t.Errorf("writing B's project while scoped to A should raise 42501, got %q", pgErrCode(err))
			}
		})
	})

	// A cross-tenant UPDATE/DELETE touches zero rows — B's row is simply invisible.
	t.Run("cross_tenant_update_delete_zero", func(t *testing.T) {
		asAppRole(ctx, t, st.Pool, projAStr, func(tx pgx.Tx) {
			tag, err := tx.Exec(ctx, `UPDATE memories SET content = 'x' WHERE id = $1`, memB)
			if err != nil {
				t.Fatalf("update: %v", err)
			}
			if tag.RowsAffected() != 0 {
				t.Errorf("cross-tenant update affected %d rows, want 0", tag.RowsAffected())
			}
			tag, err = tx.Exec(ctx, `DELETE FROM memories WHERE id = $1`, memB)
			if err != nil {
				t.Fatalf("delete: %v", err)
			}
			if tag.RowsAffected() != 0 {
				t.Errorf("cross-tenant delete affected %d rows, want 0", tag.RowsAffected())
			}
		})
	})

	// The owner/superuser bypass is intentional (migrations, analytics): as the pool role, both
	// tenants' rows are visible.
	t.Run("owner_bypass", func(t *testing.T) {
		if got := count(ctx, t, st.Pool, `SELECT count(*) FROM memories`); got != 2 {
			t.Errorf("memories visible to owner = %d, want 2 (bypass is intentional)", got)
		}
	})

	// RLS reaches the partitioned tables: embeddings is LIST-partitioned, yet the policy on the
	// parent scopes it to one tenant.
	t.Run("partition_propagation", func(t *testing.T) {
		asAppRole(ctx, t, st.Pool, projAStr, func(tx pgx.Tx) {
			if got := count(ctx, t, tx, `SELECT count(*) FROM embeddings`); got != 1 {
				t.Errorf("embeddings visible to A = %d, want 1 (RLS reaches partitions)", got)
			}
		})
	})

	// The composite foreign key stops an event whose project_id disagrees with its run's project.
	t.Run("events_composite_fk", func(t *testing.T) {
		if _, err := st.Pool.Exec(ctx,
			`INSERT INTO events (project_id, run_id, agent_id, payload) VALUES ($1, $2, 'a', '{}'::jsonb)`,
			projB.ID, runA.ID); pgErrCode(err) != "23503" {
			t.Errorf("event for run A tagged with project B should raise 23503, got %q", pgErrCode(err))
		}
	})

	// Every tenant table 0008 lists carries RLS + a policy — a per-table typo or an omitted
	// CREATE POLICY would otherwise ship green (the policies are hand-repeated).
	t.Run("all_tables_have_rls", func(t *testing.T) {
		for _, tbl := range []string{
			"projects", "api_keys", "runs", "events", "memories", "embeddings",
			"memory_versions", "memory_scopes", "claims", "entities", "entity_links", "pack_logs",
		} {
			if !rlsEnabled(ctx, t, st.Pool, tbl) {
				t.Errorf("%s should have RLS enabled", tbl)
			}
			if got := count(ctx, t, st.Pool,
				`SELECT count(*) FROM pg_policies WHERE schemaname = 'public' AND tablename = $1`, tbl); got == 0 {
				t.Errorf("%s should have a tenant policy", tbl)
			}
		}
	})

	// The two structurally distinct policies: projects is keyed on id (not project_id), and events
	// guards a server-derived column. Prove both actually filter under the app role and fail closed.
	t.Run("projects_and_events_scoped", func(t *testing.T) {
		asAppRole(ctx, t, st.Pool, projAStr, func(tx pgx.Tx) {
			if got := count(ctx, t, tx, `SELECT count(*) FROM projects`); got != 1 {
				t.Errorf("projects visible to A = %d, want 1 (only A's own row)", got)
			}
			if got := count(ctx, t, tx, `SELECT count(*) FROM projects WHERE id = $1`, projB.ID); got != 0 {
				t.Errorf("B's project visible to A = %d, want 0", got)
			}
			if got := count(ctx, t, tx, `SELECT count(*) FROM events`); got != 1 {
				t.Errorf("events visible to A = %d, want 1 (only A's event)", got)
			}
		})
		asAppRole(ctx, t, st.Pool, "", func(tx pgx.Tx) {
			if got := count(ctx, t, tx, `SELECT count(*) FROM projects`); got != 0 {
				t.Errorf("projects with no scope = %d, want 0 (fail-closed)", got)
			}
			if got := count(ctx, t, tx, `SELECT count(*) FROM events`); got != 0 {
				t.Errorf("events with no scope = %d, want 0 (fail-closed)", got)
			}
		})
	})

	// WITH CHECK also guards UPDATE: a tenant cannot relabel its OWN visible row into another
	// tenant. USING selects A's row (visible), then WITH CHECK rejects the project_id = B post-image.
	t.Run("with_check_blocks_row_move", func(t *testing.T) {
		asAppRole(ctx, t, st.Pool, projAStr, func(tx pgx.Tx) {
			if _, err := tx.Exec(ctx,
				`UPDATE memories SET project_id = $1 WHERE id = $2`, projB.ID, memA); pgErrCode(err) != "42501" {
				t.Errorf("moving A's own row to project B should raise 42501, got %q", pgErrCode(err))
			}
		})
	})

	// WithProject sets the scope for the duration of its transaction.
	t.Run("with_project_roundtrip", func(t *testing.T) {
		var got string
		if err := st.WithProject(ctx, projA.ID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT current_setting('lore.project_id', true)`).Scan(&got)
		}); err != nil {
			t.Fatalf("WithProject: %v", err)
		}
		if got != projAStr {
			t.Errorf("scope inside WithProject = %q, want %q", got, projAStr)
		}
	})

	// 0008 is reversible even with events rows present: Down drops events.project_id and Up
	// backfills it from runs, so the round-trip reconstructs the derived column rather than failing
	// on the NOT NULL re-add. events still holds A's seeded event here, so this exercises that path.
	t.Run("reversibility", func(t *testing.T) {
		sqlDB, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatalf("open sql.DB for goose: %v", err)
		}
		defer func() { _ = sqlDB.Close() }()
		provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
		if err != nil {
			t.Fatalf("create goose provider: %v", err)
		}
		if _, err := provider.DownTo(ctx, 7); err != nil {
			t.Fatalf("goose down to 0007 (revert 0008): %v", err)
		}
		if rlsEnabled(ctx, t, st.Pool, "memories") {
			t.Error("down 0008 should disable RLS on memories")
		}
		if columnExists(ctx, t, st.Pool, "events", "project_id") {
			t.Error("down 0008 should drop events.project_id")
		}
		if _, err := provider.UpTo(ctx, 8); err != nil {
			t.Fatalf("goose up to 0008 (reapply): %v", err)
		}
		if !rlsEnabled(ctx, t, st.Pool, "memories") {
			t.Error("up 0008 should re-enable RLS on memories")
		}
		if !columnExists(ctx, t, st.Pool, "events", "project_id") {
			t.Error("up 0008 should restore events.project_id")
		}
	})
}

// asAppRole runs fn in a transaction that has assumed the non-owner lore_app role (so RLS
// applies) and, when projectID is non-empty, set lore.project_id. Both are SET LOCAL / tx-scoped,
// and the transaction is rolled back — these are read-only probes with no lasting effect.
func asAppRole(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID string, fn func(tx pgx.Tx)) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE lore_app`); err != nil {
		t.Fatalf("set role lore_app: %v", err)
	}
	if projectID != "" {
		if _, err := tx.Exec(ctx, `SELECT set_config('lore.project_id', $1, true)`, projectID); err != nil {
			t.Fatalf("set_config lore.project_id: %v", err)
		}
	}
	fn(tx)
}

// count runs a scalar count query and returns it.
func count(ctx context.Context, t *testing.T, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, sqlStr string, args ...any) int {
	t.Helper()
	var n int
	if err := q.QueryRow(ctx, sqlStr, args...).Scan(&n); err != nil {
		t.Fatalf("count query: %v", err)
	}
	return n
}

// rlsEnabled reports whether a table currently has Row-Level Security enabled.
func rlsEnabled(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table string) bool {
	t.Helper()
	var on bool
	if err := pool.QueryRow(ctx,
		`SELECT relrowsecurity FROM pg_class WHERE relname = $1 AND relnamespace = 'public'::regnamespace`,
		table).Scan(&on); err != nil {
		t.Fatalf("read relrowsecurity for %q: %v", table, err)
	}
	return on
}
