package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lore-gpt/lore/core/apikey"
	"github.com/lore-gpt/lore/core/store/db"
)

// projectContextKey is the private key under which requireAuth stashes the authenticated project id.
type projectContextKey struct{}

// withProjectID returns ctx carrying the authenticated project id.
func withProjectID(ctx context.Context, id pgtype.UUID) context.Context {
	return context.WithValue(ctx, projectContextKey{}, id)
}

// projectIDFromContext returns the project id requireAuth resolved from the API key, and whether one is
// present. A handler in the authenticated group always has it; the ok guard is defence against a route wired
// outside requireAuth.
func projectIDFromContext(ctx context.Context) (pgtype.UUID, bool) {
	id, ok := ctx.Value(projectContextKey{}).(pgtype.UUID)
	return id, ok
}

// requireAuth authenticates the bearer token against the api_keys table and scopes the request to the key's
// project. The presented token is hashed and the hash is looked up (the raw token never touches the database);
// an unknown key and a revoked key are the SAME 401, so a caller cannot tell one from the other. On success the
// key's project id is stashed in the request context for handlers to open their tenant transaction with. The
// lookup runs unscoped by design — the project is not known until the key resolves — and RLS is the second belt
// once the application runs as a subject role (see the query's cutover note).
func (a *API) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthorized", "missing or malformed bearer token")
			return
		}
		projectID, err := db.New(a.pool).LookupAPIKeyProject(r.Context(), apikey.Hash(token))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, r, http.StatusUnauthorized, "unauthorized", "invalid api key")
				return
			}
			writeError(w, r, http.StatusInternalServerError, "internal", "could not verify api key")
			return
		}
		next.ServeHTTP(w, r.WithContext(withProjectID(r.Context(), projectID)))
	})
}

// bearerToken extracts the token from an Authorization header. The scheme is
// matched case-insensitively per RFC 7235; the token must be non-empty.
func bearerToken(header string) (string, bool) {
	const scheme = "bearer "
	if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}
	return header[len(scheme):], true
}

// logRequests emits one structured access log per request after it completes,
// carrying the chi request id so a slow or failed call can be traced.
//
// The logged fields are a fixed allowlist — method, path, status, response size,
// duration, request id. It deliberately never logs the request/response body or
// any header, so secrets like the Authorization bearer token cannot leak into
// logs. Keep new fields on this allowlist; do not add bodies or headers.
func (a *API) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()
		next.ServeHTTP(ww, r)
		slog.InfoContext(r.Context(), "http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", ww.Status()),
			slog.Int("bytes", ww.BytesWritten()),
			slog.Duration("duration", time.Since(start)),
			slog.String("request_id", middleware.GetReqID(r.Context())),
		)
	})
}

// recordMetrics observes HTTP request count, duration, and in-flight into the
// Prometheus registry. The route label is chi's matched route TEMPLATE (e.g.
// /v1/memories/{id}), read after ServeHTTP, never the raw path — a raw path would
// make the {id} segment an unbounded label and explode the series count. The
// scrape endpoint itself is excluded so a Prometheus poll doesn't inflate the
// counters.
func (a *API) recordMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		a.metrics.HTTPInFlight.Inc()
		defer a.metrics.HTTPInFlight.Dec()

		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()
		next.ServeHTTP(ww, r)

		route := chi.RouteContext(r.Context()).RoutePattern()
		if route == "" {
			route = "unmatched" // a 404: bucket together so arbitrary client paths never become labels
		}
		status := strconv.Itoa(ww.Status())
		a.metrics.HTTPRequests.WithLabelValues(route, r.Method, status).Inc()
		a.metrics.HTTPDuration.WithLabelValues(route, r.Method, status).Observe(time.Since(start).Seconds())
	})
}
