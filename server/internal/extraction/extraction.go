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
//
// maxTokens caps the provider's structured output; 0 keeps the provider default. It is logged because it
// is half of a pair — the other half is the pass's event window — and a truncated pass is the one failure
// where knowing both numbers is the whole diagnosis.
func Build(ctx context.Context, provider, apiKey, model string, maxTokens int64) (ext.Extractor, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case ProviderAnthropic:
		x, err := anthropic.New(anthropic.Config{APIKey: apiKey, Model: model, MaxTokens: maxTokens})
		if err != nil {
			// The most common cause is a missing key: surface it as a startup
			// failure naming the variable to set, rather than a silent fallback.
			return nil, fmt.Errorf("extraction: LORE_EXTRACTION_PROVIDER=anthropic requires ANTHROPIC_API_KEY: %w", err)
		}
		slog.InfoContext(ctx, "extraction provider: anthropic",
			slog.String("model", modelOrDefault(model)),
			slog.Int64("max_tokens", maxTokensOrDefault(maxTokens)))
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

// FixtureIdentity is what Identity reports for the offline extractor. It is exported because refusing it is
// a decision callers make (a measurement run against the fixture measures nothing real), and a caller should
// not have to hardcode the spelling.
const FixtureIdentity = ProviderFixture

// UnknownIdentity is what Identity reports for a provider name Build would reject.
const UnknownIdentity = "unknown"

// Identity names the extractor a configuration selects, as "provider/model" — "anthropic/claude-haiku-4-5",
// or bare "fixture" for the offline default, or "unknown" for a provider name Build would reject. It answers
// "what is distilling this deployment's memories?" from configuration alone, constructing nothing: no client,
// and in particular no API key.
//
// That last point is why this exists separately from Build. Extraction runs in the worker, but /healthz is
// the server's surface and the only place an operator — or a harness recording what it measured — can ask
// the question. Having the server call Build to answer it would put the provider key in a process that has
// no use for it, widening a secret's blast radius for the sake of a reporting field. The server reads the
// same two variables instead and reports what they select.
//
// The honest limit: this describes the configuration of the process reporting it, not an observation of the
// worker. The scaffold gives both roles the same variables and a test pins that, so a compose install cannot
// have them disagree; a hand-assembled deployment that configured them differently would be reading the
// server's answer for the worker's work.
func Identity(provider, model string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case ProviderAnthropic:
		return ProviderAnthropic + "/" + modelOrDefault(model)
	case ProviderFixture, "":
		return FixtureIdentity
	default:
		// Build fails the worker on this, so the deployment is already broken. Say so rather than invent a
		// name; callers that gate on the identity treat "unknown" as a refusal.
		return UnknownIdentity
	}
}

// modelOrDefault reports the model that will be used, for logging.
func modelOrDefault(model string) string {
	if model == "" {
		return anthropic.DefaultModel
	}
	return model
}

// maxTokensOrDefault reports the output ceiling that will be used, for logging.
func maxTokensOrDefault(maxTokens int64) int64 {
	if maxTokens <= 0 {
		return anthropic.DefaultMaxTokens
	}
	return maxTokens
}
