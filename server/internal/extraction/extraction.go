// Package extraction is the worker binary's composition seam for choosing an
// extraction provider from configuration. It lives under server/internal so the
// open-core core package never depends on a provider SDK: the binary wires a real
// extractor in via core.WithExtractor, and a downstream build can wire its own.
//
// Selection is explicit opt-in. The offline FixtureExtractor is the default; a
// real provider must be named, and the Anthropic provider additionally requires
// an API key (BYOK). This keeps a stray key in the environment from silently
// turning the worker into a paid-API caller.
package extraction

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/lore-gpt/lore/core/ext"
	"github.com/lore-gpt/lore/core/extract/anthropic"
)

// Provider names accepted in LORE_EXTRACTION_PROVIDER.
const (
	ProviderFixture   = "fixture"
	ProviderAnthropic = "anthropic"
)

// Build selects the extraction provider from configuration:
//
//   - "" (unset): the offline FixtureExtractor, with a nudge toward real extraction.
//   - "fixture": the offline FixtureExtractor, chosen explicitly (no nudge).
//   - "anthropic": the Anthropic provider; requires apiKey, else a loud error.
//   - anything else: an error, so a typo fails the worker at startup.
//
// The provider name is matched case-insensitively after trimming surrounding
// whitespace, so "anthropic", "Anthropic", and "ANTHROPIC" are equivalent. It
// returns an ext.Extractor for the caller to inject with core.WithExtractor.
func Build(ctx context.Context, provider, apiKey, model string) (ext.Extractor, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case ProviderAnthropic:
		x, err := anthropic.New(anthropic.Config{APIKey: apiKey, Model: model})
		if err != nil {
			// The most common cause is a missing key: surface it as a startup
			// failure naming the variable to set, rather than a silent fallback.
			return nil, fmt.Errorf("extraction: LORE_EXTRACTION_PROVIDER=anthropic requires ANTHROPIC_API_KEY: %w", err)
		}
		slog.InfoContext(ctx, "extraction provider: anthropic", slog.String("model", modelOrDefault(model)))
		return x, nil

	case ProviderFixture:
		slog.InfoContext(ctx, "extraction provider: fixture (offline, no real extraction)")
		return ext.FixtureExtractor{}, nil

	case "":
		slog.InfoContext(ctx, "no extraction provider set; using the offline fixture extractor (no real extraction). "+
			"Set LORE_EXTRACTION_PROVIDER=anthropic with ANTHROPIC_API_KEY for real extraction, or =fixture to select it explicitly.")
		return ext.FixtureExtractor{}, nil

	default:
		return nil, fmt.Errorf("extraction: unknown LORE_EXTRACTION_PROVIDER %q (want %q or %q)", provider, ProviderFixture, ProviderAnthropic)
	}
}

// modelOrDefault reports the model that will be used, for logging.
func modelOrDefault(model string) string {
	if model == "" {
		return anthropic.DefaultModel
	}
	return model
}
