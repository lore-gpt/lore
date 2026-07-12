package ext

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// evt builds an InputEvent from a raw JSON payload string.
func evt(seq int64, agent, payload string) InputEvent {
	return InputEvent{Seq: seq, AgentID: agent, Payload: json.RawMessage(payload)}
}

func TestFixtureExtractorMemory(t *testing.T) {
	res, err := FixtureExtractor{}.Extract(context.Background(), ExtractInput{
		ProjectID: "p", RunID: "r",
		Events: []InputEvent{evt(1, "a", `{"memory":"user prefers dark mode"}`)},
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Memories) != 1 || len(res.Claims) != 0 || len(res.Entities) != 0 {
		t.Fatalf("result = %+v, want exactly one memory", res)
	}
	if m := res.Memories[0]; m.Kind != "semantic" || m.Content != "user prefers dark mode" || m.SourceSeq != 1 {
		t.Errorf("memory = %+v, want {semantic, user prefers dark mode, 1}", m)
	}
}

func TestFixtureExtractorClaim(t *testing.T) {
	const when = "2026-01-02T03:04:05Z"
	payload := `{"claim":{"entity":"invoice-7","predicate":"status","value":{"paid":true},"event_time":"` + when + `"}}`
	res, err := FixtureExtractor{}.Extract(context.Background(), ExtractInput{
		Events: []InputEvent{evt(5, "a", payload)},
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Claims) != 1 {
		t.Fatalf("claims = %d, want 1", len(res.Claims))
	}
	c := res.Claims[0]
	if c.Entity != "invoice-7" || c.Predicate != "status" || c.SourceSeq != 5 {
		t.Errorf("claim = %+v, want {invoice-7, status, ..., 5}", c)
	}
	if string(c.Value) != `{"paid":true}` {
		t.Errorf("claim value = %s, want {\"paid\":true}", c.Value)
	}
	wantTime, _ := time.Parse(time.RFC3339, when)
	if c.EventTime == nil || !c.EventTime.Equal(wantTime) {
		t.Errorf("claim event_time = %v, want %v", c.EventTime, wantTime)
	}
}

func TestFixtureExtractorEntities(t *testing.T) {
	payload := `{"entities":[{"name":"Ada","type":"person","aliases":["A."]},{"name":"Acme","type":"org"}]}`
	res, err := FixtureExtractor{}.Extract(context.Background(), ExtractInput{
		Events: []InputEvent{evt(1, "a", payload)},
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Entities) != 2 {
		t.Fatalf("entities = %d, want 2", len(res.Entities))
	}
	if e := res.Entities[0]; e.Name != "Ada" || e.Type != "person" || len(e.Aliases) != 1 || e.Aliases[0] != "A." {
		t.Errorf("entity[0] = %+v, want {Ada, person, [A.]}", e)
	}
	if e := res.Entities[1]; e.Name != "Acme" || e.Type != "org" || len(e.Aliases) != 0 {
		t.Errorf("entity[1] = %+v, want {Acme, org, []}", e)
	}
}

func TestFixtureExtractorWindowOrderedAndCombined(t *testing.T) {
	// A full window accumulates all three candidate kinds in Seq order; one event may carry several
	// kinds, and an event with no convention keys (e.g. a tool log) contributes nothing.
	res, err := FixtureExtractor{}.Extract(context.Background(), ExtractInput{
		Events: []InputEvent{
			evt(1, "a", `{"memory":"first"}`),
			evt(2, "b", `{"tool":"log","exit":0}`),
			evt(3, "a", `{"memory":"third","claim":{"entity":"e","predicate":"p","value":1}}`),
			evt(4, "a", `{"entities":[{"name":"Ada","type":"person"}]}`),
		},
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Memories) != 2 || len(res.Claims) != 1 || len(res.Entities) != 1 {
		t.Fatalf("result lengths = %d/%d/%d memories/claims/entities, want 2/1/1", len(res.Memories), len(res.Claims), len(res.Entities))
	}
	if res.Memories[0].Content != "first" || res.Memories[0].SourceSeq != 1 {
		t.Errorf("memory[0] = %+v, want {first, 1}", res.Memories[0])
	}
	if res.Memories[1].Content != "third" || res.Memories[1].SourceSeq != 3 {
		t.Errorf("memory[1] = %+v, want {third, 3}", res.Memories[1])
	}
	if res.Claims[0].SourceSeq != 3 {
		t.Errorf("claim source seq = %d, want 3", res.Claims[0].SourceSeq)
	}
	if res.Entities[0].Name != "Ada" {
		t.Errorf("entity = %+v, want Ada", res.Entities[0])
	}
}

func TestFixtureExtractorSkipAndPerKeyTolerance(t *testing.T) {
	res, err := FixtureExtractor{}.Extract(context.Background(), ExtractInput{
		Events: []InputEvent{
			evt(1, "a", `{}`),                                 // empty object -> nothing
			evt(2, "a", ``),                                   // empty payload -> nothing
			evt(3, "a", `[1,2,3]`),                            // not an object -> whole event skipped
			evt(4, "a", `{"unrelated":true}`),                 // no reserved keys -> nothing
			evt(5, "a", `{"memory":"keep-me","claim":"bad"}`), // malformed claim must not drop the memory
			evt(6, "a", `{"memory":"keep-2","entities":42}`),  // malformed entities must not drop the memory
		},
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Claims) != 0 || len(res.Entities) != 0 {
		t.Errorf("malformed sibling keys must be skipped: claims=%d entities=%d, want 0/0", len(res.Claims), len(res.Entities))
	}
	if len(res.Memories) != 2 {
		t.Fatalf("memories = %d, want 2 (a malformed sibling key must not discard a valid memory)", len(res.Memories))
	}
	if res.Memories[0].Content != "keep-me" || res.Memories[0].SourceSeq != 5 {
		t.Errorf("memory[0] = %+v, want {keep-me, 5}", res.Memories[0])
	}
	if res.Memories[1].Content != "keep-2" || res.Memories[1].SourceSeq != 6 {
		t.Errorf("memory[1] = %+v, want {keep-2, 6}", res.Memories[1])
	}
}

func TestFixtureExtractorFieldEdgeCases(t *testing.T) {
	ctx := context.Background()

	t.Run("memory null contributes nothing, empty string is kept", func(t *testing.T) {
		res, err := FixtureExtractor{}.Extract(ctx, ExtractInput{
			Events: []InputEvent{
				evt(1, "a", `{"memory":null}`),
				evt(2, "a", `{"memory":""}`),
			},
		})
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if len(res.Memories) != 1 {
			t.Fatalf("memories = %d, want 1 (null dropped, empty-string kept)", len(res.Memories))
		}
		if res.Memories[0].Content != "" || res.Memories[0].SourceSeq != 2 {
			t.Errorf("memory = %+v, want {empty content, 2}", res.Memories[0])
		}
	})

	t.Run("claim without value or event_time leaves those nil", func(t *testing.T) {
		res, err := FixtureExtractor{}.Extract(ctx, ExtractInput{
			Events: []InputEvent{evt(1, "a", `{"claim":{"entity":"e","predicate":"p"}}`)},
		})
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if len(res.Claims) != 1 {
			t.Fatalf("claims = %d, want 1", len(res.Claims))
		}
		if res.Claims[0].Value != nil {
			t.Errorf("claim value = %s, want nil", res.Claims[0].Value)
		}
		if res.Claims[0].EventTime != nil {
			t.Errorf("claim event_time = %v, want nil (non-temporal claim)", res.Claims[0].EventTime)
		}
	})
}

func TestFixtureExtractorErrorInjection(t *testing.T) {
	t.Run("unavailable maps to ErrExtractorUnavailable and yields no partial result", func(t *testing.T) {
		// Pre-load all three candidate kinds before the error so the no-partial-result invariant is
		// proven for the whole ExtractResult, not just memories.
		res, err := FixtureExtractor{}.Extract(context.Background(), ExtractInput{
			Events: []InputEvent{
				evt(1, "a", `{"memory":"discarded"}`),
				evt(2, "a", `{"claim":{"entity":"e","predicate":"p","value":1}}`),
				evt(3, "a", `{"entities":[{"name":"X","type":"t"}]}`),
				evt(4, "a", `{"fixture_error":"unavailable"}`),
			},
		})
		if !errors.Is(err, ErrExtractorUnavailable) {
			t.Fatalf("err = %v, want ErrExtractorUnavailable", err)
		}
		if len(res.Memories) != 0 || len(res.Claims) != 0 || len(res.Entities) != 0 {
			t.Errorf("a failed batch must return no partial result, got %+v", res)
		}
	})
	t.Run("other value is a generic non-nil error", func(t *testing.T) {
		_, err := FixtureExtractor{}.Extract(context.Background(), ExtractInput{
			Events: []InputEvent{evt(1, "a", `{"fixture_error":"boom"}`)},
		})
		if err == nil || errors.Is(err, ErrExtractorUnavailable) {
			t.Fatalf("err = %v, want a generic non-ErrExtractorUnavailable error", err)
		}
	})
	t.Run("lowest-seq error wins (deterministic)", func(t *testing.T) {
		_, err := FixtureExtractor{}.Extract(context.Background(), ExtractInput{
			Events: []InputEvent{
				evt(1, "a", `{"fixture_error":"unavailable"}`),
				evt(2, "a", `{"fixture_error":"boom"}`),
			},
		})
		if !errors.Is(err, ErrExtractorUnavailable) {
			t.Errorf("err = %v, want the seq-1 ErrExtractorUnavailable to win", err)
		}
	})
}

// Compile-time proof the OSS default satisfies the Extractor interface.
var _ Extractor = FixtureExtractor{}
