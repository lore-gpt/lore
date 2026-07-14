package apikey

import (
	"strings"
	"testing"
)

// TestHashDeterministicAndDistinct pins the lookup hash: stable for a given token (so the request path can
// resolve a key by hashing the presented token), distinct across tokens, and a full SHA-256 (64 hex chars).
func TestHashDeterministicAndDistinct(t *testing.T) {
	const token = "lore_sk_abc"
	if h := Hash(token); Hash(token) != h {
		t.Error("Hash must be deterministic")
	}
	if Hash("a") == Hash("b") {
		t.Error("distinct inputs must hash differently")
	}
	if got := len(Hash("x")); got != 64 {
		t.Errorf("hash length = %d, want 64 (hex-encoded SHA-256)", got)
	}
}

// TestNewMintsUsableKey proves a minted key is internally consistent: the token carries the recognisable
// prefix, its hash is exactly Hash(token) (so a presented token resolves), the stored prefix is the token's
// leading characters, and two mints differ (real entropy).
func TestNewMintsUsableKey(t *testing.T) {
	token, hash, prefix, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.HasPrefix(token, tokenPrefix) {
		t.Errorf("token %q missing %q prefix", token, tokenPrefix)
	}
	if hash != Hash(token) {
		t.Error("returned hash must equal Hash(token) — the request path hashes the presented token to look it up")
	}
	if prefix != token[:prefixLen] {
		t.Errorf("stored prefix %q must be the token's first %d chars", prefix, prefixLen)
	}
	if token2, _, _, _ := New(); token == token2 {
		t.Error("two mints produced the same token — no entropy")
	}
}
