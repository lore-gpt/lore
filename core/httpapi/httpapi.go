// Package httpapi is the Lore HTTP surface: a chi router with request id, panic
// recovery, structured access logs, and bearer auth, wired to the store and the
// job queue. It is the source-of-truth implementation of spec/openapi.yaml for
// Phase 0 (health, event ingest, and a recall placeholder).
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
	"github.com/jackc/pgx/v5/pgxpool"
)

// Enqueuer schedules the follow-up extraction for an accepted event. It runs on
// the caller's transaction so the enqueue is atomic with the event insert: if
// either fails, neither is durable. *queue.Queue satisfies it.
type Enqueuer interface {
	EnqueueExtract(ctx context.Context, tx pgx.Tx, eventID string) error
}

// Pinger reports a dependency's health for /healthz. *store.Store and
// *queue.Queue both satisfy it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Config carries the API's dependencies. Handlers touch only the fields their
// route needs, so narrower tests may leave the rest nil.
type Config struct {
	Pool     *pgxpool.Pool // begins the event write transaction
	Enqueuer Enqueuer      // enqueues extraction on that transaction
	DB       Pinger        // /healthz database probe
	Queue    Pinger        // /healthz queue probe
	APIKey   string        // bearer token required on /v1 routes
	Version  string        // reported by /healthz
}

// API holds the wired dependencies and builds the router.
type API struct {
	pool     *pgxpool.Pool
	enqueuer Enqueuer
	db       Pinger
	queue    Pinger
	apiKey   string
	version  string
}

// New returns an API bound to cfg.
func New(cfg Config) *API {
	return &API{
		pool:     cfg.Pool,
		enqueuer: cfg.Enqueuer,
		db:       cfg.DB,
		queue:    cfg.Queue,
		apiKey:   cfg.APIKey,
		version:  cfg.Version,
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
		r.Post("/v1/recall", a.handleRecall)
	})

	return r
}
