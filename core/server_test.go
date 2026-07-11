package core

import (
	"context"
	"testing"

	"github.com/lore-gpt/lore/core/ext"
)

// stubPolicy is a non-default PolicyEngine used to prove an Option overrides the
// OSS default.
type stubPolicy struct{}

func (stubPolicy) Authorize(context.Context, []string, string) error { return nil }

func TestResolveExtensionsDefaults(t *testing.T) {
	e, err := resolveExtensions(nil)
	if err != nil {
		t.Fatalf("resolveExtensions: %v", err)
	}
	if _, ok := e.policy.(ext.BasicScopePolicy); !ok {
		t.Errorf("policy default = %T, want BasicScopePolicy", e.policy)
	}
	if _, ok := e.adjudicator.(ext.LWW); !ok {
		t.Errorf("adjudicator default = %T, want LWW", e.adjudicator)
	}
	if _, ok := e.metering.(ext.NoopMetering); !ok {
		t.Errorf("metering default = %T, want NoopMetering", e.metering)
	}
}

func TestResolveExtensionsOverride(t *testing.T) {
	e, err := resolveExtensions([]Option{WithPolicyEngine(stubPolicy{})})
	if err != nil {
		t.Fatalf("resolveExtensions: %v", err)
	}
	if _, ok := e.policy.(stubPolicy); !ok {
		t.Errorf("policy = %T, want stubPolicy (override)", e.policy)
	}
	// Unset options keep their defaults.
	if _, ok := e.adjudicator.(ext.LWW); !ok {
		t.Errorf("adjudicator = %T, want LWW (unchanged)", e.adjudicator)
	}
}

func TestResolveExtensionsNilRejected(t *testing.T) {
	if _, err := resolveExtensions([]Option{WithMetering(nil)}); err == nil {
		t.Error("resolveExtensions with nil metering = nil error, want error")
	}
}
