package httpapi

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// requireAuth enforces `Authorization: Bearer <key>` against the configured API
// key with a constant-time comparison. An unconfigured key (empty) rejects every
// request, so a misconfigured server fails closed rather than accepting an empty
// token.
func (a *API) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthorized", "missing or malformed bearer token")
			return
		}
		if a.apiKey == "" || subtle.ConstantTimeCompare([]byte(token), []byte(a.apiKey)) != 1 {
			writeError(w, r, http.StatusUnauthorized, "unauthorized", "invalid api key")
			return
		}
		next.ServeHTTP(w, r)
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
