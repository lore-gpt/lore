// Package httpapi is the Lore HTTP surface: a chi router with request id, panic
// recovery, structured access logs, and bearer auth, wired to the store and the
// job queue. It is the source-of-truth implementation of spec/openapi.yaml:
// health, event ingest, and the context-pack read path (with later surfaces
// stubbed at 501).
//
// Dependencies arrive as interfaces (Enqueuer, Pinger) rather than concrete
// types so the composition root wires the real store/queue while tests can
// substitute fakes — in particular a failing Enqueuer to prove the event insert
// and its extraction job commit or roll back as one unit.
package httpapi

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/lore-gpt/lore/core/metrics"
	"github.com/lore-gpt/lore/core/pack"
	"github.com/lore-gpt/lore/core/workmem"
)

// Enqueuer schedules the follow-up extraction for an accepted event. It runs on
// the caller's transaction so the enqueue is atomic with the event insert: if
// either fails, neither is durable. Extraction is coalesced per run, so the
// enqueue is keyed on the event's project and run, not the event id. *queue.Queue
// satisfies it.
type Enqueuer interface {
	EnqueueExtract(ctx context.Context, tx pgx.Tx, projectID, runID string) error
}

// Pinger reports a dependency's health for /healthz. *store.Store and
// *queue.Queue both satisfy it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Packer builds a context pack for a run on the caller's tenant transaction. *pack.Pack satisfies it. A nil
// Packer leaves /v1/pack a 501 (the endpoint composes only when the read path is wired in).
type Packer interface {
	Build(ctx context.Context, tx pgx.Tx, projectID, runID pgtype.UUID, req pack.Request) (pack.Result, error)
}

// Tenant runs a function inside a project-scoped (row-level-security) transaction, so the pack handler's reads
// and its trace write are all scoped to one project. *store.Store satisfies it.
type Tenant interface {
	WithProject(ctx context.Context, projectID pgtype.UUID, fn func(pgx.Tx) error) error
}

// Config carries the API's dependencies. Handlers touch only the fields their
// route needs, so narrower tests may leave the rest nil.
type Config struct {
	Pool     *pgxpool.Pool // begins the event write transaction; also resolves bearer keys for auth
	Enqueuer Enqueuer      // enqueues extraction on that transaction
	DB       Pinger        // /healthz database probe
	Queue    Pinger        // /healthz queue probe
	Packer   Packer        // builds a context pack for /v1/pack (nil leaves it 501)
	Tenant   Tenant        // opens the pack handler's project-scoped transaction
	Version  string        // reported by /healthz
	// Workmem is the working-memory store: a kind:"state" event is written through to it after commit,
	// and /healthz reports its mode. A nil value coerces to the disabled no-op.
	Workmem workmem.Store
	// WorkmemMaxValueBytes bounds a state fact's value at ingestion; 0 uses the package default.
	WorkmemMaxValueBytes int
	// EmbedderID is the composed embedder's model@dim identity, reported by /healthz so an operator can
	// confirm the server and worker share one vector space. Empty is reported as an empty string.
	EmbedderID string
	// ExtractionID is the provider/model identity distilling this deployment's memories, reported by
	// /healthz. Unlike EmbedderID it is not composed here — extraction runs in the worker — so the caller
	// supplies what the deployment is configured for. Empty is reported as an empty string.
	ExtractionID string
	// Metrics is the Prometheus instrument set the HTTP middleware observes into. A nil value coerces to a
	// no-op registry so the middleware runs unconditionally.
	Metrics *metrics.Registry
	// Tracer is the OTel TracerProvider the HTTP server root span is recorded against. A nil value coerces to
	// a no-op provider, so the server runs unchanged with tracing off (the default).
	Tracer trace.TracerProvider
}

// API holds the wired dependencies and builds the router.
type API struct {
	pool                 *pgxpool.Pool
	enqueuer             Enqueuer
	db                   Pinger
	queue                Pinger
	packer               Packer
	tenant               Tenant
	version              string
	workmem              workmem.Store
	workmemMaxValueBytes int
	embedderID           string
	extractionID         string
	metrics              *metrics.Registry
	tracer               trace.TracerProvider
}

// New returns an API bound to cfg. A nil Workmem coerces to the disabled no-op so
// handlers never hold a nil store.
func New(cfg Config) *API {
	wm := cfg.Workmem
	if wm == nil {
		wm = workmem.NewDisabled()
	}
	m := cfg.Metrics
	if m == nil {
		m = metrics.NewNoop()
	}
	tp := cfg.Tracer
	if tp == nil {
		tp = tracenoop.NewTracerProvider()
	}
	return &API{
		pool:                 cfg.Pool,
		enqueuer:             cfg.Enqueuer,
		db:                   cfg.DB,
		queue:                cfg.Queue,
		packer:               cfg.Packer,
		tenant:               cfg.Tenant,
		version:              cfg.Version,
		workmem:              wm,
		workmemMaxValueBytes: cfg.WorkmemMaxValueBytes,
		embedderID:           cfg.EmbedderID,
		extractionID:         cfg.ExtractionID,
		metrics:              m,
		tracer:               tp,
	}
}

// Handler builds the chi router. Request id, panic recovery, and access logging
// wrap every route; bearer auth wraps only the /v1 group. /healthz is
// deliberately unauthenticated so orchestrators can probe it.
func (a *API) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(a.logRequests)
	r.Use(a.recordMetrics)
	r.Use(a.traceRoute)

	// The router's own rejections use the same error envelope as every handled error. Without this the two
	// ways to miss the API are the only responses on this surface a client cannot parse: the router's default
	// 404 is text/plain prose, and its default 405 is a bare status with no body and no content type at all.
	// A client that decodes {code, message} — which is every SDK here — then fails to decode rather than
	// reporting the mistake, so a path typo or a wrong method surfaces as a parse error instead of an answer.
	r.NotFound(a.handleUnknownRoute)
	r.MethodNotAllowed(a.handleMethodNotAllowed)

	r.Get("/healthz", a.handleHealthz)

	// /metrics is deliberately NOT here. It is unauthenticated, so serving it on this router would tie it to
	// the API's listener and make it impossible to publish the API without publishing it too. The binary
	// serves it on a listener of its own instead, which can stay unpublished.

	r.Group(func(r chi.Router) {
		r.Use(a.requireAuth)
		r.Post("/v1/events", a.handleCreateEvent)
		r.Post("/v1/runs", a.handleCreateRun)
		r.Post("/v1/pack", a.handlePack)

		// Contracts that exist but land in a later increment answer 501 (not the router's 404), so a client
		// sees the endpoint is real but unfinished. Registering them keeps the surface honest.
		r.Get("/v1/memories", a.handleListMemories)
		r.Get("/v1/memories/{id}", a.handleGetMemory)
		r.Delete("/v1/memories/{id}", a.handleDeleteMemory)
		r.Get("/v1/memories/{id}/versions", a.handleListMemoryVersions)
		r.Get("/v1/runs/{id}/trace", a.handleGetRunTrace)

		// Direct memory create/update and the policy surface exist in the spec but land in a later increment.
		r.Post("/v1/memories", a.notImplemented)
		r.Patch("/v1/memories/{id}", a.notImplemented)
		r.Get("/v1/policies", a.notImplemented)
	})

	// Wrap the whole router in the OTel HTTP instrumentation: it opens the server root span (extracting any
	// propagated trace context from the caller's headers), records method/target/status, and — by default —
	// captures no request or response headers, so the Authorization bearer token cannot leak into a span.
	// /healthz and /metrics are filtered out so infrastructure polling never floods the trace backend. The
	// inner traceRoute middleware renames the span to "METHOD {route}" and tags http.route once chi has
	// matched, keeping the span name low-cardinality (the {id} template, never the raw path). With tracing
	// off (the no-op default) NewHandler adds no measurable overhead and exports nothing.
	return otelhttp.NewHandler(r, "http.server",
		otelhttp.WithTracerProvider(a.tracer),
		otelhttp.WithFilter(func(req *http.Request) bool {
			return req.URL.Path != "/healthz" && req.URL.Path != "/metrics"
		}),
	)
}

// handleUnknownRoute answers a path that matches no endpoint. Its code is deliberately distinct from the
// not_found a handler returns for a missing resource: those mean "the thing you named does not exist, or is
// not yours", while this means "that URL is not an endpoint at all". A client hitting the second usually has
// a typo or is speaking to a version that does not have the route, and telling the two apart is the whole
// value of a machine-readable code. Route existence is not a secret — the endpoints are published in the
// API description — so this reveals nothing a caller could not already read.
func (a *API) handleUnknownRoute(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusNotFound, "unknown_route", "no endpoint at this path")
}

// handleMethodNotAllowed answers a real endpoint addressed with a method it does not serve.
//
// A 405 is supposed to carry an Allow header listing the methods that would work, and this one does not.
// That is a known gap rather than an oversight: the router computes the allowed set while matching but
// keeps it private, and it also leaves the matched route pattern empty by the time this runs, so there is
// nothing here to look the endpoint up by. Producing the header would mean matching the path against the
// route table a second time, in code that can disagree with the router that just rejected it — and an Allow
// that names the wrong methods is worse than none. Revisit if the router starts exposing either.
func (a *API) handleMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "this endpoint does not accept that method")
}
