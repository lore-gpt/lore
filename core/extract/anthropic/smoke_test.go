package anthropic

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/lore-gpt/lore/core/ext"
)

// TestSmokeLiveExtraction exercises the real Anthropic Messages API end to end.
// It is skipped unless ANTHROPIC_API_KEY is set, so CI stays offline and
// deterministic; the offline tests in extract_test.go cover the adapter's logic.
// Run it deliberately:
//
//	ANTHROPIC_API_KEY=sk-... go test ./core/extract/anthropic -run TestSmokeLiveExtraction -v
func TestSmokeLiveExtraction(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set; skipping live extraction smoke")
	}

	e, err := New(Config{APIKey: key})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	res, err := e.Extract(ctx, ext.ExtractInput{
		ProjectID: "smoke",
		RunID:     "smoke",
		Events: []ext.InputEvent{
			{Seq: 1, AgentID: "planner", Payload: json.RawMessage(
				`{"message":"Decided to use PostgreSQL with pgvector as the vector store; the auth service now depends on it."}`)},
			{Seq: 2, AgentID: "coder", Payload: json.RawMessage(
				`{"message":"Merged the pgvector migration; the auth service version is now 2.4.0."}`)},
		},
	})
	if err != nil {
		t.Fatalf("live Extract: %v", err)
	}

	if len(res.Memories) == 0 && len(res.Claims) == 0 {
		t.Fatalf("live extraction distilled nothing: %+v", res)
	}
	t.Logf("live extraction: %d memories, %d claims, %d entities",
		len(res.Memories), len(res.Claims), len(res.Entities))

	// Provenance sanity: every candidate must name an event that was in the window.
	for _, m := range res.Memories {
		if m.SourceSeq != 1 && m.SourceSeq != 2 {
			t.Errorf("memory source_seq %d outside window [1,2]: %q", m.SourceSeq, m.Content)
		}
		if m.Content == "" {
			t.Errorf("memory with empty content survived: %+v", m)
		}
	}
	for _, c := range res.Claims {
		if c.SourceSeq != 1 && c.SourceSeq != 2 {
			t.Errorf("claim source_seq %d outside window [1,2]: %s/%s", c.SourceSeq, c.Entity, c.Predicate)
		}
	}
}

// TestSmokeLiveBatchExtraction exercises the real Batch API end to end: submit a window, poll until
// the batch ends, then collect and decode the result. Skipped unless ANTHROPIC_API_KEY is set. A
// batch can take minutes, so run it deliberately with a generous test timeout:
//
//	ANTHROPIC_API_KEY=sk-... go test ./core/extract/anthropic -run TestSmokeLiveBatchExtraction -v -timeout 15m
func TestSmokeLiveBatchExtraction(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set; skipping live batch extraction smoke")
	}

	e, err := New(Config{APIKey: key})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	window := ext.ExtractInput{
		ProjectID: "smoke",
		RunID:     "smoke",
		Events: []ext.InputEvent{
			{Seq: 1, AgentID: "planner", Payload: json.RawMessage(
				`{"message":"Decided to use PostgreSQL with pgvector as the vector store; the auth service now depends on it."}`)},
			{Seq: 2, AgentID: "coder", Payload: json.RawMessage(
				`{"message":"Merged the pgvector migration; the auth service version is now 2.4.0."}`)},
		},
	}

	handle, err := e.SubmitBatch(ctx, window)
	if err != nil {
		t.Fatalf("live SubmitBatch: %v", err)
	}
	t.Logf("submitted batch %q; polling for completion", handle)

	for {
		res, done, err := e.CollectBatch(ctx, handle)
		if err != nil {
			t.Fatalf("live CollectBatch: %v", err)
		}
		if done {
			if len(res.Memories) == 0 && len(res.Claims) == 0 {
				t.Fatalf("live batch extraction distilled nothing: %+v", res)
			}
			t.Logf("live batch extraction: %d memories, %d claims, %d entities",
				len(res.Memories), len(res.Claims), len(res.Entities))
			for _, m := range res.Memories {
				if m.SourceSeq != 1 && m.SourceSeq != 2 {
					t.Errorf("memory source_seq %d outside window [1,2]: %q", m.SourceSeq, m.Content)
				}
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("batch %q did not complete within the timeout: %v", handle, ctx.Err())
		case <-time.After(10 * time.Second):
		}
	}
}
