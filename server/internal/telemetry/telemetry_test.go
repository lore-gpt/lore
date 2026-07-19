package telemetry

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func TestBuildEnabledServesMetricsWithBuildInfo(t *testing.T) {
	tel := Build(context.Background(), Config{MetricsEnabled: true, Version: "v-test", Role: "server"})
	if tel.Metrics == nil || tel.MetricsHandler == nil {
		t.Fatal("enabled build should return a registry and a handler")
	}
	rr := httptest.NewRecorder()
	tel.MetricsHandler.ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()
	// The process-identity gauge is stamped at construction, with the version label.
	if !strings.Contains(body, `lore_build_info{go_version=`) || !strings.Contains(body, `version="v-test"`) {
		t.Errorf("build_info not stamped in /metrics output:\n%s", body)
	}
	if !strings.Contains(body, `lore_up{role="server"}`) {
		t.Error("lore_up{role=server} not exported")
	}
	if !strings.Contains(body, "go_goroutines") {
		t.Error("Go runtime collector not registered")
	}
}

func TestBuildDisabledIsNoopWithNoHandler(t *testing.T) {
	tel := Build(context.Background(), Config{MetricsEnabled: false})
	if tel.Metrics == nil {
		t.Fatal("disabled build must still return a non-nil no-op registry (unconditional call sites)")
	}
	if tel.MetricsHandler != nil {
		t.Error("disabled build must return a nil handler so /metrics is not registered")
	}
	tel.Metrics.HTTPInFlight.Inc() // the no-op registry accepts calls without panicking
}

// TestTracingDisabledByDefault proves tracing is off unless explicitly enabled: the tracer is the no-op
// provider and Shutdown is a safe no-op that callers always defer.
func TestTracingDisabledByDefault(t *testing.T) {
	tel := Build(context.Background(), Config{MetricsEnabled: false, OtelEnabled: false})
	if _, ok := tel.Tracer.(tracenoop.TracerProvider); !ok {
		t.Errorf("tracing off should leave the no-op TracerProvider, got %T", tel.Tracer)
	}
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Errorf("no-op shutdown should not error: %v", err)
	}
}

// TestTracingEnabledWithoutEndpointStaysNoop proves the fail-open contract: LORE_OTEL_ENABLED without an OTLP
// endpoint stays a silent no-op (no panic, no dial), rather than spamming localhost.
func TestTracingEnabledWithoutEndpointStaysNoop(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	tel := Build(context.Background(), Config{OtelEnabled: true, Role: "server"})
	if _, ok := tel.Tracer.(tracenoop.TracerProvider); !ok {
		t.Errorf("enabled without an endpoint should stay no-op, got %T", tel.Tracer)
	}
}
