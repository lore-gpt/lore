package extraction

import (
	"context"
	"testing"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/extract/anthropic"
)

func TestBuildUnsetUsesFixture(t *testing.T) {
	x, err := Build(context.Background(), "", "", "", 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := x.(ext.FixtureExtractor); !ok {
		t.Fatalf("unset provider = %T, want ext.FixtureExtractor", x)
	}
}

func TestBuildUnsetWithKeyStillFixture(t *testing.T) {
	// No-surprise-spend guarantee: a key present in the environment must NOT enable
	// paid API calls on its own — only an explicit provider=anthropic does. Unset
	// stays on the offline fixture even when a real-looking key is passed.
	x, err := Build(context.Background(), "", "sk-not-used", "", 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := x.(ext.FixtureExtractor); !ok {
		t.Fatalf("unset provider with key present = %T, want ext.FixtureExtractor (no surprise spend)", x)
	}
}

func TestBuildFixtureExplicit(t *testing.T) {
	x, err := Build(context.Background(), ProviderFixture, "", "", 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := x.(ext.FixtureExtractor); !ok {
		t.Fatalf("=fixture = %T, want ext.FixtureExtractor", x)
	}
}

func TestBuildAnthropicWithKey(t *testing.T) {
	x, err := Build(context.Background(), ProviderAnthropic, "test-key", "", 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := x.(*anthropic.Extractor); !ok {
		t.Fatalf("=anthropic = %T, want *anthropic.Extractor", x)
	}
}

func TestBuildProviderCaseInsensitive(t *testing.T) {
	// Provider names match case-insensitively after trimming, so common env-var
	// habits (ANTHROPIC, Anthropic) work rather than failing as "unknown".
	for _, p := range []string{"ANTHROPIC", "Anthropic", "  anthropic  "} {
		x, err := Build(context.Background(), p, "test-key", "", 0)
		if err != nil {
			t.Fatalf("Build(%q): %v", p, err)
		}
		if _, ok := x.(*anthropic.Extractor); !ok {
			t.Errorf("Build(%q) = %T, want *anthropic.Extractor", p, x)
		}
	}
	x, err := Build(context.Background(), "FIXTURE", "", "", 0)
	if err != nil {
		t.Fatalf("Build(FIXTURE): %v", err)
	}
	if _, ok := x.(ext.FixtureExtractor); !ok {
		t.Errorf("Build(FIXTURE) = %T, want ext.FixtureExtractor", x)
	}
}

func TestBuildAnthropicRequiresKey(t *testing.T) {
	// The explicit opt-in must fail loudly when the key is missing, not silently
	// fall back to the fixture.
	if _, err := Build(context.Background(), ProviderAnthropic, "", "", 0); err == nil {
		t.Fatal("=anthropic without key = nil error, want a startup error")
	}
}

func TestBuildUnknownProviderErrors(t *testing.T) {
	if _, err := Build(context.Background(), "gpt-x", "test-key", "", 0); err == nil {
		t.Fatal("unknown provider = nil error, want an error")
	}
}

// TestIdentityNamesWhatBuildWouldCompose is the anti-drift lock between the two functions. They are
// deliberately separate — Identity constructs nothing so a keyless process can call it — which means the
// pair can silently disagree, and a /healthz that names the wrong model is worse than one that names none.
// So the identity's model half is asserted against the same default Build hands the provider.
func TestIdentityNamesWhatBuildWouldCompose(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		model    string
		want     string
	}{
		{"unset falls back to the offline fixture", "", "", FixtureIdentity},
		{"fixture chosen explicitly", "fixture", "", FixtureIdentity},
		// A model override on the fixture is not a thing the fixture reads, so it must not appear.
		{"fixture ignores a model override", "FIXTURE", "claude-haiku-4-5", FixtureIdentity},
		{"anthropic without an override names the provider default", "anthropic", "",
			ProviderAnthropic + "/" + anthropic.DefaultModel},
		{"anthropic with an override names the override", "Anthropic", "claude-sonnet-4-6",
			ProviderAnthropic + "/claude-sonnet-4-6"},
		{"whitespace and case are normalised as Build normalises them", "  ANTHROPIC  ", "",
			ProviderAnthropic + "/" + anthropic.DefaultModel},
		// Build rejects this outright, so the deployment is already failing. Reporting a name here would
		// dress up a broken configuration as a working one.
		{"a provider Build would reject is not given a name", "gpt-x", "", UnknownIdentity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Identity(tc.provider, tc.model); got != tc.want {
				t.Errorf("Identity(%q, %q) = %q, want %q", tc.provider, tc.model, got, tc.want)
			}
		})
	}
}

// TestIdentityNeedsNoKey pins the reason Identity exists at all: a process that reports the extractor's
// name must not have to hold the extractor's credential. Build refuses anthropic without a key; Identity
// must answer anyway.
func TestIdentityNeedsNoKey(t *testing.T) {
	if _, err := Build(context.Background(), ProviderAnthropic, "", "", 0); err == nil {
		t.Fatal("Build with no key succeeded; this test's premise no longer holds")
	}
	if got := Identity(ProviderAnthropic, ""); got == UnknownIdentity || got == "" {
		t.Errorf("Identity(anthropic, \"\") = %q with no key in the environment; it must name the "+
			"configured extractor without constructing one", got)
	}
}
