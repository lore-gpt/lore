// Package embedding is the binary's composition seam for choosing an embedding
// provider from configuration. It lives under server/internal so the open-core
// core package never depends on a provider adapter: the binary wires a real
// embedder in via core.WithEmbedder, and a downstream build can wire its own.
//
// Selection is explicit opt-in. The offline FixtureEmbedder is the default (a
// deterministic, hermetic vector space — not a real one); a real provider must be
// named. Both `lore serve` and `lore worker` build the embedder from this one
// function, because the read path (query embedding) and the write path (stored
// memories) must share a model space or retrieval finds nothing.
package embedding

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/lore-gpt/lore/core/embed/openai"
	"github.com/lore-gpt/lore/core/ext"
)

// Provider names accepted in LORE_EMBEDDING_PROVIDER.
const (
	ProviderFixture = "fixture"
	ProviderOpenAI  = "openai"
)

// Config carries the embedding provider selection. It is a subset of the process
// configuration, passed explicitly so serve and worker build the same embedder.
type Config struct {
	Provider       string
	BaseURL        string
	APIKey         string
	Model          string
	Dim            int
	SendDimensions bool
}

// Build selects the embedding provider from cfg:
//
//   - "" (unset): the offline FixtureEmbedder, with a nudge toward a real provider.
//   - "fixture": the offline FixtureEmbedder, chosen explicitly (no nudge).
//   - "openai": an OpenAI-compatible /v1/embeddings adapter; requires a model and
//     dimension, else a loud error.
//   - anything else: an error, so a typo fails the process at startup.
//
// The provider name is matched case-insensitively after trimming. It returns an
// ext.Embedder for the caller to inject with core.WithEmbedder, logging the
// selection so a boot log shows which vector space is in effect.
func Build(ctx context.Context, cfg Config) (ext.Embedder, error) {
	e, err := build(cfg)
	if err != nil {
		return nil, err
	}
	if _, isFixture := e.(ext.FixtureEmbedder); isFixture {
		if strings.TrimSpace(cfg.Provider) == "" {
			slog.InfoContext(ctx, "no embedding provider set; using the offline fixture embedder (deterministic, not a real vector space). "+
				"Set LORE_EMBEDDING_PROVIDER=openai with LORE_EMBEDDING_MODEL and LORE_EMBEDDING_DIM for semantic recall, or =fixture to select it explicitly.")
		} else {
			slog.InfoContext(ctx, "embedding provider: fixture (offline, deterministic — not a real vector space)")
		}
		return e, nil
	}
	slog.InfoContext(ctx, "embedding provider: openai-compatible",
		slog.String("model_id", e.ModelID()), slog.String("base_url", baseURLForLog(cfg.BaseURL)))
	return e, nil
}

// build resolves the provider without logging or other side effects, so a
// diagnostic (Describe) can share the exact selection logic Build uses.
func build(cfg Config) (ext.Embedder, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case ProviderOpenAI:
		e, err := openai.New(openai.Config{
			BaseURL:        cfg.BaseURL,
			APIKey:         cfg.APIKey,
			Model:          cfg.Model,
			Dim:            cfg.Dim,
			SendDimensions: cfg.SendDimensions,
		})
		if err != nil {
			return nil, fmt.Errorf("embedding: LORE_EMBEDDING_PROVIDER=openai: %w", err)
		}
		return e, nil

	case ProviderFixture, "":
		return ext.FixtureEmbedder{}, nil

	default:
		return nil, fmt.Errorf("embedding: unknown LORE_EMBEDDING_PROVIDER %q (want %q or %q)", cfg.Provider, ProviderFixture, ProviderOpenAI)
	}
}

// Describe reports the configured embedder's model identity and whether it is the
// offline fixture, applying the same selection and validation as Build but without
// logging or network access. `lore doctor` uses it to show the active vector space
// and warn when a real install is still on the fixture.
func Describe(cfg Config) (modelID string, isFixture bool, err error) {
	e, err := build(cfg)
	if err != nil {
		return "", false, err
	}
	_, isFixture = e.(ext.FixtureEmbedder)
	return e.ModelID(), isFixture, nil
}

// baseURLForLog reports the base URL for logging, naming the default when unset.
// It never carries the API key (the key is a separate field), so it is safe to
// log.
func baseURLForLog(baseURL string) string {
	if strings.TrimSpace(baseURL) == "" {
		return "(provider default)"
	}
	return baseURL
}
