package ext

import (
	"context"
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
