// Package telemetry is the binary's composition seam for observability. It owns
// the concrete Prometheus registry and the /metrics HTTP handler, so the
// open-core core package sees only the typed instrument set (core/metrics) and an
// http.Handler — never the registry or the exporter. The registry is
// process-owned (not the global prometheus default) so tests stay isolated and a
// double registration is impossible.
//
// Tracing (an OTel TracerProvider + OTLP exporter) is a later slice; this seam is
// where it will be constructed too, gated on its own enable switch.
package telemetry

import (
	"net/http"
	"runtime"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/lore-gpt/lore/core/metrics"
)

// Config selects what telemetry to build.
type Config struct {
	// MetricsEnabled turns on the Prometheus registry + /metrics handler. When
	// false, Build returns a no-op registry and a nil handler (no /metrics route).
	MetricsEnabled bool
	// Version and Role stamp the constant build/liveness gauges. Role is "server"
	// or "worker".
	Version string
	Role    string
}

// Build constructs the process-owned metrics registry, its typed instrument set,
// and the /metrics HTTP handler. A nil handler means metrics are disabled and the
// route should not be registered. The instrument set is always non-nil (a no-op
// when disabled), so every instrumentation site calls it unconditionally.
func Build(cfg Config) (*metrics.Registry, http.Handler) {
	if !cfg.MetricsEnabled {
		return metrics.NewNoop(), nil
	}
	reg := prometheus.NewRegistry()
	// Baseline host metrics every /metrics carries: Go runtime (GC, goroutines,
	// memory) and process (CPU, fds, RSS) collectors.
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	m := metrics.New(reg)
	// Stamp the process-identity gauges once (constant 1, labels carry the joinable
	// build info and role).
	m.BuildInfo.WithLabelValues(cfg.Version, runtime.Version()).Set(1)
	m.Up.WithLabelValues(cfg.Role).Set(1)
	return m, promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}
