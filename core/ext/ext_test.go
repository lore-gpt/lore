package ext

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestBasicScopePolicy(t *testing.T) {
	p := BasicScopePolicy{}
	ctx := context.Background()

	cases := []struct {
		name   string
		scopes []string
		action string
		wantOK bool
	}{
		{"exact match", []string{"events:write"}, "events:write", true},
		{"wildcard", []string{"*"}, "anything", true},
		{"one of many", []string{"a", "events:write", "b"}, "events:write", true},
		{"no match", []string{"events:read"}, "events:write", false},
		{"empty scopes", nil, "events:write", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := p.Authorize(ctx, tc.scopes, tc.action)
			if tc.wantOK && err != nil {
				t.Errorf("Authorize = %v, want nil", err)
			}
			if !tc.wantOK && !errors.Is(err, ErrPermissionDenied) {
				t.Errorf("Authorize = %v, want ErrPermissionDenied", err)
			}
		})
	}
}

func TestLWW(t *testing.T) {
	ctx := context.Background()

	// Arrival order: the incoming write always wins. The per-run seqs ride along for audit only and must
	// NOT flip the decision — here current carries a deliberately HIGHER seq (from a different run), and
	// incoming still wins, proving cross-run seqs are not compared.
	r, err := LWW{}.Resolve(ctx, Conflict{
		ProjectID: "p",
		Current:   []byte(`"cur"`), Incoming: []byte(`"inc"`),
		CurrentSource:  Provenance{RunID: "runA", Seq: 100},
		IncomingSource: Provenance{RunID: "runB", Seq: 1},
	})
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if string(r.Value) != `"inc"` {
		t.Errorf("LWW value = %q, want the incoming (arrival order; seqs are audit-only, not compared)", r.Value)
	}
	if r.Reason != lwwReason {
		t.Errorf("LWW reason = %q, want %q", r.Reason, lwwReason)
	}
}

func TestFieldMerge(t *testing.T) {
	ctx := context.Background()

	t.Run("merges objects, incoming overrides", func(t *testing.T) {
		r, err := FieldMerge{}.Resolve(ctx, Conflict{
			Current:  []byte(`{"a":1,"b":2}`),
			Incoming: []byte(`{"b":3,"c":4}`),
		})
		if err != nil {
			t.Fatalf("Resolve error: %v", err)
		}
		var got map[string]int
		if err := json.Unmarshal(r.Value, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		want := map[string]int{"a": 1, "b": 3, "c": 4}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("key %q = %d, want %d", k, got[k], v)
			}
		}
		if r.Reason != fieldMergeReason {
			t.Errorf("merge reason = %q, want %q", r.Reason, fieldMergeReason)
		}
	})

	t.Run("non-object falls back to LWW (incoming wins)", func(t *testing.T) {
		r, err := FieldMerge{}.Resolve(ctx, Conflict{
			Current: []byte(`[1,2]`), Incoming: []byte(`{"a":1}`),
		})
		if err != nil || string(r.Value) != `{"a":1}` {
			t.Errorf("Resolve = %q, %v; want incoming (LWW fallback is arrival-order)", r.Value, err)
		}
		if r.Reason != lwwReason {
			t.Errorf("fallback reason = %q, want %q (fell back to LWW)", r.Reason, lwwReason)
		}
	})
}

func TestNoopMetering(t *testing.T) {
	if err := (NoopMetering{}).Record(context.Background(), Measurement{Op: "recall", Count: 1}); err != nil {
		t.Errorf("Record = %v, want nil", err)
	}
}

// Compile-time proof the OSS defaults satisfy the extension interfaces.
var (
	_ PolicyEngine = BasicScopePolicy{}
	_ Adjudicator  = LWW{}
	_ Adjudicator  = FieldMerge{}
	_ MeteringSink = NoopMetering{}
)
