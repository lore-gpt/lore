// Package config loads the lore server configuration from LORE_-prefixed
// environment variables. It lives under server/internal so it stays out of the
// open-core import surface (ADR-014): only the OSS binary wires configuration,
// a downstream build supplies core.Config however it likes.
package config

import (
	"fmt"
	"os"
	"strings"
)

// Config is the process configuration read from the environment.
type Config struct {
	DatabaseURL string // LORE_DATABASE_URL (required)
	APIKey      string // LORE_API_KEY (required)
	Addr        string // LORE_ADDR (default ":8080")
	ValkeyURL   string // LORE_VALKEY_URL (Phase 0: started in compose but unused)
}

// Load reads the configuration from the environment, applies defaults, and
// returns an error naming the first required variable that is missing.
func Load() (Config, error) {
	c := Config{
		DatabaseURL: strings.TrimSpace(os.Getenv("LORE_DATABASE_URL")),
		APIKey:      strings.TrimSpace(os.Getenv("LORE_API_KEY")),
		Addr:        getenv("LORE_ADDR", ":8080"),
		ValkeyURL:   strings.TrimSpace(os.Getenv("LORE_VALKEY_URL")),
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
