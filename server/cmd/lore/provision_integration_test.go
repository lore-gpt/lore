//go:build integration

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
)

const paradeDBImage = "paradedb/paradedb:0.24.2-pg17"

// runProvision executes `lore provision --out path` with fresh command state, capturing stdout and stderr.
func runProvision(t *testing.T, path string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := provisionCmd()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{"--out", path})
	err = cmd.Execute()
	return out.String(), errBuf.String(), err
}

// TestProvisionVerifyThenHeal proves the credentials file-guard checks the database, not just the file: a
// second `up` with the project still present is a verified no-op, a wiped database (the `docker compose
// down -v` trap) heals loudly by backing the stale file up to .bak and provisioning a fresh project, and an
// unreachable database fails loudly without touching the credentials — so a transient outage can never heal
// over good keys.
func TestProvisionVerifyThenHeal(t *testing.T) {
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

	t.Run("heals after the database is wiped", func(t *testing.T) {
		t.Setenv("LORE_DATABASE_URL", dsn)
		path := filepath.Join(t.TempDir(), ".lore", "credentials")

		// First provision writes credentials for a project.
		if _, _, err := runProvision(t, path); err != nil {
			t.Fatalf("first provision: %v", err)
		}
		firstID, ok := readProjectID(path)
		if !ok {
			t.Fatal("could not read a project id from the first credentials file")
		}

		// A second provision, project still present, is a verified no-op: file unchanged, no .bak.
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read credentials: %v", err)
		}
		if _, stderr, err := runProvision(t, path); err != nil {
			t.Fatalf("no-op provision: %v", err)
		} else if !strings.Contains(stderr, "already provisioned") {
			t.Errorf("no-op stderr = %q, want it to say 'already provisioned'", stderr)
		}
		if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
			t.Error("a verified no-op rewrote the credentials file")
		}
		if _, statErr := os.Stat(path + ".bak"); !os.IsNotExist(statErr) {
			t.Error("a verified no-op created a .bak")
		}

		// Wipe the project — what `docker compose down -v` does to the database while the host file lingers.
		if _, err := st.Pool.Exec(ctx, `DELETE FROM api_keys WHERE project_id = $1`, firstID); err != nil {
			t.Fatalf("delete api_keys: %v", err)
		}
		if _, err := st.Pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, firstID); err != nil {
			t.Fatalf("delete project: %v", err)
		}
		staleCreds, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read stale credentials: %v", err)
		}

		// The next provision heals: warns, backs the stale file up, writes fresh credentials.
		_, stderr, err := runProvision(t, path)
		if err != nil {
			t.Fatalf("heal provision: %v", err)
		}
		if !strings.Contains(stderr, "previous project not found") {
			t.Errorf("heal stderr = %q, want the 'previous project not found' warning", stderr)
		}
		// The old credentials survive at .bak, byte-identical — no key material is destroyed.
		if bak, err := os.ReadFile(path + ".bak"); err != nil {
			t.Errorf("read .bak: %v", err)
		} else if !bytes.Equal(bak, staleCreds) {
			t.Error(".bak does not match the pre-heal credentials")
		}
		// The fresh credentials name a different project that actually exists.
		newID, ok := readProjectID(path)
		if !ok {
			t.Fatal("could not read a project id from the healed credentials file")
		}
		if newID == firstID {
			t.Error("heal reused the wiped project id instead of minting a fresh one")
		}
		if exists, err := db.New(st.Pool).ProjectExists(ctx, newID); err != nil {
			t.Fatalf("ProjectExists: %v", err)
		} else if !exists {
			t.Error("the healed project does not exist in the database")
		}

		// A second wipe + heal keeps a single .bak, overwriting the first — and must work even though a
		// .bak already exists (os.Rename does not replace an existing file on every platform).
		if _, err := st.Pool.Exec(ctx, `DELETE FROM api_keys WHERE project_id = $1`, newID); err != nil {
			t.Fatalf("delete api_keys (second wipe): %v", err)
		}
		if _, err := st.Pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, newID); err != nil {
			t.Fatalf("delete project (second wipe): %v", err)
		}
		secondStale, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read second stale credentials: %v", err)
		}
		if _, stderr, err := runProvision(t, path); err != nil {
			t.Fatalf("second heal: %v", err)
		} else if !strings.Contains(stderr, "previous project not found") {
			t.Errorf("second heal stderr = %q, want the 'previous project not found' warning", stderr)
		}
		if bak, err := os.ReadFile(path + ".bak"); err != nil {
			t.Errorf("read .bak after second heal: %v", err)
		} else if !bytes.Equal(bak, secondStale) {
			t.Error(".bak was not overwritten with the second generation of credentials")
		}
	})

	t.Run("leaves credentials untouched when the database is unreachable", func(t *testing.T) {
		// Nothing listens on port 1; connect_timeout bounds the failure so the test stays fast.
		t.Setenv("LORE_DATABASE_URL", "postgres://lore:nope@127.0.0.1:1/lore?sslmode=disable&connect_timeout=1")
		path := filepath.Join(t.TempDir(), ".lore", "credentials")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		content := "LORE_PROJECT_ID=" + uuid.NewString() + "\nLORE_API_KEY=lore_sk_placeholder\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write credentials: %v", err)
		}

		if _, _, err := runProvision(t, path); err == nil {
			t.Fatal("provision succeeded with an unreachable database — expected a loud failure")
		}
		if after, _ := os.ReadFile(path); string(after) != content {
			t.Error("credentials were modified even though the database was unreachable")
		}
		if _, statErr := os.Stat(path + ".bak"); !os.IsNotExist(statErr) {
			t.Error("a .bak was created even though the database was unreachable")
		}
	})
}
