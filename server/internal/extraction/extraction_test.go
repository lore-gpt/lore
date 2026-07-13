package extraction

import (
	"context"
	"testing"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/extract/anthropic"
)

func TestBuildUnsetUsesFixture(t *testing.T) {
	x, err := Build(context.Background(), "", "", "")
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
	x, err := Build(context.Background(), "", "sk-not-used", "")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := x.(ext.FixtureExtractor); !ok {
		t.Fatalf("unset provider with key present = %T, want ext.FixtureExtractor (no surprise spend)", x)
	}
}

func TestBuildFixtureExplicit(t *testing.T) {
	x, err := Build(context.Background(), ProviderFixture, "", "")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := x.(ext.FixtureExtractor); !ok {
		t.Fatalf("=fixture = %T, want ext.FixtureExtractor", x)
	}
}

func TestBuildAnthropicWithKey(t *testing.T) {
	x, err := Build(context.Background(), ProviderAnthropic, "test-key", "")
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
		x, err := Build(context.Background(), p, "test-key", "")
		if err != nil {
			t.Fatalf("Build(%q): %v", p, err)
		}
		if _, ok := x.(*anthropic.Extractor); !ok {
			t.Errorf("Build(%q) = %T, want *anthropic.Extractor", p, x)
		}
	}
	x, err := Build(context.Background(), "FIXTURE", "", "")
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
	if _, err := Build(context.Background(), ProviderAnthropic, "", ""); err == nil {
		t.Fatal("=anthropic without key = nil error, want a startup error")
	}
}

func TestBuildUnknownProviderErrors(t *testing.T) {
	if _, err := Build(context.Background(), "gpt-x", "test-key", ""); err == nil {
		t.Fatal("unknown provider = nil error, want an error")
	}
}
