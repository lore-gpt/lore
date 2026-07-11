package config

import "testing"

func TestLoadRequiredVars(t *testing.T) {
	cases := []struct {
		name          string
		dbURL, apiKey string
		wantErr       bool
	}{
		{"both present", "postgres://x", "k", false},
		{"missing db url", "", "k", true},
		{"missing api key", "postgres://x", "", true},
		{"both missing", "", "", true},
		{"whitespace db url", "   ", "k", true},
		{"whitespace api key", "postgres://x", "  ", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LORE_DATABASE_URL", tc.dbURL)
			t.Setenv("LORE_API_KEY", tc.apiKey)
			_, err := Load()
			if (err != nil) != tc.wantErr {
				t.Errorf("Load() err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestLoadDefaultsAndOverrides(t *testing.T) {
	t.Setenv("LORE_DATABASE_URL", "postgres://db")
	t.Setenv("LORE_API_KEY", "secret")
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
		if c.DatabaseURL != "postgres://db" || c.APIKey != "secret" || c.ValkeyURL != "redis://cache" {
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
}
