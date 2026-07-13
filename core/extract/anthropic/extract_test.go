package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lore-gpt/lore/core/ext"
)

// fakeMessages returns an httptest server that replies to every request with the
// given status and body, and counts the requests it received.
func fakeMessages(t *testing.T, status int, body string) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// testExtractor builds an Extractor pointed at srv with retries disabled so error
// paths fail fast.
func testExtractor(t *testing.T, srv *httptest.Server) *Extractor {
	t.Helper()
	zero := 0
	e, err := New(Config{
		APIKey:     "test-key",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		MaxRetries: &zero,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// toolUseBody builds a Messages API response whose single content block is a
// tool_use call to record_extraction carrying input.
func toolUseBody(t *testing.T, input map[string]any) string {
	t.Helper()
	resp := map[string]any{
		"id":          "msg_test",
		"type":        "message",
		"role":        "assistant",
		"model":       "claude-haiku-4-5",
		"stop_reason": "tool_use",
		"content": []map[string]any{
			{"type": "tool_use", "id": "toolu_1", "name": toolName, "input": input},
		},
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 20},
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	return string(b)
}

func twoEvents() []ext.InputEvent {
	return []ext.InputEvent{
		{Seq: 1, AgentID: "planner", Payload: json.RawMessage(`{"note":"a"}`)},
		{Seq: 2, AgentID: "coder", Payload: json.RawMessage(`{"note":"b"}`)},
	}
}

func TestExtractStructuredResult(t *testing.T) {
	body := toolUseBody(t, map[string]any{
		"memories": []map[string]any{
			{"kind": "semantic", "content": "Auth uses OAuth2", "source_seq": 1},
			{"kind": "episodic", "content": "PR #42 merged", "source_seq": 2},
		},
		"claims": []map[string]any{
			{"entity": "auth-svc", "predicate": "status", "value": "done", "source_seq": 2},
		},
		"entities": []map[string]any{
			{"name": "auth-svc", "type": "service", "aliases": []string{"auth"}},
		},
	})
	srv, _ := fakeMessages(t, http.StatusOK, body)

	res, err := testExtractor(t, srv).Extract(context.Background(), ext.ExtractInput{
		ProjectID: "p", RunID: "r", Events: twoEvents(),
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if len(res.Memories) != 2 {
		t.Fatalf("memories = %d, want 2", len(res.Memories))
	}
	if res.Memories[0] != (ext.CandidateMemory{Kind: "semantic", Content: "Auth uses OAuth2", SourceSeq: 1}) {
		t.Errorf("memory[0] = %+v", res.Memories[0])
	}
	if res.Memories[1] != (ext.CandidateMemory{Kind: "episodic", Content: "PR #42 merged", SourceSeq: 2}) {
		t.Errorf("memory[1] = %+v", res.Memories[1])
	}

	if len(res.Claims) != 1 {
		t.Fatalf("claims = %d, want 1", len(res.Claims))
	}
	c := res.Claims[0]
	if c.Entity != "auth-svc" || c.Predicate != "status" || string(c.Value) != `"done"` || c.SourceSeq != 2 || c.EventTime != nil {
		t.Errorf("claim = %+v (value=%s)", c, c.Value)
	}

	if len(res.Entities) != 1 {
		t.Fatalf("entities = %d, want 1", len(res.Entities))
	}
	e := res.Entities[0]
	if e.Name != "auth-svc" || e.Type != "service" || len(e.Aliases) != 1 || e.Aliases[0] != "auth" {
		t.Errorf("entity = %+v", e)
	}
}

func TestExtractNormalizesUnknownKind(t *testing.T) {
	body := toolUseBody(t, map[string]any{
		"memories": []map[string]any{
			{"kind": "note", "content": "unknown kind", "source_seq": 5},
		},
		"claims":   []map[string]any{},
		"entities": []map[string]any{},
	})
	srv, _ := fakeMessages(t, http.StatusOK, body)

	res, err := testExtractor(t, srv).Extract(context.Background(), ext.ExtractInput{
		Events: []ext.InputEvent{{Seq: 5, AgentID: "a", Payload: json.RawMessage(`{}`)}},
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Memories) != 1 || res.Memories[0].Kind != "semantic" {
		t.Fatalf("kind = %q, want semantic (coerced)", res.Memories[0].Kind)
	}
}

func TestExtractRequestShape(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(toolUseBody(t, map[string]any{
			"memories": []map[string]any{}, "claims": []map[string]any{}, "entities": []map[string]any{},
		})))
	}))
	t.Cleanup(srv.Close)

	zero := 0
	e, err := New(Config{APIKey: "test-key", BaseURL: srv.URL, HTTPClient: srv.Client(), MaxRetries: &zero})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := e.Extract(context.Background(), ext.ExtractInput{Events: twoEvents()}); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	var req struct {
		Model       string   `json:"model"`
		Temperature *float64 `json:"temperature"`
		MaxTokens   int64    `json:"max_tokens"`
		ToolChoice  struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"tool_choice"`
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		System []struct {
			Text         string          `json:"text"`
			CacheControl json.RawMessage `json:"cache_control"`
		} `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatalf("decode request: %v (body=%s)", err, gotBody)
	}

	if req.Model != DefaultModel {
		t.Errorf("model = %q, want %q", req.Model, DefaultModel)
	}
	// Deterministic extraction: temperature must be pinned to 0, not merely defaulted
	// away. A regression that drops the line would otherwise pass unnoticed.
	if req.Temperature == nil || *req.Temperature != 0.0 {
		t.Errorf("temperature = %v, want 0.0", req.Temperature)
	}
	if req.MaxTokens != defaultMaxTokens {
		t.Errorf("max_tokens = %d, want %d", req.MaxTokens, defaultMaxTokens)
	}
	if req.ToolChoice.Type != "tool" || req.ToolChoice.Name != toolName {
		t.Errorf("tool_choice = %+v, want forced %s", req.ToolChoice, toolName)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != toolName {
		t.Errorf("tools = %+v, want [%s]", req.Tools, toolName)
	}
	// Prompt-cache discipline: the fixed system prefix carries a cache breakpoint.
	if len(req.System) == 0 || len(req.System[0].CacheControl) == 0 || string(req.System[0].CacheControl) == "null" {
		t.Errorf("system block missing cache_control breakpoint: %+v", req.System)
	}
	// Variable events go last, in the user message, carrying their seq for provenance.
	if len(req.Messages) == 0 || req.Messages[0].Role != "user" || len(req.Messages[0].Content) == 0 {
		t.Fatalf("messages = %+v, want a user message", req.Messages)
	}
	text := req.Messages[0].Content[0].Text
	if !strings.Contains(text, `"seq":1`) || !strings.Contains(text, `"seq":2`) {
		t.Errorf("user message missing event seqs: %q", text)
	}
}

func TestExtractTransientErrorUnavailable(t *testing.T) {
	srv, _ := fakeMessages(t, http.StatusServiceUnavailable,
		`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`)

	_, err := testExtractor(t, srv).Extract(context.Background(), ext.ExtractInput{Events: twoEvents()})
	if !errors.Is(err, ext.ErrExtractorUnavailable) {
		t.Fatalf("err = %v, want ErrExtractorUnavailable", err)
	}
}

func TestExtractClientErrorPermanent(t *testing.T) {
	srv, _ := fakeMessages(t, http.StatusBadRequest,
		`{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`)

	_, err := testExtractor(t, srv).Extract(context.Background(), ext.ExtractInput{Events: twoEvents()})
	if err == nil {
		t.Fatal("Extract with 400 = nil error, want error")
	}
	if errors.Is(err, ext.ErrExtractorUnavailable) {
		t.Fatalf("400 mapped to retryable ErrExtractorUnavailable: %v", err)
	}
}

func TestExtractRateLimitRetryable(t *testing.T) {
	// 429 is a 4xx but must stay retryable — it is the load-bearing carve-out in
	// mapError, and the most likely transient status under provider load.
	srv, _ := fakeMessages(t, http.StatusTooManyRequests,
		`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)

	_, err := testExtractor(t, srv).Extract(context.Background(), ext.ExtractInput{Events: twoEvents()})
	if !errors.Is(err, ext.ErrExtractorUnavailable) {
		t.Fatalf("429 err = %v, want ErrExtractorUnavailable", err)
	}
}

func TestExtractTruncatedResultUnavailable(t *testing.T) {
	// stop_reason "max_tokens": the tool input decodes fine but is only partial.
	// The adapter must not present it as a complete extraction (which would let the
	// worker advance the checkpoint past silently-dropped candidates); it retries.
	resp := map[string]any{
		"id": "msg_t", "type": "message", "role": "assistant", "model": "claude-haiku-4-5",
		"stop_reason": "max_tokens",
		"content": []map[string]any{
			{"type": "tool_use", "id": "toolu_1", "name": toolName, "input": map[string]any{
				"memories": []map[string]any{{"kind": "semantic", "content": "partial", "source_seq": 1}},
				"claims":   []map[string]any{},
				"entities": []map[string]any{},
			}},
		},
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 4096},
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	srv, _ := fakeMessages(t, http.StatusOK, string(b))

	_, err = testExtractor(t, srv).Extract(context.Background(), ext.ExtractInput{Events: twoEvents()})
	if !errors.Is(err, ext.ErrExtractorUnavailable) {
		t.Fatalf("truncated err = %v, want ErrExtractorUnavailable", err)
	}
}

func TestExtractDropsEmptyMemoryContent(t *testing.T) {
	body := toolUseBody(t, map[string]any{
		"memories": []map[string]any{
			{"kind": "semantic", "content": "", "source_seq": 1},
			{"kind": "semantic", "content": "kept", "source_seq": 2},
		},
		"claims":   []map[string]any{},
		"entities": []map[string]any{},
	})
	srv, _ := fakeMessages(t, http.StatusOK, body)

	res, err := testExtractor(t, srv).Extract(context.Background(), ext.ExtractInput{Events: twoEvents()})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Memories) != 1 || res.Memories[0].Content != "kept" {
		t.Fatalf("memories = %+v, want only the non-empty one", res.Memories)
	}
}

func TestExtractMissingToolUse(t *testing.T) {
	body := `{"id":"msg_x","type":"message","role":"assistant","model":"claude-haiku-4-5",` +
		`"stop_reason":"end_turn","content":[{"type":"text","text":"hello"}],` +
		`"usage":{"input_tokens":1,"output_tokens":1}}`
	srv, _ := fakeMessages(t, http.StatusOK, body)

	_, err := testExtractor(t, srv).Extract(context.Background(), ext.ExtractInput{Events: twoEvents()})
	if !errors.Is(err, ext.ErrExtractorUnavailable) {
		t.Fatalf("err = %v, want ErrExtractorUnavailable for missing tool_use", err)
	}
}

func TestExtractEmptyEventsSkipsCall(t *testing.T) {
	srv, calls := fakeMessages(t, http.StatusOK, toolUseBody(t, map[string]any{
		"memories": []map[string]any{}, "claims": []map[string]any{}, "entities": []map[string]any{},
	}))

	res, err := testExtractor(t, srv).Extract(context.Background(), ext.ExtractInput{Events: nil})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Memories) != 0 || len(res.Claims) != 0 || len(res.Entities) != 0 {
		t.Errorf("empty window produced %+v", res)
	}
	if *calls != 0 {
		t.Errorf("server called %d times for an empty window, want 0", *calls)
	}
}

func TestExtractSingleEventSeqFallback(t *testing.T) {
	// The model omits source_seq (decodes to 0). With a single-event window the
	// provenance is unambiguous, so it is repaired to that event's seq.
	body := toolUseBody(t, map[string]any{
		"memories": []map[string]any{
			{"kind": "semantic", "content": "sole event fact"},
		},
		"claims":   []map[string]any{},
		"entities": []map[string]any{},
	})
	srv, _ := fakeMessages(t, http.StatusOK, body)

	res, err := testExtractor(t, srv).Extract(context.Background(), ext.ExtractInput{
		Events: []ext.InputEvent{{Seq: 7, AgentID: "a", Payload: json.RawMessage(`{}`)}},
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Memories) != 1 || res.Memories[0].SourceSeq != 7 {
		t.Fatalf("source_seq = %d, want 7 (single-event fallback)", res.Memories[0].SourceSeq)
	}
}

func TestNewRequiresAPIKey(t *testing.T) {
	if _, err := New(Config{APIKey: ""}); err == nil {
		t.Fatal("New with empty API key = nil error, want error")
	}
}
