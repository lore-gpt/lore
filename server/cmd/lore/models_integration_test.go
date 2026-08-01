//go:build integration

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
)

// runModelsShow executes `lore models show <project>` with fresh command state, capturing stdout.
func runModelsShow(t *testing.T, projectID string) (string, error) {
	t.Helper()
	cmd := modelsShowCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{projectID})
	err := cmd.Execute()
	return out.String(), err
}

// TestModelsShowReportsTheConfiguredEmbedder pins the command to the deployment it is pointed at.
//
// It used to compose a fixed fixture embedder regardless of configuration, so against a server running a real
// provider it printed the fixture's identity and reported MATCH — while that same project's recall was being
// refused with a model mismatch. A command whose entire purpose is "does the project's pinned model match the
// running embedder" answering confidently in the wrong direction is worse than not having it, so the cases
// below drive both verdicts through the real command against a real database.
func TestModelsShowReportsTheConfiguredEmbedder(t *testing.T) {
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

	// A project pinned to a real provider's vector space — the state a deployment reaches after its first
	// consolidated memory under LORE_EMBEDDING_PROVIDER=openai.
	q := db.New(st.Pool)
	org, err := q.InsertOrganization(ctx, "acme")
	if err != nil {
		t.Fatalf("insert org: %v", err)
	}
	proj, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "pinned"})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	const pinned = "text-embedding-3-small@1536"
	if _, err := st.Pool.Exec(ctx, `UPDATE projects SET active_model_id = $2 WHERE id = $1`, proj.ID, pinned); err != nil {
		t.Fatalf("pin active model: %v", err)
	}
	projectID := uuid.UUID(proj.ID.Bytes).String()

	setOpenAIEnv := func(t *testing.T) {
		t.Helper()
		t.Setenv("LORE_EMBEDDING_PROVIDER", "openai")
		t.Setenv("LORE_EMBEDDING_MODEL", "text-embedding-3-small")
		t.Setenv("LORE_EMBEDDING_DIM", "1536")
	}

	t.Run("a configured provider is reported, and matching is judged against it", func(t *testing.T) {
		t.Setenv("LORE_DATABASE_URL", dsn)
		setOpenAIEnv(t)

		out, err := runModelsShow(t, projectID)
		if err != nil {
			t.Fatalf("models show: %v (out %q)", err, out)
		}
		if !strings.Contains(out, "embedder model: "+pinned) {
			t.Errorf("embedder model line missing %q:\n%s", pinned, out)
		}
		if !strings.Contains(out, "embedder dim:   1536") {
			t.Errorf("embedder dim line wrong:\n%s", out)
		}
		if !strings.Contains(out, "status:         MATCH") {
			t.Errorf("status = not MATCH, but the project is pinned to the configured embedder:\n%s", out)
		}
		// The decisive negative: the OSS default must not appear when a real provider is configured.
		if strings.Contains(out, "fixture-embed-v1") {
			t.Errorf("the fixture embedder was reported despite an openai configuration:\n%s", out)
		}
	})

	t.Run("swapping the embedder away flips the verdict to MISMATCH", func(t *testing.T) {
		t.Setenv("LORE_DATABASE_URL", dsn)
		// No LORE_EMBEDDING_* — the offline default, exactly the state a deployment falls back to when the
		// provider configuration is dropped. The project is still pinned to the real model.
		out, err := runModelsShow(t, projectID)
		if err != nil {
			t.Fatalf("models show: %v (out %q)", err, out)
		}
		if !strings.Contains(out, "status:         MISMATCH") {
			t.Errorf("status = not MISMATCH, but the running embedder no longer matches the pin:\n%s", out)
		}
		if !strings.Contains(out, "active model:   "+pinned) {
			t.Errorf("active model line missing %q:\n%s", pinned, out)
		}
	})

	t.Run("an unpinned project reports UNSET", func(t *testing.T) {
		t.Setenv("LORE_DATABASE_URL", dsn)
		setOpenAIEnv(t)

		fresh, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "unpinned"})
		if err != nil {
			t.Fatalf("insert project: %v", err)
		}
		out, err := runModelsShow(t, uuid.UUID(fresh.ID.Bytes).String())
		if err != nil {
			t.Fatalf("models show: %v (out %q)", err, out)
		}
		if !strings.Contains(out, "status:         UNSET") {
			t.Errorf("status = not UNSET for a project with no active model:\n%s", out)
		}
	})

	// A configuration the server would refuse to start on must not be reported as a comparison against some
	// other embedder: the command fails with the same error instead of inventing a default.
	t.Run("an invalid embedder configuration fails loudly", func(t *testing.T) {
		t.Setenv("LORE_DATABASE_URL", dsn)
		t.Setenv("LORE_EMBEDDING_PROVIDER", "openai") // model and dim deliberately missing
		t.Setenv("LORE_EMBEDDING_MODEL", "")
		t.Setenv("LORE_EMBEDDING_DIM", "")

		out, err := runModelsShow(t, projectID)
		if err == nil {
			t.Fatalf("models show succeeded on an invalid embedder configuration:\n%s", out)
		}
		if strings.Contains(out, "status:") {
			t.Errorf("a verdict was printed despite an unusable embedder configuration:\n%s", out)
		}
	})

}
