package workmem

import (
	"encoding/json"
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"
)

// StateKind is the event payload kind that routes a fact to working memory (the hot lane) instead of
// durable, versioned extraction. A "state" event carries a single subject/value the run updates too often
// to version in Postgres.
const StateKind = "state"

const (
	// MaxSubjectLen bounds the byte length of a state fact's entity and predicate.
	MaxSubjectLen = 256
	// DefaultMaxValueBytes bounds a state fact's value when the caller passes maxValueBytes <= 0.
	DefaultMaxValueBytes = 8 << 10 // 8 KiB
)

// ErrNotStateFact reports that a payload is not a kind:"state" event — an ordinary event a caller leaves
// to the normal (durable) write path. It is not a validation failure.
var ErrNotStateFact = errors.New("workmem: not a state fact")

// InvalidStateFactError is a kind:"state" payload that is malformed or out of limits. The HTTP layer maps
// it to 400 invalid_state_fact; its message names the offending field and reason and never echoes the
// payload value.
type InvalidStateFactError struct {
	Field  string
	Reason string
}

func (e *InvalidStateFactError) Error() string {
	return "state fact " + e.Field + " " + e.Reason
}

// StateFact is the flat single-fact convention decoded from a kind:"state" event payload:
//
//	{"kind":"state","entity":"auth","predicate":"status","value":"up"}
//
// One fact per event; the writing agent and seq come from the event itself, not the payload. A batch shape
// ({"kind":"state","facts":[...]}) can be added later without breaking this one.
type StateFact struct {
	Entity    string
	Predicate string
	Value     json.RawMessage
}

// statePayload keeps the subject fields as raw JSON so kind can be decided BEFORE their types are checked:
// a payload that has already declared itself kind:"state" but gives a non-string entity/predicate is a
// malformed state fact (loud 400), not an ordinary event.
type statePayload struct {
	Kind      string          `json:"kind"`
	Entity    json.RawMessage `json:"entity"`
	Predicate json.RawMessage `json:"predicate"`
	Value     json.RawMessage `json:"value"`
}

// ParseStateFact decodes and validates an event payload as a state fact. It returns:
//   - (fact, nil)                     for a well-formed, in-limits kind:"state" payload;
//   - (zero, ErrNotStateFact)         for a payload whose kind is not "state" (an ordinary event);
//   - (zero, *InvalidStateFactError)  for a kind:"state" payload that is malformed or out of limits.
//
// maxValueBytes bounds the value's size; <= 0 uses DefaultMaxValueBytes. raw must already be well-formed
// JSON (the request decoder guarantees it).
func ParseStateFact(raw []byte, maxValueBytes int) (StateFact, error) {
	if maxValueBytes <= 0 {
		maxValueBytes = DefaultMaxValueBytes
	}
	var p statePayload
	if err := json.Unmarshal(raw, &p); err != nil || p.Kind != StateKind {
		// Not readable as a state payload, or a different kind: leave it to the normal path. Kind is a
		// typed string, so a non-string kind also lands here (it is not a state fact).
		return StateFact{}, ErrNotStateFact
	}
	// From here the payload IS kind:"state"; any subject/value problem is a malformed state fact.
	entity, err := decodeSubject("entity", p.Entity)
	if err != nil {
		return StateFact{}, err
	}
	predicate, err := decodeSubject("predicate", p.Predicate)
	if err != nil {
		return StateFact{}, err
	}
	if len(p.Value) == 0 {
		return StateFact{}, &InvalidStateFactError{Field: "value", Reason: "is required"}
	}
	if len(p.Value) > maxValueBytes {
		return StateFact{}, &InvalidStateFactError{Field: "value", Reason: fmt.Sprintf("exceeds %d bytes", maxValueBytes)}
	}
	return StateFact{Entity: entity, Predicate: predicate, Value: p.Value}, nil
}

// decodeSubject decodes a subject field (entity/predicate) to a string and validates it. Because the
// payload is already confirmed kind:"state", an absent field or a non-string type is a malformed state
// fact — an *InvalidStateFactError — not an ordinary event.
func decodeSubject(field string, raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", &InvalidStateFactError{Field: field, Reason: "is required"}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", &InvalidStateFactError{Field: field, Reason: "must be a string"}
	}
	if err := validateSubject(field, s); err != nil {
		return "", err
	}
	return s, nil
}

// validateSubject enforces the entity/predicate rules: non-empty, within MaxSubjectLen bytes, valid UTF-8,
// and free of control characters (which would corrupt the hash-field encoding and logs).
func validateSubject(field, s string) error {
	switch {
	case s == "":
		return &InvalidStateFactError{Field: field, Reason: "is required"}
	case len(s) > MaxSubjectLen:
		return &InvalidStateFactError{Field: field, Reason: fmt.Sprintf("exceeds %d bytes", MaxSubjectLen)}
	case !utf8.ValidString(s):
		return &InvalidStateFactError{Field: field, Reason: "is not valid UTF-8"}
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return &InvalidStateFactError{Field: field, Reason: "contains a control character"}
		}
	}
	return nil
}
