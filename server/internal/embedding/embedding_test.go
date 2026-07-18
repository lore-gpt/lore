package embedding

import (
	"context"
	"testing"

	"github.com/lore-gpt/lore/core/embed/openai"
	"github.com/lore-gpt/lore/core/ext"
)

func TestBuildUnsetUsesFixture(t *testing.T) {
	e, err := Build(context.Background(), Config{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := e.(ext.FixtureEmbedder); !ok {
		t.Fatalf("unset provider = %T, want ext.FixtureEmbedder", e)
	}
}

func TestBuildFixtureExplicit(t *testing.T) {
	e, err := Build(context.Background(), Config{Provider: ProviderFixture})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := e.(ext.FixtureEmbedder); !ok {
		t.Fatalf("=fixture = %T, want ext.FixtureEmbedder", e)
	}
}

func TestBuildOpenAI(t *testing.T) {
	e, err := Build(context.Background(), Config{
		Provider: ProviderOpenAI,
		Model:    "text-embedding-3-small",
		Dim:      1536,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := e.(*openai.Embedder); !ok {
		t.Fatalf("=openai = %T, want *openai.Embedder", e)
	}
	if e.ModelID() != "text-embedding-3-small@1536" {
		t.Errorf("ModelID = %q, want text-embedding-3-small@1536", e.ModelID())
	}
}

func TestBuildProviderCaseInsensitive(t *testing.T) {
	for _, p := range []string{"OPENAI", "OpenAI", "  openai  "} {
		e, err := Build(context.Background(), Config{Provider: p, Model: "m", Dim: 8})
		if err != nil {
			t.Fatalf("Build(%q): %v", p, err)
		}
		if _, ok := e.(*openai.Embedder); !ok {
			t.Errorf("Build(%q) = %T, want *openai.Embedder", p, e)
		}
	}
}

func TestBuildOpenAIRequiresModelAndDim(t *testing.T) {
	// The explicit opt-in must fail loudly on a missing model or dimension, not fall
	// back to the fixture — a real deployment that mis-set the config should not
	// silently store fixture vectors.
	if _, err := Build(context.Background(), Config{Provider: ProviderOpenAI, Dim: 8}); err == nil {
		t.Error("=openai without model = nil error, want a startup error")
	}
	if _, err := Build(context.Background(), Config{Provider: ProviderOpenAI, Model: "m"}); err == nil {
		t.Error("=openai without dim = nil error, want a startup error")
	}
}

func TestBuildUnknownProviderErrors(t *testing.T) {
	if _, err := Build(context.Background(), Config{Provider: "voyage", Model: "m", Dim: 8}); err == nil {
		t.Fatal("unknown provider = nil error, want an error")
	}
}
