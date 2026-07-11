package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Error is the API's error body. It mirrors the Error schema in
// spec/openapi.yaml: message is required, code is a stable machine-readable tag.
type Error struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

// writeJSON encodes body as the response with the given status. A failed encode
// is logged rather than surfaced, since the status line is already committed.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.ErrorContext(r.Context(), "write json response", slog.String("error", err.Error()))
	}
}

// writeError writes an Error body with the given status, code, and message.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeJSON(w, r, status, Error{Message: message, Code: code})
}
