package ext

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
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

// TestFixtureExtractorMemoryKeyAliases pins the payload shapes the SDKs, the MCP server and the
// quickstart emit. Before these aliases the offline default distilled nothing from any of them, so
// every documented write path produced an empty memory store; the aliases and this test keep the
// convention and the shipped clients from drifting apart again.
func TestFixtureExtractorMemoryKeyAliases(t *testing.T) {
	ctx := context.Background()

	t.Run("content and note each distil one memory", func(t *testing.T) {
		res, err := FixtureExtractor{}.Extract(ctx, ExtractInput{
			Events: []InputEvent{
				evt(1, "a", `{"content":"auth flow moved to v2"}`),
				evt(2, "b", `{"note":"deploy freeze starts Friday"}`),
			},
		})
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if len(res.Memories) != 2 {
			t.Fatalf("memories = %d, want 2", len(res.Memories))
		}
		if m := res.Memories[0]; m.Kind != "semantic" || m.Content != "auth flow moved to v2" || m.SourceSeq != 1 {
			t.Errorf("memory[0] = %+v, want {semantic, auth flow moved to v2, 1}", m)
		}
		if m := res.Memories[1]; m.Kind != "semantic" || m.Content != "deploy freeze starts Friday" || m.SourceSeq != 2 {
			t.Errorf("memory[1] = %+v, want {semantic, deploy freeze starts Friday, 2}", m)
		}
	})

	// The keys share one slot: a payload carrying several must not multiply into several memories.
	t.Run("one memory per event, highest-priority key wins", func(t *testing.T) {
		for _, tc := range []struct{ name, payload, want string }{
			{"all three", `{"memory":"m","content":"c","note":"n"}`, "m"},
			{"memory over note", `{"memory":"m","note":"n"}`, "m"},
			{"content over note", `{"content":"c","note":"n"}`, "c"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				res, err := FixtureExtractor{}.Extract(ctx, ExtractInput{
					Events: []InputEvent{evt(1, "a", tc.payload)},
				})
				if err != nil {
					t.Fatalf("Extract: %v", err)
				}
				if len(res.Memories) != 1 {
					t.Fatalf("memories = %d, want exactly 1 (the keys share one slot)", len(res.Memories))
				}
				if got := res.Memories[0].Content; got != tc.want {
					t.Errorf("content = %q, want %q", got, tc.want)
				}
			})
		}
	})

	// A higher-priority key that yields nothing must not swallow the event: it contributes no
	// memory of its own, so the next key is still consulted.
	t.Run("a null or malformed higher-priority key falls through", func(t *testing.T) {
		res, err := FixtureExtractor{}.Extract(ctx, ExtractInput{
			Events: []InputEvent{
				evt(1, "a", `{"memory":null,"content":"from content"}`),
				evt(2, "a", `{"memory":42,"note":"from note"}`),
				evt(3, "a", `{"content":null,"note":"note wins"}`),
			},
		})
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		want := []string{"from content", "from note", "note wins"}
		if len(res.Memories) != len(want) {
			t.Fatalf("memories = %d, want %d", len(res.Memories), len(want))
		}
		for i, w := range want {
			if got := res.Memories[i].Content; got != w {
				t.Errorf("memory[%d] = %q, want %q", i, got, w)
			}
		}
	})

	// Per-key tolerance still holds for the aliases: a malformed sibling never eats the memory.
	t.Run("an alias coexists with claim and entities", func(t *testing.T) {
		res, err := FixtureExtractor{}.Extract(ctx, ExtractInput{
			Events: []InputEvent{evt(7, "a",
				`{"content":"payments moved to gRPC","claim":{"entity":"payments","predicate":"protocol","value":"grpc"},"entities":[{"name":"payments","type":"service"}]}`)},
		})
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if len(res.Memories) != 1 || len(res.Claims) != 1 || len(res.Entities) != 1 {
			t.Fatalf("result = %+v, want one memory, one claim, one entity", res)
		}
		if res.Memories[0].Content != "payments moved to gRPC" || res.Memories[0].SourceSeq != 7 {
			t.Errorf("memory = %+v, want {payments moved to gRPC, 7}", res.Memories[0])
		}
	})

	// An event with no memory key at all still yields nothing — the aliases widen the convention,
	// they do not make the fixture guess at arbitrary payloads.
	t.Run("an unrelated key still distils nothing", func(t *testing.T) {
		res, err := FixtureExtractor{}.Extract(ctx, ExtractInput{
			Events: []InputEvent{evt(1, "a", `{"observation":"not a recognised key"}`)},
		})
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if len(res.Memories) != 0 {
			t.Errorf("memories = %d, want 0", len(res.Memories))
		}
	})
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

func TestFixtureExtractorBatchRoundTrip(t *testing.T) {
	ctx := context.Background()
	// A window mixing every candidate kind, including a valueless claim (nil Value) and a temporal
	// one — the batch path must reproduce the synchronous result exactly, nil-ness and all.
	in := ExtractInput{
		ProjectID: "p", RunID: "r",
		Events: []InputEvent{
			evt(1, "a", `{"memory":"batched fact"}`),
			evt(2, "b", `{"tool":"log"}`),                           // gated-style noise contributes nothing
			evt(3, "a", `{"claim":{"entity":"e","predicate":"p"}}`), // valueless → Value must stay nil
			evt(4, "a", `{"claim":{"entity":"deploy","predicate":"status","value":"done","event_time":"2026-01-02T03:04:05Z"}}`),
			evt(5, "a", `{"entities":[{"name":"E","type":"svc","aliases":["e1","e2"]}]}`),
		},
	}

	handle, err := FixtureExtractor{}.SubmitBatch(ctx, in)
	if err != nil {
		t.Fatalf("SubmitBatch: %v", err)
	}
	if handle == "" {
		t.Fatal("SubmitBatch returned an empty handle")
	}

	res, done, err := FixtureExtractor{}.CollectBatch(ctx, handle)
	if err != nil {
		t.Fatalf("CollectBatch: %v", err)
	}
	if !done {
		t.Fatal("CollectBatch done = false, want the fixture batch to be immediately ready")
	}

	// The batch path must distil exactly what the synchronous path would — every field, not just
	// counts. A result round-trip would fail this on the valueless claim (nil Value becoming JSON null).
	want, err := FixtureExtractor{}.Extract(ctx, in)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !reflect.DeepEqual(res, want) {
		t.Fatalf("batch result != sync result\n batch = %+v\n  sync = %+v", res, want)
	}

	// Pin the fidelity-critical case explicitly: the valueless claim's Value stays nil (the write path
	// drops a nil value but keeps a literal JSON null — the two must never be conflated).
	if len(res.Claims) != 2 {
		t.Fatalf("claims = %d, want 2", len(res.Claims))
	}
	if res.Claims[0].SourceSeq != 3 || res.Claims[0].Value != nil {
		t.Errorf("valueless claim = %+v (value=%q), want seq 3 and a nil value", res.Claims[0], res.Claims[0].Value)
	}
	if res.Claims[1].SourceSeq != 4 || string(res.Claims[1].Value) != `"done"` || res.Claims[1].EventTime == nil {
		t.Errorf("valued claim = %+v (value=%q), want seq 4, value \"done\", non-nil event_time", res.Claims[1], res.Claims[1].Value)
	}
}

func TestFixtureExtractorBatchErrorSurfacesAtCollect(t *testing.T) {
	ctx := context.Background()
	// Submission succeeds; the extraction failure surfaces at collect (modelling a failed batch item),
	// with no partial result and done=false.
	in := ExtractInput{Events: []InputEvent{
		evt(1, "a", `{"memory":"discarded"}`),
		evt(2, "a", `{"fixture_error":"unavailable"}`),
	}}
	handle, err := FixtureExtractor{}.SubmitBatch(ctx, in)
	if err != nil {
		t.Fatalf("SubmitBatch: %v (submission should not fail; extraction errors surface at collect)", err)
	}

	res, done, err := FixtureExtractor{}.CollectBatch(ctx, handle)
	if !errors.Is(err, ErrExtractorUnavailable) {
		t.Fatalf("CollectBatch err = %v, want ErrExtractorUnavailable", err)
	}
	if done {
		t.Error("CollectBatch done = true on error, want false")
	}
	if len(res.Memories) != 0 || len(res.Claims) != 0 || len(res.Entities) != 0 {
		t.Errorf("a failed collect must return no partial result, got %+v", res)
	}
}

func TestFixtureExtractorCollectBatchRejectsBadHandle(t *testing.T) {
	var f FixtureExtractor
	_, _, err := f.CollectBatch(context.Background(), "not-base64!!")
	if err == nil {
		t.Error("CollectBatch with a malformed handle = nil error, want an error")
	}
}

// Compile-time proof the OSS default satisfies the Extractor interface and the optional batch one.
var (
	_ Extractor      = FixtureExtractor{}
	_ BatchExtractor = FixtureExtractor{}
)
