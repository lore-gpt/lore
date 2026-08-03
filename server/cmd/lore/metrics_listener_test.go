package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// freeAddr reserves a loopback port and releases it, returning an address the caller can bind. Binding
// something real is the point of these tests — a listener that never binds proves nothing — and a fixed
// port would make them fail on a machine already using it.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return addr
}

// get retries briefly, because the listener starts in a goroutine and the first attempt can beat it.
func get(t *testing.T, url string) (*http.Response, error) {
	t.Helper()
	var lastErr error
	for range 50 {
		resp, err := http.Get(url) //nolint:noctx // short-lived probe against a local test listener
		if err == nil {
			return resp, nil
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	return nil, lastErr
}

// TestServeMetricsListensOnItsOwnAddress proves the endpoint is reachable on the address the operator
// configures, separately from any API listener.
//
// That separation is the whole point of this function: /metrics is unauthenticated, so while it shared the
// server's API listener there was no way to publish the API without publishing it too — the deployment
// advice this project gives could not actually be followed. On its own port it can simply stay unpublished.
//
// Scope of these tests, stated plainly: they cover the listener, not the wiring. Whether `serve` and `worker`
// actually call it is not asserted here — both commands boot a real database and queue, which is not
// something this package can stand up. That half is verified by running the stack: the API port answers 404
// for /metrics while the metrics port serves it, for both services.
func TestServeMetricsListensOnItsOwnAddress(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	serveMetrics(ctx, "server", addr, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "lore_up 1\n")
	}))

	resp, err := get(t, "http://"+addr+"/metrics")
	if err != nil {
		t.Fatalf("GET /metrics on the metrics listener: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "lore_up") {
		t.Errorf("body = %q, want the handler's output", body)
	}
}

// TestServeMetricsStopsWithItsContext proves the listener is tied to the process's lifetime rather than
// outliving it. It runs on its own port, so nothing else would notice a listener that kept the address after
// shutdown — the next start would just fail to bind, at which point the cause is several minutes away from
// the symptom.
func TestServeMetricsStopsWithItsContext(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(t.Context())

	serveMetrics(ctx, "server", addr, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "lore_up 1\n")
	}))

	resp, err := get(t, "http://"+addr+"/metrics")
	if err != nil {
		t.Fatalf("precondition: the listener never came up: %v", err)
	}
	_ = resp.Body.Close()

	cancel()

	// Poll until it stops answering: shutdown is asynchronous, so a single immediate probe would be racing it.
	stopped := false
	for range 100 {
		if r, err := http.Get("http://" + addr + "/metrics"); err != nil { //nolint:noctx // local probe
			stopped = true
			break
		} else {
			_ = r.Body.Close()
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !stopped {
		t.Error("the listener still answers after its context was cancelled; it must not outlive the process")
	}
}

// TestServeMetricsIsANoopWhenDisabled proves that turning metrics off leaves no listener at all, rather than
// one serving an empty page — otherwise a port would be held open for nothing.
func TestServeMetricsIsANoopWhenDisabled(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	serveMetrics(ctx, "server", addr, nil) // nil handler is what telemetry.Build returns when disabled

	// One quick attempt, not the retrying probe: here the expectation is that nothing ever comes up.
	time.Sleep(100 * time.Millisecond)
	if resp, err := http.Get("http://" + addr + "/metrics"); err == nil { //nolint:noctx // local probe
		_ = resp.Body.Close()
		t.Fatalf("something answered on %s; a disabled metrics surface must not bind at all", addr)
	}
}

// recordingHandler captures slog records under a mutex, so a test can assert what an operator would see
// without racing the goroutine that writes them.
type recordingHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r.Clone())
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

// firstError waits briefly for an error-level record and returns it. The wait is what synchronises with the
// listener goroutine: without it the test can finish before the failed bind has been reported at all.
func (h *recordingHandler) firstError(t *testing.T) slog.Record {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		for _, r := range h.recs {
			if r.Level == slog.LevelError {
				rec := r.Clone()
				h.mu.Unlock()
				return rec
			}
		}
		h.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no error-level record for the failed bind; an operator would have no idea why their dashboard is empty")
	return slog.Record{}
}

func recordAttrs(r slog.Record) map[string]string {
	out := make(map[string]string, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		out[a.Key] = a.Value.String()
		return true
	})
	return out
}

// TestServeMetricsSurvivesABindConflict pins the loud-but-NOT-fatal contract, both halves.
//
// Both commands default to the same metrics address, so running the server and the worker on one host is a
// real way to hit an address already in use — this change is what created that possibility, so it owes the
// operator a clear answer. Two things must hold, and each needs its own assertion:
//
//   - NOT fatal: losing the observability surface must not take down the process it observes.
//   - Loud: the failure is reported at error level, carrying the fields an operator acts on.
//
// Waiting for that record is also what makes the "not fatal" half meaningful. serveMetrics does all its work
// in goroutines, so it returns immediately no matter what happens next — a test that only checks it returned
// would pass even if the listener never bound, even if the failure were swallowed, and even if the failure
// killed the process, because none of that has happened yet when the call returns. Blocking until the error
// surfaces moves the test into the window where those outcomes are observable.
//
// The assertions are on the structured attributes, never on the error text: the bind failure comes from the
// operating system and is localized, so matching its message would pass or fail depending on the machine's
// language.
func TestServeMetricsSurvivesABindConflict(t *testing.T) {
	occupied := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer occupied.Close()
	addr := strings.TrimPrefix(occupied.URL, "http://")

	h := &recordingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h)) // process-global: this test must not run in parallel
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		serveMetrics(ctx, "worker", addr, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serveMetrics blocked on a bind conflict; it must return and let the process continue")
	}

	attrs := recordAttrs(h.firstError(t))
	if attrs["role"] != "worker" {
		t.Errorf("role = %q, want worker — without it an operator cannot tell which process lost its metrics", attrs["role"])
	}
	if attrs["addr"] != addr {
		t.Errorf("addr = %q, want %q — the address is what makes the report actionable", attrs["addr"], addr)
	}
	if attrs["hint"] == "" {
		t.Error("hint is empty; the report should say what to do about a clash, not only that one happened")
	}

	// Not fatal, observed rather than assumed: the process is still here, and it did not disturb the
	// occupant it collided with.
	resp, err := get(t, occupied.URL)
	if err != nil {
		t.Fatalf("the address's original owner stopped answering: %v", err)
	}
	_ = resp.Body.Close()
}
