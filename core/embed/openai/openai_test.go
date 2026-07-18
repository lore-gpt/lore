package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeVector returns a length-dim vector whose first component encodes seed, so a
// test can prove a specific input's vector landed at a specific output position.
func fakeVector(dim int, seed float32) []float32 {
	v := make([]float32, dim)
	v[0] = seed
	return v
}

// embedHandler is a configurable OpenAI-compatible /v1/embeddings stub. It records
// each request body and returns one vector per input; order and vector length are
// controllable to exercise the join and the length assert.
type embedHandler struct {
	dim        int
	reversed   bool // return data in reverse index order
	badLenAt   int  // if >=0, the vector at this index gets a wrong length
	status     int  // if non-200, return this with an error body
	requests   []embedRequest
	authHeader string
}

func (h *embedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req embedRequest
	_ = json.Unmarshal(body, &req)
	h.requests = append(h.requests, req)
	h.authHeader = r.Header.Get("Authorization")

	if h.status != 0 && h.status != http.StatusOK {
		w.WriteHeader(h.status)
		_, _ = w.Write([]byte(`{"error":{"message":"boom from provider","type":"test"}}`))
		return
	}

	type item struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	}
	items := make([]item, len(req.Input))
	for i := range req.Input {
		dim := h.dim
		if h.badLenAt == i {
			dim = h.dim + 1
		}
		// Seed each vector with its input index so the client-side join is verifiable.
		items[i] = item{Index: i, Embedding: fakeVector(dim, float32(i))}
	}
	if h.reversed {
		for l, r := 0, len(items)-1; l < r; l, r = l+1, r-1 {
			items[l], items[r] = items[r], items[l]
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
}

func newTestEmbedder(t *testing.T, h *embedHandler, cfg Config) (*Embedder, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg.BaseURL = srv.URL
	if cfg.Model == "" {
		cfg.Model = "text-embedding-3-small"
	}
	if cfg.Dim == 0 {
		cfg.Dim = h.dim
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, srv
}

func TestEmbedHappyPathPreservesOrder(t *testing.T) {
	h := &embedHandler{dim: 8, badLenAt: -1}
	e, _ := newTestEmbedder(t, h, Config{Dim: 8})
	texts := []string{"a", "b", "c"}
	got, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d vectors, want 3", len(got))
	}
	for i, v := range got {
		if len(v) != 8 {
			t.Errorf("vector %d length %d, want 8", i, len(v))
		}
		if v[0] != float32(i) {
			t.Errorf("vector %d seed %v, want %d — order not preserved", i, v[0], i)
		}
	}
}

func TestEmbedJoinsByIndexNotResponseOrder(t *testing.T) {
	// The provider returns data in reverse order; the client must place each vector
	// by its `index`, not by arrival position.
	h := &embedHandler{dim: 4, badLenAt: -1, reversed: true}
	e, _ := newTestEmbedder(t, h, Config{Dim: 4})
	got, err := e.Embed(context.Background(), []string{"a", "b", "c", "d"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	for i, v := range got {
		if v[0] != float32(i) {
			t.Fatalf("vector %d seed %v, want %d — response reordered, join by index failed", i, v[0], i)
		}
	}
}

func TestEmbedRejectsWrongDimensionLoudly(t *testing.T) {
	h := &embedHandler{dim: 8, badLenAt: 1} // second vector comes back length 9
	e, _ := newTestEmbedder(t, h, Config{Dim: 8})
	_, err := e.Embed(context.Background(), []string{"a", "b", "c"})
	if err == nil {
		t.Fatal("expected a loud dimension error, got nil")
	}
	if !strings.Contains(err.Error(), "want 8") || !strings.Contains(err.Error(), "text-embedding-3-small@8") {
		t.Errorf("error should name got/want and the model@dim identity: %v", err)
	}
}

func TestEmbedChunksLargeBatchInOrder(t *testing.T) {
	h := &embedHandler{dim: 2, badLenAt: -1}
	e, _ := newTestEmbedder(t, h, Config{Dim: 2})
	n := maxInputsPerRequest*2 + 5 // forces 3 chunks
	texts := make([]string, n)
	for i := range texts {
		texts[i] = "t"
	}
	got, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != n {
		t.Fatalf("got %d vectors, want %d", len(got), n)
	}
	if len(h.requests) != 3 {
		t.Fatalf("made %d requests, want 3 chunks", len(h.requests))
	}
	// Global order: each vector's seed is its within-chunk index; check the chunk
	// boundaries reset as expected and every chunk stayed within the cap.
	for i, req := range h.requests {
		if len(req.Input) > maxInputsPerRequest {
			t.Errorf("chunk %d has %d inputs, over the cap %d", i, len(req.Input), maxInputsPerRequest)
		}
	}
	if got[0][0] != 0 || got[maxInputsPerRequest][0] != 0 {
		t.Errorf("chunk-relative seeding broke: got[0]=%v got[cap]=%v", got[0][0], got[maxInputsPerRequest][0])
	}
}

func TestEmbedEmptyInputMakesNoRequest(t *testing.T) {
	h := &embedHandler{dim: 8, badLenAt: -1}
	e, _ := newTestEmbedder(t, h, Config{Dim: 8})
	got, err := e.Embed(context.Background(), nil)
	if err != nil {
		t.Fatalf("Embed(nil): %v", err)
	}
	if got != nil {
		t.Errorf("Embed(nil) returned %d vectors, want nil", len(got))
	}
	if len(h.requests) != 0 {
		t.Errorf("made %d requests for an empty batch, want 0", len(h.requests))
	}
}

func TestSendDimensionsIsOptIn(t *testing.T) {
	// Off by default: no `dimensions` field on the wire.
	off := &embedHandler{dim: 8, badLenAt: -1}
	eOff, _ := newTestEmbedder(t, off, Config{Dim: 8, SendDimensions: false})
	if _, err := eOff.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if off.requests[0].Dimensions != 0 {
		t.Errorf("dimensions sent while opt-out: %d", off.requests[0].Dimensions)
	}

	// On: the field carries Dim.
	on := &embedHandler{dim: 8, badLenAt: -1}
	eOn, _ := newTestEmbedder(t, on, Config{Dim: 8, SendDimensions: true})
	if _, err := eOn.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if on.requests[0].Dimensions != 8 {
		t.Errorf("dimensions=%d on the wire, want 8", on.requests[0].Dimensions)
	}
}

func TestAuthHeaderSentOnlyWithKey(t *testing.T) {
	with := &embedHandler{dim: 4, badLenAt: -1}
	eWith, _ := newTestEmbedder(t, with, Config{Dim: 4, APIKey: "sk-secret-123"})
	if _, err := eWith.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if with.authHeader != "Bearer sk-secret-123" {
		t.Errorf("auth header = %q, want the bearer token", with.authHeader)
	}

	without := &embedHandler{dim: 4, badLenAt: -1}
	eWithout, _ := newTestEmbedder(t, without, Config{Dim: 4})
	if _, err := eWithout.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if without.authHeader != "" {
		t.Errorf("auth header = %q, want none when no key is set", without.authHeader)
	}
}

func TestStatusErrorsAreClassifiedAndKeyNeverLeaks(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, "401"},
		{http.StatusTooManyRequests, "429"},
		{http.StatusBadGateway, "502"},
		{http.StatusBadRequest, "400"},
	} {
		h := &embedHandler{dim: 4, badLenAt: -1, status: tc.status}
		e, _ := newTestEmbedder(t, h, Config{Dim: 4, APIKey: "sk-must-not-leak"})
		_, err := e.Embed(context.Background(), []string{"a"})
		if err == nil {
			t.Fatalf("status %d: expected an error", tc.status)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("status %d: error %q should name the status", tc.status, err)
		}
		if strings.Contains(err.Error(), "sk-must-not-leak") {
			t.Errorf("status %d: API key leaked into the error: %v", tc.status, err)
		}
	}
}

func TestNewValidatesModelAndDim(t *testing.T) {
	if _, err := New(Config{Model: "", Dim: 8}); err == nil {
		t.Error("empty model should error")
	}
	if _, err := New(Config{Model: "m", Dim: 0}); err == nil {
		t.Error("dim 0 should error")
	}
	if _, err := New(Config{Model: "m", Dim: maxVectorDim + 1}); err == nil {
		t.Error("dim over the ceiling should error")
	}
	e, err := New(Config{Model: "m", Dim: 1536})
	if err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if e.ModelID() != "m@1536" {
		t.Errorf("ModelID = %q, want m@1536", e.ModelID())
	}
	if e.Dim() != 1536 {
		t.Errorf("Dim = %d, want 1536", e.Dim())
	}
}
