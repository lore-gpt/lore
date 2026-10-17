package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lore-gpt/lore/core/ext"
)

func TestTakesTemperatureReadsTheModelGeneration(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  bool
	}{
		{"claude-haiku-4-5", true},
		{"claude-haiku-4-5-20251001", true},
		{"claude-sonnet-4-6", true},
		{"claude-3-5-haiku-latest", true},
		{"claude-haiku-5-5", false},
		{"claude-opus-5-5", false},
		{"claude-fable-5-1", false},
		{"claude-sonnet-6", false},
		// An id outside the pattern keeps the behaviour every model had before the check existed.
		{"my-gateway-alias", true},
	} {
		if got := takesTemperature(tc.model); got != tc.want {
			t.Errorf("takesTemperature(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

// modelExtractor is testExtractor with a model override.
func modelExtractor(t *testing.T, srv *httptest.Server, model string) *Extractor {
	t.Helper()
	zero := 0
	e, err := New(Config{APIKey: "test-key", Model: model, BaseURL: srv.URL, HTTPClient: srv.Client(), MaxRetries: &zero})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// TestRequestsCarryTemperatureOnlyWhereTheModelTakesIt checks the wire, on both paths, for a model on each
// side of the line: 0 for one that takes the parameter, the field absent for one that refuses any value with
// a 400 — where sending 0 would fail every pass, realtime or batched.
func TestRequestsCarryTemperatureOnlyWhereTheModelTakesIt(t *testing.T) {
	for _, tc := range []struct {
		model   string
		present bool
	}{
		{"claude-haiku-4-5", true},
		{"claude-haiku-5-5", false},
	} {
		t.Run(tc.model, func(t *testing.T) {
			var realtime []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				realtime, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(toolUseBody(t, map[string]any{
					"memories": []map[string]any{}, "claims": []map[string]any{}, "entities": []map[string]any{},
				})))
			}))
			t.Cleanup(srv.Close)
			if _, err := modelExtractor(t, srv, tc.model).Extract(context.Background(), ext.ExtractInput{Events: twoEvents()}); err != nil {
				t.Fatalf("Extract: %v", err)
			}
			var req map[string]json.RawMessage
			if err := json.Unmarshal(realtime, &req); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			checkTemperature(t, "realtime", req, tc.present)

			var submitted []byte
			batchSrv := fakeBatchServer(t, batchRoutes{submitBody: batchJSON("batch_1", "in_progress"), gotSubmit: &submitted})
			if _, err := modelExtractor(t, batchSrv, tc.model).SubmitBatch(context.Background(), ext.ExtractInput{Events: twoEvents()}); err != nil {
				t.Fatalf("SubmitBatch: %v", err)
			}
			var batch struct {
				Requests []struct {
					Params map[string]json.RawMessage `json:"params"`
				} `json:"requests"`
			}
			if err := json.Unmarshal(submitted, &batch); err != nil || len(batch.Requests) != 1 {
				t.Fatalf("decode batch submission: %v (%d requests)", err, len(batch.Requests))
			}
			checkTemperature(t, "batch", batch.Requests[0].Params, tc.present)
		})
	}
}

func checkTemperature(t *testing.T, path string, params map[string]json.RawMessage, present bool) {
	t.Helper()
	raw, ok := params["temperature"]
	switch {
	case present && (!ok || string(raw) != "0"):
		t.Errorf("%s request temperature = %s (present %v), want 0", path, raw, ok)
	case !present && ok:
		t.Errorf("%s request carries temperature %s; this model refuses the parameter", path, raw)
	}
}

// refusalMessage is a completed message in which the model declined instead of calling the tool.
func refusalMessage(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"id": "msg_r", "type": "message", "role": "assistant", "model": "claude-haiku-5-5",
		"stop_reason": "refusal",
		"content":     []map[string]any{},
		"usage":       map[string]any{"input_tokens": 10, "output_tokens": 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestRefusalIsItsOwnSentinel: a declined window must reach the worker as ErrResponseRefused on both paths —
// never as ErrExtractorUnavailable, which the worker retries unchanged until the job is discarded, and never
// as truncation, whose remedy (a larger output ceiling) does nothing for it.
func TestRefusalIsItsOwnSentinel(t *testing.T) {
	check := func(path string, err error) {
		t.Helper()
		if !errors.Is(err, ext.ErrResponseRefused) {
			t.Fatalf("%s: err = %v, want ErrResponseRefused", path, err)
		}
		if errors.Is(err, ext.ErrExtractorUnavailable) || errors.Is(err, ext.ErrResponseTruncated) {
			t.Errorf("%s: err = %v also matches another sentinel; the worker would choose the wrong remedy", path, err)
		}
	}

	srv, _ := fakeMessages(t, http.StatusOK, refusalMessage(t))
	_, err := modelExtractor(t, srv, "claude-haiku-5-5").Extract(context.Background(), ext.ExtractInput{Events: twoEvents()})
	check("realtime", err)

	batchSrv := fakeBatchServer(t, batchRoutes{
		getBody:     batchJSON("batch_r", "ended"),
		resultsBody: `{"custom_id":"` + batchCustomID + `","result":{"type":"succeeded","message":` + refusalMessage(t) + `}}`,
	})
	_, _, err = modelExtractor(t, batchSrv, "claude-haiku-5-5").CollectBatch(context.Background(), "batch_r")
	check("batch", err)
}
