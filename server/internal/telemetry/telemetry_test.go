package telemetry

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildEnabledServesMetricsWithBuildInfo(t *testing.T) {
	m, h := Build(Config{MetricsEnabled: true, Version: "v-test", Role: "server"})
	if m == nil || h == nil {
		t.Fatal("enabled build should return a registry and a handler")
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()
	// The process-identity gauge is stamped at construction, with the version label.
	if !strings.Contains(body, `lore_build_info{go_version=`) || !strings.Contains(body, `version="v-test"`) {
		t.Errorf("build_info not stamped in /metrics output:\n%s", body)
	}
	if !strings.Contains(body, `lore_up{role="server"}`) {
		t.Error("lore_up{role=server} not exported")
	}
	// The baseline Go collector is registered.
	if !strings.Contains(body, "go_goroutines") {
		t.Error("Go runtime collector not registered")
	}
}

func TestBuildDisabledIsNoopWithNoHandler(t *testing.T) {
	m, h := Build(Config{MetricsEnabled: false})
	if m == nil {
		t.Fatal("disabled build must still return a non-nil no-op registry (unconditional call sites)")
	}
	if h != nil {
		t.Error("disabled build must return a nil handler so /metrics is not registered")
	}
	// The no-op registry accepts calls without panicking.
	m.HTTPInFlight.Inc()
}
