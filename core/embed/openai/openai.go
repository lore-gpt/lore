// Package openai implements ext.Embedder against any OpenAI-compatible
// /v1/embeddings endpoint — OpenAI itself, or a self-hosted server that speaks
// the same wire format (Text Embeddings Inference, Ollama, vLLM, LM Studio, and
// the like). It is an OSS, bring-your-own-endpoint provider: the operator points
// it at a base URL and names a model and dimension. The offline
// ext.FixtureEmbedder stays the default; this adapter is composed in only when a
// provider is configured, so a stray key in the environment can't silently turn
// the worker into a paid-API caller.
//
// The read and write paths must embed in the same vector space, so the identity a
// project pins is model@dim (ModelID): changing either the model or the dimension
// is a new space. The model-mismatch guard surfaces that loudly rather than
// letting two incomparable spaces mix. The dimension is configured, not
// discovered, and every returned vector's length is asserted against it — a wrong
// provider/model/dimension combination fails loud at embed time and is never
// stored.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lore-gpt/lore/core/ext"
)

const (
	// defaultBaseURL is OpenAI's own endpoint; a self-hosted OpenAI-compatible
	// server overrides it via LORE_EMBEDDING_BASE_URL.
	defaultBaseURL = "https://api.openai.com/v1"
	// maxInputsPerRequest bounds one request's input array so a large pass is split
	// into ordered chunks rather than tripping a provider's per-request input cap. A
	// consolidation pass is small, so this rarely splits; it is a safety bound for a
	// pathological pass, not a throughput knob.
	maxInputsPerRequest = 96
	// maxVectorDim mirrors the retriever's ceiling (pgvector's own limit) so a
	// misconfigured dimension fails at construction, not later at index build.
	maxVectorDim = 16000
	// defaultTimeout bounds a single embeddings request. A transient failure returns
	// an error with no partial result; the job layer retries the whole pass, so the
	// adapter itself does not retry (no double-layer backoff).
	defaultTimeout = 30 * time.Second
	// maxResponseBytes caps the response we read from an untrusted endpoint. It is
	// generous enough for a full chunk at the largest dimension; a truncated body
	// surfaces as a loud decode error, never as a silently short result.
	maxResponseBytes = 128 << 20
)

// Config configures the OpenAI-compatible Embedder.
type Config struct {
	// BaseURL is the endpoint root (e.g. https://api.openai.com/v1). A trailing
	// slash is trimmed. Empty uses defaultBaseURL.
	BaseURL string
	// APIKey is sent as a Bearer token when non-empty. A self-hosted endpoint that
	// needs no auth leaves it empty; an endpoint that requires it answers 401, which
	// surfaces loud.
	APIKey string
	// Model is the embedding model name, passed to the provider verbatim. Required.
	Model string
	// Dim is the vector dimension. Required, in [1, maxVectorDim]. Every returned
	// vector is asserted against it.
	Dim int
	// SendDimensions includes the `dimensions` request field so an OpenAI-family
	// model truncates to Dim. Some OpenAI-compatible servers reject an unknown
	// field, so it is opt-in; the length assert is the contract regardless.
	SendDimensions bool
	// HTTPClient overrides the transport. Optional; injected by tests.
	HTTPClient *http.Client
	// Timeout bounds a single request when HTTPClient is nil. Zero uses
	// defaultTimeout.
	Timeout time.Duration
}

// Embedder is an ext.Embedder backed by an OpenAI-compatible /v1/embeddings
// endpoint.
type Embedder struct {
	client         *http.Client
	baseURL        string
	apiKey         string
	model          string
	dim            int
	sendDimensions bool
}

var _ ext.Embedder = (*Embedder)(nil)

// New builds an Embedder from cfg. It returns an error when the model is empty or
// the dimension is out of range, so a misconfigured worker fails at construction
// rather than at first use; the caller keeps the offline fixture in that case.
func New(cfg Config) (*Embedder, error) {
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		return nil, errors.New("embed/openai: model is required (set LORE_EMBEDDING_MODEL)")
	}
	if cfg.Dim < 1 || cfg.Dim > maxVectorDim {
		return nil, fmt.Errorf("embed/openai: dimension %d out of range [1,%d] (set LORE_EMBEDDING_DIM)", cfg.Dim, maxVectorDim)
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = defaultBaseURL
	}
	client := cfg.HTTPClient
	if client == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = defaultTimeout
		}
		client = &http.Client{Timeout: timeout}
	}
	return &Embedder{
		client:         client,
		baseURL:        base,
		apiKey:         strings.TrimSpace(cfg.APIKey),
		model:          model,
		dim:            cfg.Dim,
		sendDimensions: cfg.SendDimensions,
	}, nil
}

// Dim reports the configured vector dimension.
func (e *Embedder) Dim() int { return e.dim }

// ModelID is the vector-space identity embeddings are stored under: model@dim.
// The dimension is part of the identity because the same model at two dimensions
// produces incomparable vectors, and reads query a single model space.
func (e *Embedder) ModelID() string { return fmt.Sprintf("%s@%d", e.model, e.dim) }

// Embed returns one vector per input text, in the same order, each of length
// Dim(). It splits a large batch into ordered chunks and preserves global order.
// A transport or provider failure returns an error and no partial result.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += maxInputsPerRequest {
		end := start + maxInputsPerRequest
		if end > len(texts) {
			end = len(texts)
		}
		vecs, err := e.embedChunk(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

type embedRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions,omitempty"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *providerError `json:"error"`
}

type providerError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// embedChunk embeds one input chunk in a single request and joins the response by
// index. Neither the request order nor the response order is guaranteed to match,
// so vectors are placed by their returned index; each is length-checked against
// Dim before it can be stored.
func (e *Embedder) embedChunk(ctx context.Context, chunk []string) ([][]float32, error) {
	reqBody := embedRequest{Model: e.model, Input: chunk}
	if e.sendDimensions {
		reqBody.Dimensions = e.dim
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("embed/openai: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/embeddings", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("embed/openai: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		// The key is never part of a transport error, and it is not echoed anywhere
		// below either: only the server's own status and body are surfaced.
		return nil, fmt.Errorf("embed/openai: request to %s/embeddings failed: %w", e.baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("embed/openai: read response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp.StatusCode, body)
	}

	var parsed embedResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("embed/openai: decode response (status %d): %w", resp.StatusCode, err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, fmt.Errorf("embed/openai: provider error: %s", parsed.Error.Message)
	}
	if len(parsed.Data) != len(chunk) {
		return nil, fmt.Errorf("embed/openai: got %d embeddings for %d inputs (model %q)", len(parsed.Data), len(chunk), e.ModelID())
	}

	vecs := make([][]float32, len(chunk))
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= len(chunk) {
			return nil, fmt.Errorf("embed/openai: response index %d out of range [0,%d)", d.Index, len(chunk))
		}
		if vecs[d.Index] != nil {
			return nil, fmt.Errorf("embed/openai: duplicate response index %d", d.Index)
		}
		if len(d.Embedding) != e.dim {
			return nil, fmt.Errorf("embed/openai: embedding at index %d has length %d, want %d (model %q) — check that LORE_EMBEDDING_DIM matches the model",
				d.Index, len(d.Embedding), e.dim, e.ModelID())
		}
		vecs[d.Index] = d.Embedding
	}
	return vecs, nil
}

// statusError classifies a non-200 response into a clear, actionable error. It
// echoes only the server's status and message — never the request — so the API
// key can't leak into a log or error string.
func statusError(status int, body []byte) error {
	msg := providerMessage(body)
	switch {
	case status == http.StatusUnauthorized:
		return fmt.Errorf("embed/openai: unauthorized (401) — check the embedding API key: %s", msg)
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("embed/openai: rate limited (429): %s", msg)
	case status >= 500:
		return fmt.Errorf("embed/openai: provider error (%d): %s", status, msg)
	default:
		return fmt.Errorf("embed/openai: request failed (%d): %s", status, msg)
	}
}

// providerMessage pulls a human-readable message out of an error body, falling
// back to a bounded raw snippet when the body isn't the expected shape.
func providerMessage(body []byte) string {
	var parsed embedResponse
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error != nil && parsed.Error.Message != "" {
		return parsed.Error.Message
	}
	snippet := strings.TrimSpace(string(body))
	const max = 200
	if len(snippet) > max {
		snippet = snippet[:max] + "…"
	}
	if snippet == "" {
		return "(no response body)"
	}
	return snippet
}
