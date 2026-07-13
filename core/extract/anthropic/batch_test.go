package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lore-gpt/lore/core/ext"
)

// batchRoutes canned responses for the three batch endpoints the SDK calls.
type batchRoutes struct {
	submitStatus int
	submitBody   string
	getStatus    int
	getBody      string
	resultsBody  string
	gotSubmit    *[]byte // captures the POST /batches request body, if non-nil
}

// fakeBatchServer serves the Message Batch endpoints (create, get, results) from canned responses, so
// the adapter's batch path is exercised offline.
func fakeBatchServer(t *testing.T, r batchRoutes) *httptest.Server {
	t.Helper()
	write := func(w http.ResponseWriter, status int, contentType, body string) {
		w.Header().Set("Content-Type", contentType)
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages/batches", func(w http.ResponseWriter, req *http.Request) {
		if r.gotSubmit != nil {
			*r.gotSubmit, _ = io.ReadAll(req.Body)
		}
		write(w, r.submitStatus, "application/json", r.submitBody)
	})
	mux.HandleFunc("GET /v1/messages/batches/{id}/results", func(w http.ResponseWriter, _ *http.Request) {
		write(w, http.StatusOK, "application/x-jsonl", r.resultsBody)
	})
	mux.HandleFunc("GET /v1/messages/batches/{id}", func(w http.ResponseWriter, _ *http.Request) {
		write(w, r.getStatus, "application/json", r.getBody)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// batchJSON is a MessageBatch envelope with the given id and processing status.
func batchJSON(id, status string) string {
	return fmt.Sprintf(`{"id":%q,"type":"message_batch","processing_status":%q,`+
		`"request_counts":{"processing":0,"succeeded":1,"errored":0,"canceled":0,"expired":0},`+
		`"results_url":"https://example.test/results","created_at":"2026-01-02T03:04:05Z","expires_at":"2026-01-03T03:04:05Z"}`, id, status)
}

// succeededResultLine is one JSONL result line: our request succeeded, carrying the tool_use message.
func succeededResultLine(t *testing.T, input map[string]any) string {
	return fmt.Sprintf(`{"custom_id":%q,"result":{"type":"succeeded","message":%s}}`, batchCustomID, toolUseBody(t, input))
}

func TestSubmitBatch(t *testing.T) {
	var gotSubmit []byte
	srv := fakeBatchServer(t, batchRoutes{submitBody: batchJSON("batch_xyz", "in_progress"), gotSubmit: &gotSubmit})

	handle, err := testExtractor(t, srv).SubmitBatch(context.Background(), ext.ExtractInput{Events: twoEvents()})
	if err != nil {
		t.Fatalf("SubmitBatch: %v", err)
	}
	if handle != "batch_xyz" {
		t.Errorf("handle = %q, want the batch id batch_xyz", handle)
	}

	// The submission carries exactly one request, tagged with our custom_id and forcing the tool.
	var req struct {
		Requests []struct {
			CustomID string `json:"custom_id"`
			Params   struct {
				ToolChoice struct {
					Name string `json:"name"`
				} `json:"tool_choice"`
				Messages []json.RawMessage `json:"messages"`
			} `json:"params"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(gotSubmit, &req); err != nil {
		t.Fatalf("decode submit body: %v (body=%s)", err, gotSubmit)
	}
	if len(req.Requests) != 1 {
		t.Fatalf("submitted %d requests, want 1", len(req.Requests))
	}
	if req.Requests[0].CustomID != batchCustomID {
		t.Errorf("custom_id = %q, want %q", req.Requests[0].CustomID, batchCustomID)
	}
	if req.Requests[0].Params.ToolChoice.Name != toolName {
		t.Errorf("tool_choice = %q, want the forced %s", req.Requests[0].Params.ToolChoice.Name, toolName)
	}
	if len(req.Requests[0].Params.Messages) == 0 {
		t.Error("batch request carried no messages (the events window)")
	}
}

func TestSubmitBatchEmptyWindowErrors(t *testing.T) {
	srv := fakeBatchServer(t, batchRoutes{submitBody: batchJSON("x", "in_progress")})
	if _, err := testExtractor(t, srv).SubmitBatch(context.Background(), ext.ExtractInput{Events: nil}); err == nil {
		t.Error("SubmitBatch on an empty window = nil error, want an error")
	}
}

func TestCollectBatchNotReady(t *testing.T) {
	srv := fakeBatchServer(t, batchRoutes{getBody: batchJSON("batch_xyz", "in_progress")})

	res, done, err := testExtractor(t, srv).CollectBatch(context.Background(), "batch_xyz")
	if err != nil {
		t.Fatalf("CollectBatch: %v", err)
	}
	if done {
		t.Error("a still-processing batch should report done=false")
	}
	if len(res.Memories) != 0 {
		t.Errorf("a not-ready batch must return no result, got %+v", res)
	}
}

func TestCollectBatchSucceeded(t *testing.T) {
	input := map[string]any{
		"memories": []map[string]any{{"kind": "semantic", "content": "batched fact", "source_seq": 1}},
		"claims":   []map[string]any{},
		"entities": []map[string]any{},
	}
	srv := fakeBatchServer(t, batchRoutes{
		getBody:     batchJSON("batch_xyz", "ended"),
		resultsBody: succeededResultLine(t, input),
	})

	res, done, err := testExtractor(t, srv).CollectBatch(context.Background(), "batch_xyz")
	if err != nil {
		t.Fatalf("CollectBatch: %v", err)
	}
	if !done {
		t.Fatal("an ended batch should report done=true")
	}
	if len(res.Memories) != 1 || res.Memories[0].Content != "batched fact" || res.Memories[0].SourceSeq != 1 {
		t.Fatalf("collected memories = %+v, want one {batched fact, 1}", res.Memories)
	}
}

func TestCollectBatchErroredItem(t *testing.T) {
	srv := fakeBatchServer(t, batchRoutes{
		getBody:     batchJSON("batch_xyz", "ended"),
		resultsBody: fmt.Sprintf(`{"custom_id":%q,"result":{"type":"errored"}}`, batchCustomID),
	})

	_, done, err := testExtractor(t, srv).CollectBatch(context.Background(), "batch_xyz")
	if err == nil {
		t.Fatal("an errored batch request should surface an error")
	}
	if done {
		t.Error("an errored batch must not report done=true")
	}
}

func TestCollectBatchTransientGetUnavailable(t *testing.T) {
	srv := fakeBatchServer(t, batchRoutes{
		getStatus: http.StatusServiceUnavailable,
		getBody:   `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`,
	})

	_, _, err := testExtractor(t, srv).CollectBatch(context.Background(), "batch_xyz")
	if !errors.Is(err, ext.ErrExtractorUnavailable) {
		t.Fatalf("err = %v, want ErrExtractorUnavailable for a transient Get", err)
	}
}

func TestCollectBatchNoMatchingResult(t *testing.T) {
	// The batch ended but streamed a result for a different custom_id: a provider inconsistency, retryable.
	srv := fakeBatchServer(t, batchRoutes{
		getBody:     batchJSON("batch_xyz", "ended"),
		resultsBody: `{"custom_id":"someone-else","result":{"type":"succeeded","message":{"id":"m","type":"message","role":"assistant","model":"claude-haiku-4-5","stop_reason":"tool_use","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}}`,
	})

	_, _, err := testExtractor(t, srv).CollectBatch(context.Background(), "batch_xyz")
	if !errors.Is(err, ext.ErrExtractorUnavailable) {
		t.Fatalf("err = %v, want ErrExtractorUnavailable when no result matches our request", err)
	}
}
