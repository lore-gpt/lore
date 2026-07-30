package config

import (
	"testing"
	"time"
)

func TestLoadRequiredVars(t *testing.T) {
	cases := []struct {
		name    string
		dbURL   string
		wantErr bool
	}{
		{"present", "postgres://x", false},
		{"missing db url", "", true},
		{"whitespace db url", "   ", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LORE_DATABASE_URL", tc.dbURL)
			_, err := Load()
			if (err != nil) != tc.wantErr {
				t.Errorf("Load() err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestLoadDefaultsAndOverrides(t *testing.T) {
	t.Setenv("LORE_DATABASE_URL", "postgres://db")
	t.Setenv("LORE_VALKEY_URL", "redis://cache")

	t.Run("addr defaults when unset", func(t *testing.T) {
		t.Setenv("LORE_ADDR", "")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.Addr != ":8080" {
			t.Errorf("Addr = %q, want :8080", c.Addr)
		}
		if c.DatabaseURL != "postgres://db" || c.ValkeyURL != "redis://cache" {
			t.Errorf("unexpected config: %+v", c)
		}
	})

	t.Run("addr override", func(t *testing.T) {
		t.Setenv("LORE_ADDR", ":9000")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.Addr != ":9000" {
			t.Errorf("Addr = %q, want :9000", c.Addr)
		}
	})

	t.Run("valkey url empty when unset", func(t *testing.T) {
		t.Setenv("LORE_VALKEY_URL", "")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.ValkeyURL != "" {
			t.Errorf("ValkeyURL = %q, want empty when unset", c.ValkeyURL)
		}
	})

	t.Run("extraction settings read from env", func(t *testing.T) {
		t.Setenv("LORE_EXTRACTION_PROVIDER", "anthropic")
		t.Setenv("LORE_EXTRACTION_MODEL", "claude-haiku-4-5")
		t.Setenv("ANTHROPIC_API_KEY", "sk-test")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.ExtractionProvider != "anthropic" || c.ExtractionModel != "claude-haiku-4-5" || c.AnthropicAPIKey != "sk-test" {
			t.Errorf("extraction config = %+v", c)
		}
	})

	t.Run("extraction provider empty when unset", func(t *testing.T) {
		t.Setenv("LORE_EXTRACTION_PROVIDER", "")
		t.Setenv("ANTHROPIC_API_KEY", "")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.ExtractionProvider != "" || c.AnthropicAPIKey != "" {
			t.Errorf("extraction config = %+v, want empty when unset", c)
		}
	})
}

// TestRetrievalPartialTimeoutParsing pins the degrade-to-default rule for the retrieval budget. The
// zero value means "use the package default", so every rejected input must map to 0 — a typo that
// parsed as an instant timeout would silently disable the dense retrieval leg it bounds, which is
// worse than ignoring the setting.
func TestRetrievalPartialTimeoutParsing(t *testing.T) {
	t.Setenv("LORE_DATABASE_URL", "postgres://db")

	for _, tc := range []struct {
		name string
		set  string
		want time.Duration
	}{
		{"unset uses the package default", "", 0},
		{"a valid duration is honoured", "2s", 2 * time.Second},
		{"sub-second is honoured", "750ms", 750 * time.Millisecond},
		{"whitespace is trimmed", "  1500ms  ", 1500 * time.Millisecond},
		{"a bare number is not a duration", "2", 0},
		{"gibberish falls back", "soon", 0},
		{"zero falls back rather than disabling the leg", "0s", 0},
		{"negative falls back rather than disabling the leg", "-5s", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LORE_RETRIEVAL_PARTIAL_TIMEOUT", tc.set)
			c, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if c.RetrievalPartialTimeout != tc.want {
				t.Errorf("RetrievalPartialTimeout = %v, want %v", c.RetrievalPartialTimeout, tc.want)
			}
		})
	}
}
