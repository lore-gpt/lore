//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/lore-gpt/lore/core/httpapi"
	"github.com/lore-gpt/lore/core/queue"
	"github.com/lore-gpt/lore/core/store"
	"github.com/lore-gpt/lore/core/store/db"
)

const paradeDBImage = "paradedb/paradedb:0.24.2-pg17"

// failingEnqueuer stands in for a queue whose enqueue fails after the event has
// been inserted, exercising the rollback guarantee.
type failingEnqueuer struct{}

func (failingEnqueuer) EnqueueExtract(context.Context, pgx.Tx, string, string) error {
	return errors.New("simulated enqueue failure")
}

// TestEventsWritePath proves the /v1/events write path against a real ParadeDB:
// the happy path persists the event and enqueues its job in one transaction, and
// a failing enqueue rolls the event back (acceptance criterion 3) — never a
// persisted event without its job.
func TestEventsWritePath(t *testing.T) {
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

	q, err := queue.New(st.Pool)
	if err != nil {
		t.Fatalf("new queue: %v", err)
	}

	runID := seedRun(ctx, t, st.Pool)

	const apiKey = "test-key"
	handler := httpapi.New(httpapi.Config{
		Pool:     st.Pool,
		Enqueuer: q,
		DB:       st,
		Queue:    q,
		APIKey:   apiKey,
		Version:  "test",
	}).Handler()

	// --- Happy path: 202 with a monotonic per-run seq; two event rows, one coalesced extract_run. ---
	body := `{"run_id":"` + runID + `","agent_id":"researcher","payload":{"hello":"world"}}`
	if seq := postEvent(t, handler, apiKey, body); seq != 1 {
		t.Fatalf("first event seq = %d, want 1", seq)
	}
	// A second event on the same run advances the sequence.
	if seq := postEvent(t, handler, apiKey, body); seq != 2 {
		t.Fatalf("second event seq = %d, want 2", seq)
	}
	if got := countEvents(ctx, t, st.Pool); got != 2 {
		t.Fatalf("events after two writes = %d, want 2", got)
	}
	// Extraction is coalesced per run, so the two same-run events enqueue a single extract_run job.
	if got := countRiverJobs(ctx, t, st.Pool); got != 1 {
		t.Fatalf("river_job after two same-run writes = %d, want 1 (coalesced per run)", got)
	}

	// --- Rollback (criterion 3): a failing enqueue rolls back the whole transaction — no new
	// event row, and the run counter is NOT advanced, because seq is assigned in that same
	// transaction (a rolled-back write must burn no sequence number). ---
	beforeSeq := runLastSeq(ctx, t, st.Pool, runID)
	rollbackHandler := httpapi.New(httpapi.Config{
		Pool:     st.Pool,
		Enqueuer: failingEnqueuer{},
		DB:       st,
		Queue:    q,
		APIKey:   apiKey,
		Version:  "test",
	}).Handler()

	rr := do(rollbackHandler, apiKey, body)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("rollback path status = %d, want 500 (body %q)", rr.Code, rr.Body.String())
	}
	if got := countEvents(ctx, t, st.Pool); got != 2 {
		t.Errorf("events after failed enqueue = %d, want 2 (the insert must roll back)", got)
	}
	if got := countRiverJobs(ctx, t, st.Pool); got != 1 {
		t.Errorf("river_job after failed enqueue = %d, want 1 (no new job)", got)
	}
	if after := runLastSeq(ctx, t, st.Pool, runID); after != beforeSeq {
		t.Errorf("runs.last_seq moved from %d to %d on a rolled-back write — seq must not be consumed", beforeSeq, after)
	}

	// --- Unknown run: a well-formed but non-existent run_id maps pgx.ErrNoRows to 400
	// unknown_run. The insert derives project_id from the run, so no matching run means the CTE
	// is empty and nothing is inserted. ---
	unknownBody := `{"run_id":"` + uuid.NewString() + `","agent_id":"researcher","payload":{"x":1}}`
	rr = do(handler, apiKey, unknownBody)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown run status = %d, want 400 (body %q)", rr.Code, rr.Body.String())
	}
	var errResp struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if errResp.Code != "unknown_run" {
		t.Errorf("unknown run error code = %q, want unknown_run (body %q)", errResp.Code, rr.Body.String())
	}
	if got := countEvents(ctx, t, st.Pool); got != 2 {
		t.Errorf("events after unknown-run attempt = %d, want 2 (nothing inserted)", got)
	}
}

// postEvent issues one authenticated POST /v1/events, asserts a 202 with a UUID
// event_id, and returns the server-assigned seq.
func postEvent(t *testing.T, handler http.Handler, apiKey, body string) int64 {
	t.Helper()
	rr := do(handler, apiKey, body)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", rr.Code, rr.Body.String())
	}
	var resp httpapi.CreateEventResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, err := uuid.Parse(resp.EventID); err != nil {
		t.Errorf("event_id %q is not a UUID: %v", resp.EventID, err)
	}
	return resp.Seq
}

// runLastSeq reads a run's last_seq counter directly, for the rollback invariant.
func runLastSeq(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID string) int64 {
	t.Helper()
	var seq int64
	if err := pool.QueryRow(ctx, `SELECT last_seq FROM runs WHERE id = $1::uuid`, runID).Scan(&seq); err != nil {
		t.Fatalf("read run last_seq: %v", err)
	}
	return seq
}

// do issues an authenticated POST /v1/events and returns the recorder.
func do(handler http.Handler, apiKey, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

// seedRun creates the org -> project -> run chain an event needs and returns the
// run id as a canonical UUID string.
func seedRun(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	q := db.New(pool)
	org, err := q.InsertOrganization(ctx, "acme")
	if err != nil {
		t.Fatalf("insert org: %v", err)
	}
	proj, err := q.InsertProject(ctx, db.InsertProjectParams{OrgID: org.ID, Name: "demo"})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	run, err := q.InsertRun(ctx, proj.ID)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return uuid.UUID(run.ID.Bytes).String()
}

func countEvents(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	n, err := db.New(pool).CountAllEvents(ctx)
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

func countRiverJobs(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job").Scan(&n); err != nil {
		t.Fatalf("count river_job: %v", err)
	}
	return n
}
