// Package telemetry is the binary's composition seam for observability. It owns
// the concrete Prometheus registry, the /metrics HTTP handler, and the OTel
// TracerProvider + OTLP exporter, so the open-core core package sees only the
// typed instrument set (core/metrics), an http.Handler, and a trace.TracerProvider
// — never the registry or the exporters. The registry is process-owned (not the
// global prometheus default) so tests stay isolated.
//
// Tracing is off by default: a real TracerProvider is built only when
// LORE_OTEL_ENABLED is set AND an OTLP endpoint is configured. Everything fails
// open — a missing endpoint or an exporter error leaves a no-op provider and logs,
// never taking the process down.
package telemetry

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/lore-gpt/lore/core/metrics"
)

// Config selects what telemetry to build.
type Config struct {
	// MetricsEnabled turns on the Prometheus registry + /metrics handler. When false,
	// the registry is a no-op and the handler is nil (no /metrics route).
	MetricsEnabled bool
	// OtelEnabled turns on OTel tracing. It still requires an OTLP endpoint to be
	// configured; without one, tracing stays a silent no-op.
	OtelEnabled bool
	// Version and Role stamp the build/liveness gauges and the trace resource. Role
	// is "server" or "worker".
	Version string
	Role    string
}

// Telemetry is the constructed observability surface.
type Telemetry struct {
	// Metrics is the typed instrument set — always non-nil (no-op when disabled).
	Metrics *metrics.Registry
	// MetricsHandler serves /metrics; nil when metrics are disabled.
	MetricsHandler http.Handler
	// Tracer is the OTel TracerProvider spans record against — always non-nil (no-op
	// when tracing is disabled).
	Tracer trace.TracerProvider
	// Shutdown flushes and stops the tracer's exporter within the caller's deadline.
	// It is a no-op when tracing is disabled, so the caller always defers it.
	Shutdown func(context.Context) error
}

// Build constructs the process-owned metrics registry + /metrics handler and the
// OTel TracerProvider. Both are optional and default to no-ops, so a composition
// with no telemetry runs unchanged and exports nothing.
func Build(ctx context.Context, cfg Config) Telemetry {
	t := Telemetry{
		Metrics:  metrics.NewNoop(),
		Tracer:   tracenoop.NewTracerProvider(),
		Shutdown: func(context.Context) error { return nil },
	}
	if cfg.MetricsEnabled {
		t.Metrics, t.MetricsHandler = buildMetrics(cfg)
	}
	buildTracing(ctx, cfg, &t)
	return t
}

func buildMetrics(cfg Config) (*metrics.Registry, http.Handler) {
	reg := prometheus.NewRegistry()
	// Baseline host metrics every /metrics carries: Go runtime (GC, goroutines,
	// memory) and process (CPU, fds, RSS) collectors.
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	m := metrics.New(reg)
	m.BuildInfo.WithLabelValues(cfg.Version, runtime.Version()).Set(1)
	m.Up.WithLabelValues(cfg.Role).Set(1)
	return m, promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}

// buildTracing constructs an OTel TracerProvider + OTLP/HTTP exporter when tracing
// is enabled and an OTLP endpoint is configured. It fails OPEN: a missing endpoint
// or an exporter init error leaves the no-op tracer and logs, never failing the
// boot. The endpoint honours the ecosystem-standard OTEL_EXPORTER_OTLP_* variables
// (read by the exporter itself); we only check that one is set so enabling tracing
// without a collector stays a silent no-op rather than dialling localhost:4318.
func buildTracing(ctx context.Context, cfg Config, t *Telemetry) {
	if !cfg.OtelEnabled {
		return
	}
	endpoint := firstNonEmpty(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"), os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if endpoint == "" {
		slog.WarnContext(ctx, "LORE_OTEL_ENABLED is set but no OTLP endpoint is configured "+
			"(set OTEL_EXPORTER_OTLP_ENDPOINT); tracing stays disabled")
		return
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "otel trace exporter init failed; tracing stays disabled", slog.Any("err", err))
		return
	}
	res, err := resource.New(ctx, resource.WithAttributes(
		attribute.String("service.name", "lore"),
		attribute.String("service.version", cfg.Version),
		attribute.String("service.role", cfg.Role),
	))
	if err != nil {
		res = resource.Default()
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	// The global provider + a W3C propagator: otelriver (API->job) and otelhttp (inbound HTTP) carry trace
	// context through the global TextMapPropagator.
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	t.Tracer = tp
	t.Shutdown = tp.Shutdown
	slog.InfoContext(ctx, "otel tracing enabled", slog.String("otlp_endpoint", endpoint), slog.String("role", cfg.Role))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
