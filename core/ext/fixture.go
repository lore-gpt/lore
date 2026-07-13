package ext

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// FixtureExtractor is a deterministic, LLM-free Extractor for local runs and tests. It reads a
// fixed convention from each event payload, so a caller drives extraction purely through the
// events it writes — no API key, fully offline:
//
//   - "memory": "<text>"                                  → one semantic CandidateMemory
//   - "claim": {"entity","predicate","value","event_time"} → one CandidateClaim
//   - "entities": [{"name","type","aliases":[...]}, ...]    → EntityMentions
//   - "fixture_error": "unavailable"                        → the whole call fails with
//     ErrExtractorUnavailable (any other non-empty value fails with a generic error), so retry and
//     error paths — including the coalesced job's attempts — are testable offline.
//
// Each reserved key is decoded independently. A payload that is not a JSON object is skipped
// whole, but within an object a null or malformed key is ignored without discarding the event's
// other valid keys (so a typo'd "claim" never eats a good "memory"). A null "memory" contributes
// nothing; an empty-string "memory" is kept as explicitly-empty content. An event with none of the
// keys yields nothing. A fixture_error fails the whole call with no partial result; the lowest-Seq
// error wins. Output follows event order.
type FixtureExtractor struct{}

type fixtureClaim struct {
	Entity    string          `json:"entity"`
	Predicate string          `json:"predicate"`
	Value     json.RawMessage `json:"value"`
	EventTime *time.Time      `json:"event_time"`
}

type fixtureEntity struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Aliases []string `json:"aliases"`
}

// Extract distils the window per the payload convention. If any event carries a valid fixture_error
// the whole call fails with no partial result (modelling a failed provider batch); since Events are
// Seq-ordered, the lowest-Seq error wins, so the outcome is deterministic.
func (FixtureExtractor) Extract(_ context.Context, in ExtractInput) (ExtractResult, error) {
	var res ExtractResult
	for _, ev := range in.Events {
		if len(ev.Payload) == 0 {
			continue
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(ev.Payload, &keys); err != nil {
			// Payload is not a JSON object: a real extractor would simply distil nothing.
			continue
		}

		// Deliberate error injection: a well-formed, non-empty fixture_error fails the whole call.
		if raw, ok := keys["fixture_error"]; ok {
			var msg string
			if json.Unmarshal(raw, &msg) == nil && msg != "" {
				if msg == "unavailable" {
					return ExtractResult{}, ErrExtractorUnavailable
				}
				return ExtractResult{}, fmt.Errorf("ext: fixture extractor error: %s", msg)
			}
		}

		// Each reserved key decodes on its own; a null or malformed key is skipped, never the event.
		if raw, ok := keys["memory"]; ok {
			var content *string
			if json.Unmarshal(raw, &content) == nil && content != nil {
				res.Memories = append(res.Memories, CandidateMemory{
					Kind:      "semantic",
					Content:   *content,
					SourceSeq: ev.Seq,
				})
			}
		}
		if raw, ok := keys["claim"]; ok {
			var c fixtureClaim
			if json.Unmarshal(raw, &c) == nil {
				res.Claims = append(res.Claims, CandidateClaim{
					Entity:    c.Entity,
					Predicate: c.Predicate,
					Value:     c.Value,
					EventTime: c.EventTime,
					SourceSeq: ev.Seq,
				})
			}
		}
		if raw, ok := keys["entities"]; ok {
			var es []fixtureEntity
			if json.Unmarshal(raw, &es) == nil {
				for _, e := range es {
					// fixtureEntity and EntityMention share the same shape (json tags are ignored in
					// a conversion); the private type keeps the wire convention out of the public API.
					res.Entities = append(res.Entities, EntityMention(e))
				}
			}
		}
	}
	return res, nil
}

// SubmitBatch implements BatchExtractor. The fixture has no real batch backend, so the handle simply
// carries the window itself (base64-encoded JSON); the distillation happens in CollectBatch. Encoding
// the input rather than a pre-computed result is deliberate: CollectBatch then runs the very same
// Extract path, so the batch result is byte-for-byte identical to the synchronous one — round-tripping
// the distilled candidates instead would lose fidelity (a nil claim value would resurface as JSON
// null, which the write path treats differently). Submission itself never fails here; extraction
// errors surface at collect, as they would for a real batch.
func (FixtureExtractor) SubmitBatch(_ context.Context, in ExtractInput) (string, error) {
	encoded, err := json.Marshal(in)
	if err != nil {
		return "", fmt.Errorf("ext: fixture batch encode: %w", err)
	}
	return base64.StdEncoding.EncodeToString(encoded), nil
}

// CollectBatch implements BatchExtractor. It decodes the window SubmitBatch stored in the handle and
// distils it with Extract, so the result matches the synchronous path exactly. The fixture's batch is
// always immediately ready; the not-ready (done=false) path is exercised by callers supplying their
// own BatchExtractor. A window that would fail Extract (a fixture_error) fails here, modelling a
// failed batch item.
func (f FixtureExtractor) CollectBatch(ctx context.Context, handle string) (ExtractResult, bool, error) {
	decoded, err := base64.StdEncoding.DecodeString(handle)
	if err != nil {
		return ExtractResult{}, false, fmt.Errorf("ext: fixture batch handle: %w", err)
	}
	var in ExtractInput
	if err := json.Unmarshal(decoded, &in); err != nil {
		return ExtractResult{}, false, fmt.Errorf("ext: fixture batch handle: %w", err)
	}
	res, err := f.Extract(ctx, in)
	if err != nil {
		return ExtractResult{}, false, err
	}
	return res, true, nil
}
