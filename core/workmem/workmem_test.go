package workmem

import (
	"context"
	"testing"
)

func TestModeString(t *testing.T) {
	cases := map[Mode]string{Disabled: "disabled", Healthy: "ok", Degraded: "degraded"}
	for m, want := range cases {
		if got := m.String(); got != want {
			t.Errorf("Mode(%d).String() = %q, want %q", m, got, want)
		}
	}
}

func TestField(t *testing.T) {
	// Injective across ANY bytes: moving a byte across the entity/predicate boundary must not collide,
	// even when the byte is the separator or a digit/colon the length prefix uses.
	if field("a", "b\x00c") == field("a\x00b", "c") {
		t.Error("field encoding is ambiguous across the entity/predicate boundary")
	}
	// field and parseField round-trip, including names that contain the prefix's own delimiters.
	for _, c := range []struct{ e, p string }{{"svc", "state"}, {"a\x00b", "c"}, {"3:x", "y"}, {"", "p"}} {
		e, p, ok := parseField(field(c.e, c.p))
		if !ok || e != c.e || p != c.p {
			t.Errorf("parseField(field(%q,%q)) = (%q,%q,%v), want (%q,%q,true)", c.e, c.p, e, p, ok, c.e, c.p)
		}
	}
}

func TestOpenEmptyURLIsDisabled(t *testing.T) {
	s, err := Open(context.Background(), "   ")
	if err != nil {
		t.Fatalf("Open(\"\"): %v", err)
	}
	t.Cleanup(s.Close)
	if s.Mode() != Disabled {
		t.Errorf("Mode() = %v, want Disabled for an unset URL", s.Mode())
	}
	// Disabled is a no-op: Set drops, Get finds nothing, GetAll is empty — so hot facts fall through.
	if err := s.Set(context.Background(), Key{ProjectID: "p", RunID: "r", Entity: "e", Predicate: "pr"}, Value{Value: []byte(`"x"`)}); err != nil {
		t.Errorf("disabled Set: %v", err)
	}
	if _, ok, _ := s.Get(context.Background(), Key{ProjectID: "p", RunID: "r", Entity: "e", Predicate: "pr"}); ok {
		t.Error("disabled Get should find nothing")
	}
	if all, _ := s.GetAll(context.Background(), "p", "r"); len(all) != 0 {
		t.Errorf("disabled GetAll = %v, want empty", all)
	}
}

// TestOpenMalformedURLIsFatal locks the documented contract that a malformed URL fails the boot (the serve
// command surfaces it), unlike an unreachable server (which degrades). ParseURL fails synchronously, so no
// cache is needed — this stays a unit test.
func TestOpenMalformedURLIsFatal(t *testing.T) {
	for _, url := range []string{":::bad", "http://", "://nope"} {
		s, err := Open(context.Background(), url)
		if err == nil {
			if s != nil {
				s.Close()
			}
			t.Errorf("Open(%q) returned nil error, want a fatal config error", url)
		}
		if s != nil {
			t.Errorf("Open(%q) returned a non-nil store, want nil on a malformed URL", url)
		}
	}
}

func TestMemoryStore(t *testing.T) {
	ctx := context.Background()
	s := NewMemory()
	t.Cleanup(s.Close)
	if s.Mode() != Healthy {
		t.Errorf("in-memory Mode() = %v, want Healthy", s.Mode())
	}

	k := Key{ProjectID: "p", RunID: "r1", Entity: "auth", Predicate: "state"}
	if err := s.Set(ctx, k, Value{Value: []byte(`"up"`), Seq: 3, Agent: "planner"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := s.Get(ctx, k)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if string(got.Value) != `"up"` || got.Seq != 3 || got.Agent != "planner" {
		t.Errorf("Get = %+v, want {\"up\",3,planner}", got)
	}

	// Overwrite wins.
	if err := s.Set(ctx, k, Value{Value: []byte(`"down"`), Seq: 5}); err != nil {
		t.Fatalf("overwrite Set: %v", err)
	}
	if got, _, _ := s.Get(ctx, k); string(got.Value) != `"down"` || got.Seq != 5 {
		t.Errorf("after overwrite Get = %+v, want down/5", got)
	}

	// GetAll returns the run's whole working section keyed by encoded subject; other runs are isolated.
	if err := s.Set(ctx, Key{ProjectID: "p", RunID: "r1", Entity: "db", Predicate: "ver"}, Value{Value: []byte(`"2"`)}); err != nil {
		t.Fatalf("Set 2: %v", err)
	}
	if err := s.Set(ctx, Key{ProjectID: "p", RunID: "r2", Entity: "auth", Predicate: "state"}, Value{Value: []byte(`"other"`)}); err != nil {
		t.Fatalf("Set other run: %v", err)
	}
	all, err := s.GetAll(ctx, "p", "r1")
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("GetAll(r1) = %d entries, want 2 (other run isolated)", len(all))
	}
	found := false
	for _, e := range all {
		if e.Entity == "auth" && e.Predicate == "state" {
			found = true
			if string(e.Value.Value) != `"down"` {
				t.Errorf("GetAll auth/state = %s, want down", e.Value.Value)
			}
		}
	}
	if !found {
		t.Errorf("GetAll(r1) missing the auth/state entry: %+v", all)
	}
}
