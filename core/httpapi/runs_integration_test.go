//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/lore-gpt/lore/core/httpapi"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/lore-gpt/lore/core/queue"
	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
)

// TestCreateRunTenantScope proves POST /v1/runs derives the run's project ONLY from the API key: a run
// created with a key is born in that key's project, a body-supplied project_id is ignored (the project can
// never come from the client), a second key's runs land in its own project, and an unknown or revoked key is
// 401. This is the create-side twin of the events cross-tenant guard — a key cannot manufacture a run in a
// project it does not own.
func TestCreateRunTenantScope(t *testing.T) {
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
	if err := queue.Migrate(ctx, st.Pool); err != nil {
		t.Fatalf("river migrate: %v", err)
	}
	q, err := queue.New(st.Pool, tracenoop.NewTracerProvider())
	if err != nil {
		t.Fatalf("new queue: %v", err)
	}

	handler := httpapi.New(httpapi.Config{
		Pool: st.Pool, Enqueuer: q, DB: st, Queue: q, Tenant: st, Version: "test",
	}).Handler()

	// Two tenants, each with its own key. (seedProjectRun also makes a run, unused here.)
	projA, _ := seedProjectRun(ctx, t, st.Pool)
	projB, _ := seedProjectRun(ctx, t, st.Pool)
	keyA, _ := provisionKey(ctx, t, st.Pool, projA)
	keyB, _ := provisionKey(ctx, t, st.Pool, projB)

	// --- Key A creates a run: 201 with a UUID run_id, and the row is born in project A. ---
	runA := createRun(t, handler, keyA, "")
	if got := projectOfRun(ctx, t, st.Pool, runA); got != projA {
		t.Errorf("run created by key A landed in project %s, want %s", got, projA)
	}

	// --- The project comes from the key, never the body: a body naming project B is ignored — the run still
	// lands in key A's project. This closes a spoof where a client tries to plant a run in another tenant. ---
	runSpoof := createRun(t, handler, keyA, `{"project_id":"`+projB+`"}`)
	if got := projectOfRun(ctx, t, st.Pool, runSpoof); got != projA {
		t.Errorf("body-supplied project_id was honored: run landed in %s, want key A's project %s", got, projA)
	}

	// --- A different key's runs land in its own project — proving the project tracks the key, not a constant. ---
	runB := createRun(t, handler, keyB, "")
	if got := projectOfRun(ctx, t, st.Pool, runB); got != projB {
		t.Errorf("run created by key B landed in project %s, want %s", got, projB)
	}

	// --- Unknown and revoked keys are both 401 (no run created). ---
	assertErr(t, doRuns(handler, "lore_sk_not-a-real-key", ""), http.StatusUnauthorized, "unauthorized")
	revoked, revokedID := provisionKey(ctx, t, st.Pool, projA)
	if n, err := db.New(st.Pool).RevokeAPIKey(ctx, mustUUID(t, revokedID)); err != nil || n != 1 {
		t.Fatalf("revoke key: rows=%d err=%v", n, err)
	}
	assertErr(t, doRuns(handler, revoked, ""), http.StatusUnauthorized, "unauthorized")
}

// createRun issues one authenticated POST /v1/runs, asserts a 201 with a UUID run_id and a non-zero
// created_at, and returns the run id.
func createRun(t *testing.T, handler http.Handler, apiKey, body string) string {
	t.Helper()
	rr := doRuns(handler, apiKey, body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %q)", rr.Code, rr.Body.String())
	}
	var resp httpapi.CreateRunResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, err := uuid.Parse(resp.RunID); err != nil {
		t.Errorf("run_id %q is not a UUID: %v", resp.RunID, err)
	}
	if resp.CreatedAt.IsZero() {
		t.Errorf("created_at is the zero time")
	}
	return resp.RunID
}

// doRuns issues a POST /v1/runs with the given bearer key and (optional) body, returning the recorder.
func doRuns(handler http.Handler, apiKey, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

// projectOfRun reads a run's project_id directly, as a canonical UUID string, to prove which tenant owns it.
func projectOfRun(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID string) string {
	t.Helper()
	var pid string
	if err := pool.QueryRow(ctx, `SELECT project_id::text FROM runs WHERE id = $1::uuid`, runID).Scan(&pid); err != nil {
		t.Fatalf("read run project_id: %v", err)
	}
	return pid
}
