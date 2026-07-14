package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lore-gpt/lore/core/store/db"
	"github.com/lore-gpt/lore/core/workmem"
)

// fakePinger is a Pinger whose health is fixed at construction.
type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

// stubWorkmem is a workmem.Store with a fixed Mode, for asserting how /healthz reports the stripe and that
// its mode never changes the overall status. Set records the last write so a handler test can read it back.
type stubWorkmem struct {
	mode   workmem.Mode
	set    *workmem.Value // last Set value, if a pointer is provided
	key    *workmem.Key
	err    error  // returned by Set to exercise the write-through failure path
	ctxErr *error // if set, records ctx.Err() seen inside Set (to assert caller-cancellation detaching)
}

func (s stubWorkmem) Set(ctx context.Context, k workmem.Key, v workmem.Value) error {
	if s.ctxErr != nil {
		*s.ctxErr = ctx.Err()
	}
	if s.err != nil {
		return s.err
	}
	if s.set != nil {
		*s.set = v
		*s.key = k
	}
	return nil
}
func (stubWorkmem) Get(context.Context, workmem.Key) (workmem.Value, bool, error) {
	return workmem.Value{}, false, nil
}
func (stubWorkmem) GetAll(context.Context, string, string) ([]workmem.Entry, error) { return nil, nil }
func (s stubWorkmem) Mode() workmem.Mode                                            { return s.mode }
func (stubWorkmem) Close()                                                          {}

// fakeEnqueuer records nothing; it is only used where auth/validation reject the
// request before the enqueue would run.
type fakeEnqueuer struct{}

func (fakeEnqueuer) EnqueueExtract(context.Context, pgx.Tx, string, string) error { return nil }

// TestRequireAuthRejectsMalformedToken locks the parse half of the bearer-auth contract without a database: a
// missing header, a wrong scheme, or an empty token is rejected 401 BEFORE any api_keys lookup. The lookup half
// (an unknown or revoked key is 401, a valid key is accepted and scopes the request) needs a real database and
// is covered by the integration test.
func TestRequireAuthRejectsMalformedToken(t *testing.T) {
	handler := New(Config{}).Handler() // no pool: these reject before the lookup would run
	cases := []struct {
		name   string
		header string
	}{
		{"no header", ""},
		{"wrong scheme", "Token s3cret"},
		{"empty token", "Bearer "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/recall", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rr.Code)
			}
		})
	}
}

// TestHealthz checks the /healthz status code and body for both healthy and
// degraded dependencies. It needs no database: health is derived from Pingers.
func TestHealthz(t *testing.T) {
	boom := fakePinger{err: context.DeadlineExceeded}
	ok := fakePinger{}

	cases := []struct {
		name       string
		db, queue  Pinger
		workmem    workmem.Store // nil coerces to disabled
		wantStatus int
		wantBody   Health
	}{
		{"all ok, workmem disabled", ok, ok, nil, http.StatusOK, Health{Status: "ok", Version: "v-test", DB: "ok", Queue: "ok", Workmem: "disabled"}},
		{"db down", boom, ok, nil, http.StatusServiceUnavailable, Health{Status: "degraded", Version: "v-test", DB: "error", Queue: "ok", Workmem: "disabled"}},
		{"queue down", ok, boom, nil, http.StatusServiceUnavailable, Health{Status: "degraded", Version: "v-test", DB: "ok", Queue: "error", Workmem: "disabled"}},
		// The stripe's mode is reported but never drives overall status: a healthy or degraded cache both
		// leave status ok and the code 200 — only DB and Queue can fail the instance.
		{"workmem healthy, status still ok", ok, ok, stubWorkmem{mode: workmem.Healthy}, http.StatusOK, Health{Status: "ok", Version: "v-test", DB: "ok", Queue: "ok", Workmem: "ok"}},
		{"workmem degraded, status still ok", ok, ok, stubWorkmem{mode: workmem.Degraded}, http.StatusOK, Health{Status: "ok", Version: "v-test", DB: "ok", Queue: "ok", Workmem: "degraded"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := New(Config{DB: tc.db, Queue: tc.queue, Version: "v-test", Workmem: tc.workmem}).Handler()
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rr.Code, tc.wantStatus)
			}
			var got Health
			if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if got != tc.wantBody {
				t.Errorf("body = %+v, want %+v", got, tc.wantBody)
			}
		})
	}
}

// TestCreateEventValidation covers the 400 paths that reject before any database
// work, so it runs without a pool. A malformed kind:"state" payload is rejected
// here too (invalid_state_fact), before the event is written. The happy path and
// the rollback guarantee are covered by the integration test.
func TestCreateEventValidation(t *testing.T) {
	// Call the handler directly (not through the router): these bodies are rejected before any auth lookup or
	// database work, so the test needs neither a key nor a pool.
	a := New(Config{Enqueuer: fakeEnqueuer{}})

	const runID = "11111111-1111-1111-1111-111111111111"
	state := func(payload string) string {
		return `{"run_id":"` + runID + `","agent_id":"a","payload":` + payload + `}`
	}
	cases := []struct {
		name     string
		body     string
		wantCode string // asserted when non-empty
	}{
		{name: "malformed json", body: "{"},
		{name: "missing run_id", body: `{"agent_id":"a","payload":{}}`},
		{name: "bad run_id", body: `{"run_id":"not-a-uuid","agent_id":"a","payload":{}}`},
		{name: "missing agent_id", body: `{"run_id":"` + runID + `","payload":{}}`},
		{name: "payload not object", body: `{"run_id":"` + runID + `","agent_id":"a","payload":[1,2]}`},
		{name: "payload missing", body: `{"run_id":"` + runID + `","agent_id":"a"}`},
		// A malformed state fact is rejected loudly at the door with a stable code.
		{name: "state missing entity", body: state(`{"kind":"state","predicate":"p","value":1}`), wantCode: "invalid_state_fact"},
		{name: "state missing value", body: state(`{"kind":"state","entity":"e","predicate":"p"}`), wantCode: "invalid_state_fact"},
		{name: "state control char in entity", body: state(`{"kind":"state","entity":"a\tb","predicate":"p","value":1}`), wantCode: "invalid_state_fact"},
		{name: "state entity not a string", body: state(`{"kind":"state","entity":5,"predicate":"p","value":1}`), wantCode: "invalid_state_fact"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(tc.body))
			rr := httptest.NewRecorder()
			a.handleCreateEvent(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", rr.Code, tc.body)
			}
			if tc.wantCode != "" {
				var e Error
				if err := json.NewDecoder(rr.Body).Decode(&e); err != nil {
					t.Fatalf("decode error body: %v", err)
				}
				if e.Code != tc.wantCode {
					t.Errorf("error code = %q, want %q (body %q)", e.Code, tc.wantCode, rr.Body.String())
				}
			}
		})
	}
}

// TestWriteThroughStateDetachesFromCallerCancellation proves a client that disconnects (cancelling the
// request context) does not cancel the post-commit best-effort write-through. Otherwise one caller's abort
// would surface as a Set error and poison the shared stripe health for every concurrent run.
func TestWriteThroughStateDetachesFromCallerCancellation(t *testing.T) {
	var sawErr error
	a := New(Config{Workmem: stubWorkmem{mode: workmem.Healthy, ctxErr: &sawErr}})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client has already disconnected before the write-through runs

	event := db.Event{
		ProjectID: pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
		RunID:     pgtype.UUID{Bytes: [16]byte{2}, Valid: true},
		Seq:       1,
		AgentID:   "planner",
	}
	a.writeThroughState(ctx, event, workmem.StateFact{Entity: "e", Predicate: "p", Value: []byte(`"v"`)})

	if sawErr != nil {
		t.Errorf("write-through Set saw ctx.Err() = %v, want nil — a caller cancellation must be detached", sawErr)
	}
}

// TestCreateEventStateValueLimit proves the configurable value cap is enforced at ingestion: a value over
// the limit is rejected with invalid_state_fact before any database work.
func TestCreateEventStateValueLimit(t *testing.T) {
	a := New(Config{Enqueuer: fakeEnqueuer{}, WorkmemMaxValueBytes: 4})

	const runID = "11111111-1111-1111-1111-111111111111"
	body := `{"run_id":"` + runID + `","agent_id":"a","payload":{"kind":"state","entity":"e","predicate":"p","value":"way too long"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(body))
	rr := httptest.NewRecorder()
	a.handleCreateEvent(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	var e Error
	if err := json.NewDecoder(rr.Body).Decode(&e); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if e.Code != "invalid_state_fact" {
		t.Errorf("error code = %q, want invalid_state_fact", e.Code)
	}
}
