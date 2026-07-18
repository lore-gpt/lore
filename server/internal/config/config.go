// Package config loads the lore server configuration from LORE_-prefixed
// environment variables. It lives under server/internal so it stays out of the
// open-core import surface: only the OSS binary wires configuration,
// a downstream build supplies core.Config however it likes.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the process configuration read from the environment.
type Config struct {
	DatabaseURL string // LORE_DATABASE_URL (required)
	Addr        string // LORE_ADDR (default ":8080")
	ValkeyURL   string // LORE_VALKEY_URL (working-memory cache; unset disables the stripe)

	// WorkmemMaxValueBytes bounds a working-memory (kind:"state") fact's value at ingestion.
	// LORE_WORKMEM_MAX_VALUE_BYTES; 0 (unset/invalid) uses the package default.
	WorkmemMaxValueBytes int

	// Extraction worker settings (used by `lore worker`; ignored by serve/migrate).
	ExtractionProvider string // LORE_EXTRACTION_PROVIDER: "" (offline fixture) | "fixture" | "anthropic"
	ExtractionModel    string // LORE_EXTRACTION_MODEL: optional model override for the provider
	// AnthropicAPIKey is the provider's own key (BYOK), read from the ecosystem's
	// native ANTHROPIC_API_KEY rather than a LORE_-prefixed name so an operator who
	// already exports it needs no extra configuration.
	AnthropicAPIKey string // ANTHROPIC_API_KEY

	// Embedding settings (used by both `lore serve` — the read path embeds the query
	// — and `lore worker` — the write path embeds stored memories). Both must build
	// the same embedder from the same configuration, or the query and the stored
	// vectors land in different model spaces and retrieval returns nothing.
	EmbeddingProvider string // LORE_EMBEDDING_PROVIDER: "" (offline fixture) | "fixture" | "openai"
	EmbeddingBaseURL  string // LORE_EMBEDDING_BASE_URL: OpenAI-compatible endpoint root; empty uses the provider default
	EmbeddingModel    string // LORE_EMBEDDING_MODEL: model name passed to the provider verbatim
	EmbeddingDim      int    // LORE_EMBEDDING_DIM: vector dimension (required for a real provider)
	// EmbeddingSendDimensions includes the `dimensions` request field so an
	// OpenAI-family model truncates to EmbeddingDim. Off by default because some
	// OpenAI-compatible servers reject an unknown field; the length assert is the
	// contract regardless.
	EmbeddingSendDimensions bool // LORE_EMBEDDING_SEND_DIMENSIONS
	// EmbeddingAPIKey is the embedding endpoint's key. Optional: a self-hosted
	// endpoint that needs no auth leaves it unset. It is LORE_-prefixed (not a
	// provider-native name) because one adapter serves many backends.
	EmbeddingAPIKey string // LORE_EMBEDDING_API_KEY
}

// Load reads the configuration from the environment, applies defaults, and
// returns an error naming the first required variable that is missing.
func Load() (Config, error) {
	c := Config{
		DatabaseURL: strings.TrimSpace(os.Getenv("LORE_DATABASE_URL")),
		Addr:        getenv("LORE_ADDR", ":8080"),
		ValkeyURL:   strings.TrimSpace(os.Getenv("LORE_VALKEY_URL")),

		WorkmemMaxValueBytes: getenvInt("LORE_WORKMEM_MAX_VALUE_BYTES"),

		ExtractionProvider: strings.TrimSpace(os.Getenv("LORE_EXTRACTION_PROVIDER")),
		ExtractionModel:    strings.TrimSpace(os.Getenv("LORE_EXTRACTION_MODEL")),
		AnthropicAPIKey:    strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")),

		EmbeddingProvider:       strings.TrimSpace(os.Getenv("LORE_EMBEDDING_PROVIDER")),
		EmbeddingBaseURL:        strings.TrimSpace(os.Getenv("LORE_EMBEDDING_BASE_URL")),
		EmbeddingModel:          strings.TrimSpace(os.Getenv("LORE_EMBEDDING_MODEL")),
		EmbeddingDim:            getenvInt("LORE_EMBEDDING_DIM"),
		EmbeddingSendDimensions: getenvBool("LORE_EMBEDDING_SEND_DIMENSIONS"),
		EmbeddingAPIKey:         strings.TrimSpace(os.Getenv("LORE_EMBEDDING_API_KEY")),
	}

	for _, req := range []struct{ name, val string }{
		{"LORE_DATABASE_URL", c.DatabaseURL},
	} {
		if req.val == "" {
			return Config{}, fmt.Errorf("config: %s is required", req.name)
		}
	}
	return c, nil
}

// getenv returns the trimmed value of key, or fallback when it is unset, empty,
// or whitespace-only.
func getenv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// getenvInt returns the value of key parsed as an int, or 0 when it is unset,
// empty, or not a valid integer. A downstream consumer treats 0 as "use the
// default", so a misconfigured value degrades to the default rather than failing
// the boot.
func getenvInt(key string) int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil {
		return 0
	}
	return n
}

// getenvBool reports whether key is set to a truthy value ("1", "true", "yes",
// "on", case-insensitively). Anything else — including unset — is false, so the
// default is off.
func getenvBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
