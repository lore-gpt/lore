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
}

// New returns an API bound to cfg. A nil Workmem coerces to the disabled no-op so
// handlers never hold a nil store.
func New(cfg Config) *API {
	wm := cfg.Workmem
	if wm == nil {
		wm = workmem.NewDisabled()
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

	r.Get("/healthz", a.handleHealthz)

	r.Group(func(r chi.Router) {
		r.Use(a.requireAuth)
		r.Post("/v1/events", a.handleCreateEvent)
		r.Post("/v1/runs", a.handleCreateRun)
		r.Post("/v1/pack", a.handlePack)

		// Contracts that exist but land in a later increment answer 501 (not the router's 404), so a client
		// sees the endpoint is real but unfinished. Registering them keeps the surface honest.
		r.Get("/v1/memories", a.notImplemented)
		r.Post("/v1/memories", a.notImplemented)
		r.Get("/v1/memories/{id}", a.notImplemented)
		r.Patch("/v1/memories/{id}", a.notImplemented)
		r.Delete("/v1/memories/{id}", a.notImplemented)
		r.Get("/v1/memories/{id}/versions", a.notImplemented)
		r.Get("/v1/runs/{id}/trace", a.notImplemented)
		r.Get("/v1/policies", a.notImplemented)
	})

	return r
}
