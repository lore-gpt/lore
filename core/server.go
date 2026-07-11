package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/httpapi"
	"github.com/lore-gpt/lore/core/queue"
	"github.com/lore-gpt/lore/core/store"
)

// Config is the composition input shared by the server and worker. The OSS
// binary fills it from the environment (server/internal/config); a downstream
// build fills it however it likes.
type Config struct {
	Addr        string // HTTP listen address for the server, e.g. ":8080"
	DatabaseURL string // Postgres DSN
	APIKey      string // bearer token required on /v1 routes
}

// extensions holds the swappable extension-point implementations. Phase 0 wires
// the OSS defaults and records which are active; it does not yet invoke them on
// the request path.
type extensions struct {
	policy      ext.PolicyEngine
	adjudicator ext.Adjudicator
	metering    ext.MeteringSink
}

func defaultExtensions() extensions {
	return extensions{
		policy:      ext.BasicScopePolicy{},
		adjudicator: ext.LWW{},
		metering:    ext.NoopMetering{},
	}
}

// resolveExtensions applies opts over the OSS defaults and rejects a nil
// override so a misconfigured composition fails at construction, not at first
// use in Phase 1.
func resolveExtensions(opts []Option) (extensions, error) {
	e := defaultExtensions()
	for _, o := range opts {
		o(&e)
	}
	if e.policy == nil || e.adjudicator == nil || e.metering == nil {
		return extensions{}, errors.New("core: extension point set to nil")
	}
	return e, nil
}

func (e extensions) logComposed(ctx context.Context, role string) {
	slog.InfoContext(ctx, "composed "+role,
		slog.String("policy", fmt.Sprintf("%T", e.policy)),
		slog.String("adjudicator", fmt.Sprintf("%T", e.adjudicator)),
		slog.String("metering", fmt.Sprintf("%T", e.metering)),
	)
}

// Option overrides a default extension point. A closed-source build injects its
// own implementations here without forking the core (ADR-014).
type Option func(*extensions)

// WithPolicyEngine overrides the default PolicyEngine.
func WithPolicyEngine(p ext.PolicyEngine) Option {
	return func(e *extensions) { e.policy = p }
}

// WithAdjudicator overrides the default Adjudicator.
func WithAdjudicator(a ext.Adjudicator) Option {
	return func(e *extensions) { e.adjudicator = a }
}

// WithMetering overrides the default MeteringSink.
func WithMetering(m ext.MeteringSink) Option {
	return func(e *extensions) { e.metering = m }
}

// Server is the HTTP-serving composition: the store, an insert-only queue
// client, and the HTTP API. It enqueues extraction jobs but structurally cannot
// work them — that is the Worker's role.
type Server struct {
	store *store.Store
	queue *queue.Queue
	http  *http.Server
	ext   extensions
}

// NewServer composes the HTTP server from cfg, defaulting the extension points
// to their OSS implementations. It opens the database pool and an insert-only
// queue client. The caller runs Start and must Close the Server when done.
func NewServer(ctx context.Context, cfg Config, opts ...Option) (*Server, error) {
	e, err := resolveExtensions(opts)
	if err != nil {
		return nil, err
	}

	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	q, err := queue.New(st.Pool)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("build queue: %w", err)
	}

	api := httpapi.New(httpapi.Config{
		Pool:     st.Pool,
		Enqueuer: q,
		DB:       st,
		Queue:    q,
		APIKey:   cfg.APIKey,
		Version:  Version,
	})

	return &Server{
		store: st,
		queue: q,
		http: &http.Server{
			Addr:              cfg.Addr,
			Handler:           api.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		},
		ext: e,
	}, nil
}

// Start serves HTTP until ctx is canceled, then drains in-flight requests
// within a bounded grace period.
func (s *Server) Start(ctx context.Context) error {
	s.ext.logComposed(ctx, "server")

	errc := make(chan error, 1)
	go func() {
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return s.http.Shutdown(shutdownCtx)
	}
}

// Close releases the Server's resources (the database pool).
func (s *Server) Close() {
	s.store.Close()
}
