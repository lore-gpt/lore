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
	APIKey      string // LORE_API_KEY (required)
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
}

// Load reads the configuration from the environment, applies defaults, and
// returns an error naming the first required variable that is missing.
func Load() (Config, error) {
	c := Config{
		DatabaseURL: strings.TrimSpace(os.Getenv("LORE_DATABASE_URL")),
		APIKey:      strings.TrimSpace(os.Getenv("LORE_API_KEY")),
		Addr:        getenv("LORE_ADDR", ":8080"),
		ValkeyURL:   strings.TrimSpace(os.Getenv("LORE_VALKEY_URL")),

		WorkmemMaxValueBytes: getenvInt("LORE_WORKMEM_MAX_VALUE_BYTES"),

		ExtractionProvider: strings.TrimSpace(os.Getenv("LORE_EXTRACTION_PROVIDER")),
		ExtractionModel:    strings.TrimSpace(os.Getenv("LORE_EXTRACTION_MODEL")),
		AnthropicAPIKey:    strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")),
	}

	for _, req := range []struct{ name, val string }{
		{"LORE_DATABASE_URL", c.DatabaseURL},
		{"LORE_API_KEY", c.APIKey},
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
