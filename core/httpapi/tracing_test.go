package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// newRecordingAPI builds an API whose HTTP spans are captured in-memory, returning the handler and the recorder.
func newRecordingAPI(t *testing.T, cfg Config) (http.Handler, *tracetest.SpanRecorder) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	cfg.Tracer = tp
	return New(cfg).Handler(), sr
}

// spanAttr returns the string value of the named attribute on a span, and whether it was present.
func spanAttr(span sdktrace.ReadOnlySpan, key string) (string, bool) {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsString(), true
		}
	}
	return "", false
}

// TestHTTPSpanNamedByRouteTemplate proves the server span carries the low-cardinality route template, not the raw
// path: after chi matches, traceRoute renames the span "METHOD {route}" and tags http.route. It also pins the
// request-id correlator is attached. A route with no auth header short-circuits 401 before any database lookup, so
// the span is fully formed without a pool — the route is matched regardless of the auth outcome.
func TestHTTPSpanNamedByRouteTemplate(t *testing.T) {
	handler, sr := newRecordingAPI(t, Config{})

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/events", nil))

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want exactly 1 for a matched route", len(spans))
	}
	span := spans[0]
	if got, want := span.Name(), "POST /v1/events"; got != want {
		t.Errorf("span name = %q, want %q (METHOD + route template, not the raw path)", got, want)
	}
	if route, ok := spanAttr(span, "http.route"); !ok || route != "/v1/events" {
		t.Errorf("http.route attr = %q (present=%v), want %q", route, ok, "/v1/events")
	}
	if id, ok := spanAttr(span, "request_id"); !ok || id == "" {
		t.Errorf("request_id attr = %q (present=%v), want a non-empty correlator", id, ok)
	}
}

// TestHTTPSpanExcludesHealthAndMetrics proves the otelhttp filter keeps infrastructure polling out of the trace
// backend: neither /healthz nor /metrics opens a span, so a Prometheus scrape or a liveness probe every few
// seconds never floods traces. A business route in the same handler still records one span (the control).
func TestHTTPSpanExcludesHealthAndMetrics(t *testing.T) {
	// /metrics only registers when a handler is wired, so give it a real promhttp handler.
	handler, sr := newRecordingAPI(t, Config{
		DB:             fakePinger{},
		Queue:          fakePinger{},
		Version:        "v-test",
		MetricsHandler: promhttp.Handler(),
	})

	for _, path := range []string{"/healthz", "/metrics"} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	}
	if n := len(sr.Ended()); n != 0 {
		t.Fatalf("recorded %d spans for /healthz+/metrics, want 0 (both are filtered)", n)
	}

	// Control: a non-excluded route in the same handler DOES record a span, so the zero above is the filter at
	// work, not tracing being off entirely.
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/events", nil))
	if n := len(sr.Ended()); n != 1 {
		t.Errorf("recorded %d spans after one business request, want 1", n)
	}
}

// TestHTTPSpanNeverCapturesAuthorization is the content-hygiene guard: the Authorization header — or any header —
// must never land in a span attribute, so a bearer token cannot leak into the trace backend. A request with a
// present-but-wrong scheme is rejected 401 before any lookup, yet the secret-looking credential must appear in no
// attribute of the recorded span.
func TestHTTPSpanNeverCapturesAuthorization(t *testing.T) {
	handler, sr := newRecordingAPI(t, Config{})

	const secret = "sk_live_TOPSECRET_must_not_leak"
	req := httptest.NewRequest(http.MethodPost, "/v1/events", nil)
	req.Header.Set("Authorization", "Secret "+secret) // wrong scheme -> 401 before the api_keys lookup
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(spans))
	}
	for _, kv := range spans[0].Attributes() {
		if strings.Contains(kv.Value.AsString(), secret) || strings.Contains(strings.ToLower(string(kv.Key)), "authorization") {
			t.Errorf("span attribute %s=%q leaked the Authorization credential", kv.Key, kv.Value.AsString())
		}
	}
}
