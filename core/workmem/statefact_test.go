package workmem

import (
	"errors"
	"strings"
	"testing"
)

func TestParseStateFact(t *testing.T) {
	// A subject at the byte limit is valid; one byte over is not.
	maxSubject := strings.Repeat("e", MaxSubjectLen)
	overSubject := strings.Repeat("e", MaxSubjectLen+1)

	cases := []struct {
		name         string
		payload      string
		maxValue     int
		wantErr      error  // sentinel to match with errors.Is; nil means "expect a valid fact or an InvalidStateFactError"
		wantInvalid  bool   // expect an *InvalidStateFactError
		wantField    string // expected InvalidStateFactError.Field
		wantEntity   string
		wantPred     string
		wantValueStr string
	}{
		{
			name:         "valid fact",
			payload:      `{"kind":"state","entity":"auth","predicate":"status","value":"up"}`,
			wantEntity:   "auth",
			wantPred:     "status",
			wantValueStr: `"up"`,
		},
		{
			name:         "value may be any json (object)",
			payload:      `{"kind":"state","entity":"db","predicate":"cfg","value":{"pool":10}}`,
			wantEntity:   "db",
			wantPred:     "cfg",
			wantValueStr: `{"pool":10}`,
		},
		{
			name:         "json null is a valid value",
			payload:      `{"kind":"state","entity":"e","predicate":"p","value":null}`,
			wantEntity:   "e",
			wantPred:     "p",
			wantValueStr: `null`,
		},
		{
			name:         "subject at the byte limit is valid",
			payload:      `{"kind":"state","entity":"` + maxSubject + `","predicate":"p","value":1}`,
			wantEntity:   maxSubject,
			wantPred:     "p",
			wantValueStr: `1`,
		},
		{name: "not state kind", payload: `{"kind":"note","entity":"a","predicate":"b","value":1}`, wantErr: ErrNotStateFact},
		{name: "no kind field", payload: `{"entity":"a","predicate":"b","value":1}`, wantErr: ErrNotStateFact},
		{name: "kind is not a string", payload: `{"kind":5}`, wantErr: ErrNotStateFact},
		{name: "payload is a json array", payload: `[1,2,3]`, wantErr: ErrNotStateFact},

		{name: "entity missing", payload: `{"kind":"state","predicate":"b","value":1}`, wantInvalid: true, wantField: "entity"},
		{name: "entity blank", payload: `{"kind":"state","entity":"","predicate":"b","value":1}`, wantInvalid: true, wantField: "entity"},
		// A confirmed state kind with a non-string subject is malformed, NOT an ordinary event.
		{name: "entity is a number", payload: `{"kind":"state","entity":5,"predicate":"b","value":1}`, wantInvalid: true, wantField: "entity"},
		{name: "entity is an array", payload: `{"kind":"state","entity":["a"],"predicate":"b","value":1}`, wantInvalid: true, wantField: "entity"},
		{name: "predicate is a bool", payload: `{"kind":"state","entity":"a","predicate":true,"value":1}`, wantInvalid: true, wantField: "predicate"},
		{name: "entity is json null", payload: `{"kind":"state","entity":null,"predicate":"b","value":1}`, wantInvalid: true, wantField: "entity"},
		{name: "entity over limit", payload: `{"kind":"state","entity":"` + overSubject + `","predicate":"b","value":1}`, wantInvalid: true, wantField: "entity"},
		// \t is a JSON escape here; the decoded predicate/entity holds a real tab (a control character).
		{name: "entity control char", payload: `{"kind":"state","entity":"a\tb","predicate":"b","value":1}`, wantInvalid: true, wantField: "entity"},
		{name: "predicate missing", payload: `{"kind":"state","entity":"a","value":1}`, wantInvalid: true, wantField: "predicate"},
		{name: "predicate control char", payload: `{"kind":"state","entity":"a","predicate":"b\nc","value":1}`, wantInvalid: true, wantField: "predicate"},
		{name: "value missing", payload: `{"kind":"state","entity":"a","predicate":"b"}`, wantInvalid: true, wantField: "value"},
		{name: "value over limit", payload: `{"kind":"state","entity":"a","predicate":"b","value":"toolong"}`, maxValue: 4, wantInvalid: true, wantField: "value"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fact, err := ParseStateFact([]byte(tc.payload), tc.maxValue)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if tc.wantInvalid {
				var ive *InvalidStateFactError
				if !errors.As(err, &ive) {
					t.Fatalf("err = %v, want *InvalidStateFactError", err)
				}
				if ive.Field != tc.wantField {
					t.Errorf("invalid field = %q, want %q", ive.Field, tc.wantField)
				}
				// An ordinary event must not be mistaken for a malformed state fact.
				if errors.Is(err, ErrNotStateFact) {
					t.Error("an invalid state fact must not report ErrNotStateFact")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if fact.Entity != tc.wantEntity || fact.Predicate != tc.wantPred {
				t.Errorf("subject = (%q,%q), want (%q,%q)", fact.Entity, fact.Predicate, tc.wantEntity, tc.wantPred)
			}
			if string(fact.Value) != tc.wantValueStr {
				t.Errorf("value = %s, want %s", fact.Value, tc.wantValueStr)
			}
		})
	}
}

// TestDecodeStateFactSkipsSizeCap proves DecodeStateFact validates the fact but does NOT apply the
// value-size cap (ParseStateFact does) — a consumer re-reading an already-stored fact must not reject it
// under a since-lowered limit.
func TestDecodeStateFactSkipsSizeCap(t *testing.T) {
	big := `{"kind":"state","entity":"e","predicate":"p","value":"` + strings.Repeat("x", DefaultMaxValueBytes+100) + `"}`
	if _, err := DecodeStateFact([]byte(big)); err != nil {
		t.Fatalf("DecodeStateFact rejected an oversized value: %v (it must skip the size cap)", err)
	}
	if _, err := ParseStateFact([]byte(big), DefaultMaxValueBytes); err == nil {
		t.Error("ParseStateFact accepted an oversized value; the cap must apply at ingestion")
	}
	// A normal fact still decodes, and still enforces the subject rules.
	fact, err := DecodeStateFact([]byte(`{"kind":"state","entity":"e","predicate":"p","value":"ok"}`))
	if err != nil || fact.Entity != "e" || string(fact.Value) != `"ok"` {
		t.Errorf("DecodeStateFact = %+v, err %v; want {e,p,\"ok\"}", fact, err)
	}
	if _, err := DecodeStateFact([]byte(`{"kind":"state","predicate":"p","value":1}`)); err == nil {
		t.Error("DecodeStateFact accepted a missing entity; subject validation must still apply")
	}
}

// TestParseStateFactDefaultValueLimit proves maxValueBytes <= 0 falls back to the package default rather
// than rejecting every value.
func TestParseStateFactDefaultValueLimit(t *testing.T) {
	fact, err := ParseStateFact([]byte(`{"kind":"state","entity":"a","predicate":"b","value":"ok"}`), 0)
	if err != nil {
		t.Fatalf("with maxValueBytes=0 (use default), got error: %v", err)
	}
	if string(fact.Value) != `"ok"` {
		t.Errorf("value = %s, want \"ok\"", fact.Value)
	}
}
