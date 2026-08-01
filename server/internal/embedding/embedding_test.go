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

// TestDescribeMatchesBuild pins Describe as a faithful stand-in for Build. Every diagnostic that names the
// running vector space (`lore doctor`, `lore models show`) reports through Describe while the server and
// worker actually run what Build returns, so the two drifting apart makes a diagnostic lie about the
// deployment it is pointed at — which is exactly how `models show` came to report MATCH while the running
// server rejected recall with a model mismatch.
func TestDescribeMatchesBuild(t *testing.T) {
	for _, cfg := range []Config{
		{},
		{Provider: ProviderFixture},
		{Provider: ProviderOpenAI, Model: "text-embedding-3-small", Dim: 1536},
		{Provider: "OpenAI", Model: "text-embedding-3-large", Dim: 3072, BaseURL: "https://example.test/v1"},
	} {
		built, buildErr := Build(context.Background(), cfg)
		modelID, dim, isFixture, descErr := Describe(cfg)

		if (buildErr == nil) != (descErr == nil) {
			t.Fatalf("cfg %+v: Build err = %v but Describe err = %v", cfg, buildErr, descErr)
		}
		if buildErr != nil {
			continue
		}
		if modelID != built.ModelID() {
			t.Errorf("cfg %+v: Describe model = %q, Build model = %q", cfg, modelID, built.ModelID())
		}
		if dim != built.Dim() {
			t.Errorf("cfg %+v: Describe dim = %d, Build dim = %d", cfg, dim, built.Dim())
		}
		_, wantFixture := built.(ext.FixtureEmbedder)
		if isFixture != wantFixture {
			t.Errorf("cfg %+v: Describe isFixture = %v, want %v", cfg, isFixture, wantFixture)
		}
	}
}

// TestDescribeReportsConfiguredProviderNotTheDefault is the direct guard on the reported identity: a
// configured real provider must never be described as the offline fixture. A diagnostic that answers with the
// OSS default regardless of configuration is worse than no diagnostic, because it contradicts /healthz and
// sends an operator debugging a 409 in the wrong direction.
func TestDescribeReportsConfiguredProviderNotTheDefault(t *testing.T) {
	fixtureModel, fixtureDim, _, err := Describe(Config{})
	if err != nil {
		t.Fatalf("Describe fixture: %v", err)
	}

	modelID, dim, isFixture, err := Describe(Config{Provider: ProviderOpenAI, Model: "text-embedding-3-small", Dim: 1536})
	if err != nil {
		t.Fatalf("Describe openai: %v", err)
	}
	if isFixture {
		t.Errorf("a configured openai provider was described as the fixture")
	}
	if modelID == fixtureModel || dim == fixtureDim {
		t.Errorf("openai described as model=%q dim=%d, which is the fixture's identity (%q/%d)",
			modelID, dim, fixtureModel, fixtureDim)
	}
	if want := "text-embedding-3-small@1536"; modelID != want {
		t.Errorf("model = %q, want %q", modelID, want)
	}
	if dim != 1536 {
		t.Errorf("dim = %d, want 1536", dim)
	}
}
