package ext

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
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
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)

	t.Run("current newer wins", func(t *testing.T) {
		r, err := LWW{}.Resolve(ctx, Conflict{
			Current: []byte(`"cur"`), Incoming: []byte(`"inc"`),
			CurrentAt: newer, IncomingAt: older,
		})
		if err != nil || string(r.Value) != `"cur"` {
			t.Errorf("Resolve = %q, %v; want \"cur\"", r.Value, err)
		}
	})
	t.Run("incoming wins ties and newer", func(t *testing.T) {
		r, err := LWW{}.Resolve(ctx, Conflict{
			Current: []byte(`"cur"`), Incoming: []byte(`"inc"`),
			CurrentAt: older, IncomingAt: older, // tie -> incoming
		})
		if err != nil || string(r.Value) != `"inc"` {
			t.Errorf("Resolve = %q, %v; want \"inc\"", r.Value, err)
		}
	})
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
	})

	t.Run("non-object falls back to LWW", func(t *testing.T) {
		older := time.Unix(0, 0)
		r, err := FieldMerge{}.Resolve(ctx, Conflict{
			Current: []byte(`[1,2]`), Incoming: []byte(`{"a":1}`),
			CurrentAt: older.Add(time.Hour), IncomingAt: older,
		})
		if err != nil || string(r.Value) != `[1,2]` {
			t.Errorf("Resolve = %q, %v; want current (LWW fallback)", r.Value, err)
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
