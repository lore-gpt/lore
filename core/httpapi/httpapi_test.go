package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// fakePinger is a Pinger whose health is fixed at construction.
type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

// fakeEnqueuer records nothing; it is only used where auth/validation reject the
// request before the enqueue would run.
type fakeEnqueuer struct{}

func (fakeEnqueuer) EnqueueExtract(context.Context, pgx.Tx, string, string) error { return nil }

// TestRequireAuth locks the bearer-auth contract without a database. It probes
// via /v1/recall, which passes through auth and then returns 501 — so a 401
// means auth rejected and a 501 means auth accepted.
func TestRequireAuth(t *testing.T) {
	handler := New(Config{APIKey: "s3cret"}).Handler()

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong scheme", "Token s3cret", http.StatusUnauthorized},
		{"empty token", "Bearer ", http.StatusUnauthorized},
		{"wrong key", "Bearer nope", http.StatusUnauthorized},
		{"correct key", "Bearer s3cret", http.StatusNotImplemented},
		{"scheme case-insensitive", "bearer s3cret", http.StatusNotImplemented},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/recall", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Errorf("status = %d, want %d", rr.Code, tc.want)
			}
		})
	}
}

// TestRequireAuthUnconfiguredFailsClosed proves an empty configured key rejects
// every request rather than accepting an empty token.
func TestRequireAuthUnconfiguredFailsClosed(t *testing.T) {
	handler := New(Config{APIKey: ""}).Handler()
	for _, header := range []string{"Bearer ", "Bearer anything"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/recall", nil)
		req.Header.Set("Authorization", header)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("header %q: status = %d, want 401", header, rr.Code)
		}
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
		wantStatus int
		wantBody   Health
	}{
		{"all ok", ok, ok, http.StatusOK, Health{Status: "ok", Version: "v-test", DB: "ok", Queue: "ok"}},
		{"db down", boom, ok, http.StatusServiceUnavailable, Health{Status: "degraded", Version: "v-test", DB: "error", Queue: "ok"}},
		{"queue down", ok, boom, http.StatusServiceUnavailable, Health{Status: "degraded", Version: "v-test", DB: "ok", Queue: "error"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := New(Config{DB: tc.db, Queue: tc.queue, Version: "v-test"}).Handler()
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
// work, so it runs without a pool. The happy path and the rollback guarantee are
// covered by the integration test.
func TestCreateEventValidation(t *testing.T) {
	handler := New(Config{APIKey: "s3cret", Enqueuer: fakeEnqueuer{}}).Handler()

	const runID = "11111111-1111-1111-1111-111111111111"
	cases := []struct {
		name string
		body string
	}{
		{"malformed json", "{"},
		{"missing run_id", `{"agent_id":"a","payload":{}}`},
		{"bad run_id", `{"run_id":"not-a-uuid","agent_id":"a","payload":{}}`},
		{"missing agent_id", `{"run_id":"` + runID + `","payload":{}}`},
		{"payload not object", `{"run_id":"` + runID + `","agent_id":"a","payload":[1,2]}`},
		{"payload missing", `{"run_id":"` + runID + `","agent_id":"a"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer s3cret")
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body %q)", rr.Code, tc.body)
			}
		})
	}
}
