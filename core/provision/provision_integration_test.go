//go:build integration

package provision_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/lore-gpt/lore/core/apikey"
	"github.com/lore-gpt/lore/core/provision"
	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
)

const paradeDBImage = "paradedb/paradedb:0.24.2-pg17"

// TestProvision proves the bootstrap end to end against a real ParadeDB: one call creates the organization,
// the project, its partitions, and an API key that actually resolves to the project — everything the write
// path needs. It also proves the pieces commit together (a project row exists) and that a second call is a
// fresh tenant (Provision is not idempotent by itself; the compose one-shot guards on its credentials file).
func TestProvision(t *testing.T) {
	ctx := context.Background()

	ctr, err := tcpostgres.Run(ctx, paradeDBImage,
		tcpostgres.WithDatabase("lore"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start paradedb container: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			t.Logf("terminate container: %v", err)
		}
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	if err := store.RunMigrations(ctx, dsn); err != nil {
		t.Fatalf("store migrations: %v", err)
	}
	st, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	res, err := provision.Provision(ctx, st.Pool, "acme", "demo")
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	// --- The result is well formed. ---
	if _, err := uuid.Parse(res.OrgID); err != nil {
		t.Errorf("org id %q is not a UUID: %v", res.OrgID, err)
	}
	if _, err := uuid.Parse(res.ProjectID); err != nil {
		t.Errorf("project id %q is not a UUID: %v", res.ProjectID, err)
	}
	if !strings.HasPrefix(res.Token, "lore_sk_") {
		t.Errorf("token %q lacks the lore_sk_ prefix", res.Token)
	}
	if res.KeyPrefix == "" {
		t.Error("key prefix is empty")
	}

	// --- The org and project rows landed with the requested names. ---
	if got := scalarText(ctx, t, st.Pool, `SELECT name FROM organizations WHERE id = $1::uuid`, res.OrgID); got != "acme" {
		t.Errorf("organization name = %q, want acme", got)
	}
	if got := scalarText(ctx, t, st.Pool, `SELECT name FROM projects WHERE id = $1::uuid`, res.ProjectID); got != "demo" {
		t.Errorf("project name = %q, want demo", got)
	}

	// --- The partitions the first memory/embedding write needs exist. ---
	suffix := strings.ReplaceAll(res.ProjectID, "-", "")
	for _, leaf := range []string{"memories_p_" + suffix, "embeddings_p_" + suffix} {
		if !relationExists(ctx, t, st.Pool, leaf) {
			t.Errorf("partition %s was not created", leaf)
		}
	}

	// --- The minted token authenticates: hashing it and looking it up resolves to this project. This is the
	// end-to-end proof that the key was stored correctly, not just that a row exists. ---
	pid, err := db.New(st.Pool).LookupAPIKeyProject(ctx, apikey.Hash(res.Token))
	if err != nil {
		t.Fatalf("look up minted key: %v", err)
	}
	if got := uuid.UUID(pid.Bytes).String(); got != res.ProjectID {
		t.Errorf("minted token resolves to project %s, want %s", got, res.ProjectID)
	}

	// --- A second provision is a distinct tenant (not idempotent on its own). ---
	res2, err := provision.Provision(ctx, st.Pool, "acme", "demo")
	if err != nil {
		t.Fatalf("second provision: %v", err)
	}
	if res2.ProjectID == res.ProjectID {
		t.Error("second provision reused the first project — Provision must mint a fresh tenant each call")
	}

	// --- Atomicity: a provision that fails partway leaves nothing behind. Rename api_keys away so the final
	// CreateAPIKey step errors AFTER the organization, project, and partitions are inserted in the same
	// transaction; those inserts must roll back, so the org and project counts do not move. ---
	orgsBefore := countRows(ctx, t, st.Pool, "organizations")
	projectsBefore := countRows(ctx, t, st.Pool, "projects")
	if _, err := st.Pool.Exec(ctx, `ALTER TABLE api_keys RENAME TO api_keys_hidden`); err != nil {
		t.Fatalf("hide api_keys: %v", err)
	}
	_, provErr := provision.Provision(ctx, st.Pool, "acme", "demo")
	if _, err := st.Pool.Exec(ctx, `ALTER TABLE api_keys_hidden RENAME TO api_keys`); err != nil {
		t.Fatalf("restore api_keys: %v", err)
	}
	if provErr == nil {
		t.Fatal("provision succeeded with api_keys missing — expected the key insert to fail")
	}
	if got := countRows(ctx, t, st.Pool, "organizations"); got != orgsBefore {
		t.Errorf("organizations moved %d -> %d after a failed provision — the transaction did not roll back", orgsBefore, got)
	}
	if got := countRows(ctx, t, st.Pool, "projects"); got != projectsBefore {
		t.Errorf("projects moved %d -> %d after a failed provision — the transaction did not roll back", projectsBefore, got)
	}
}

// countRows returns a table's row count, for the rollback assertion. The table name is a fixed literal, not
// user input.
func countRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// scalarText reads a single text value, failing the test on error.
func scalarText(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql, arg string) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, sql, arg).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return s
}

// relationExists reports whether a relation is present (its partition was created).
func relationExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var reg *string
	if err := pool.QueryRow(ctx, `SELECT to_regclass($1)::text`, name).Scan(&reg); err != nil {
		t.Fatalf("to_regclass(%q): %v", name, err)
	}
	return reg != nil
}
