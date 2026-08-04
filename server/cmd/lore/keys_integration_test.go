//go:build integration

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
)

// runKeys executes one `lore keys` subcommand with fresh command state, capturing everything it printed.
// Cobra commands hold flag state, so each call builds its own — reusing one would leak --project between cases.
func runKeys(t *testing.T, build func() *cobra.Command, args ...string) (string, error) {
	t.Helper()
	cmd := build()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// TestKeysLifecycle covers `lore keys` end to end against a real database.
//
// Neither subcommand had a single test before this: `RevokeAPIKey` appeared only as happy-path fixture setup
// in two HTTP tests, so the branch this slice is about — the one where nothing was revoked — had never been
// executed. The cases below walk the lifecycle an operator actually performs, and each pins a distinct
// decision rather than just exercising the code.
func TestKeysLifecycle(t *testing.T) {
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
	t.Setenv("LORE_DATABASE_URL", dsn)

	st, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	q := db.New(st.Pool)

	org, err := q.InsertOrganization(ctx, "acme")
	if err != nil {
		t.Fatalf("insert org: %v", err)
	}
	proj, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "keys"})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	projectID := uuid.UUID(proj.ID.Bytes).String()

	// An empty project must not be an error: `list` is usable as a scripted precondition check only if
	// "there are none" exits 0.
	out, err := runKeys(t, keysListCmd, "--project", projectID)
	if err != nil {
		t.Fatalf("listing a project with no keys failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no api keys") {
		t.Errorf("an empty project should say so plainly, got:\n%s", out)
	}

	if _, err := runKeys(t, keysCreateCmd, "--project", projectID, "--name", "ci"); err != nil {
		t.Fatalf("keys create: %v", err)
	}

	// The listing has to carry the id (the handle for revoke) and the prefix (the reason that column exists —
	// it is what lets an operator match a row against a key they are holding). It must never carry the hash.
	out, err = runKeys(t, keysListCmd, "--project", projectID)
	if err != nil {
		t.Fatalf("keys list: %v\n%s", err, out)
	}
	keyID := firstListedKeyID(t, out)
	if !strings.Contains(out, "ci") {
		t.Errorf("listing omits the key's name:\n%s", out)
	}
	if !strings.Contains(out, "lore_sk_") {
		t.Errorf("listing omits the key prefix, which is what makes a row recognisable:\n%s", out)
	}
	if !strings.Contains(out, "active") {
		t.Errorf("listing omits the key's status:\n%s", out)
	}
	var hash string
	if err := st.Pool.QueryRow(ctx, `SELECT key_hash FROM api_keys WHERE id = $1`,
		keyID).Scan(&hash); err != nil {
		t.Fatalf("read key hash: %v", err)
	}
	if strings.Contains(out, hash) {
		t.Error("the listing printed the stored key hash; it must show only non-secret columns")
	}

	// First revoke: the state changed, so it reports the change.
	out, err = runKeys(t, keysRevokeCmd, keyID)
	if err != nil {
		t.Fatalf("first revoke failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "revoked api key") {
		t.Errorf("first revoke did not confirm the revocation:\n%s", out)
	}

	// And the listing reflects it, which is the question an operator usually has.
	out, err = runKeys(t, keysListCmd, "--project", projectID)
	if err != nil {
		t.Fatalf("keys list after revoke: %v\n%s", err, out)
	}
	if !strings.Contains(out, "revoked ") {
		t.Errorf("a revoked key must still appear, marked revoked — the history is the point:\n%s", out)
	}
	if strings.Contains(out, "\tactive") {
		t.Errorf("the revoked key is still listed as active:\n%s", out)
	}

	// Second revoke: the caller's desired state is already true. Exiting non-zero here would fail a retried
	// deploy step for having nothing to do, which is the trap this slice removes.
	out, err = runKeys(t, keysRevokeCmd, keyID)
	if err != nil {
		t.Errorf("re-revoking an already-revoked key must succeed — the key is revoked, which is what the "+
			"caller asked for; got: %v\n%s", err, out)
	}
	if !strings.Contains(out, "already revoked") || !strings.Contains(out, "no change") {
		t.Errorf("the second revoke must say it changed nothing, so a human is not misled into thinking it "+
			"just took effect:\n%s", out)
	}

	// An unknown id is a different thing entirely: the operator holds an id that names nothing, and a script
	// should stop. This is the case that must NOT be swallowed by the idempotency above.
	unknown := uuid.New().String()
	out, err = runKeys(t, keysRevokeCmd, unknown)
	if err == nil {
		t.Errorf("revoking an unknown id succeeded; it must fail, or a typo'd id reports success:\n%s", out)
	}

	if _, err := runKeys(t, keysRevokeCmd, "not-a-uuid"); err == nil {
		t.Error("a malformed key id was accepted")
	}
}

// firstListedKeyID pulls the id out of the first data row of the listing. It parses the rendered table rather
// than querying the database, so the test proves the id an operator can actually copy off their terminal is
// the one revoke accepts — reading it from the database would prove only that the database is self-consistent.
func firstListedKeyID(t *testing.T, listing string) string {
	t.Helper()
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] == "ID" {
			continue
		}
		if _, err := uuid.Parse(fields[0]); err == nil {
			return fields[0]
		}
	}
	t.Fatalf("no key id in the listing:\n%s", listing)
	return ""
}
