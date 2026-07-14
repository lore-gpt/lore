// Package apikey mints and hashes Lore API keys. A key is a high-entropy random bearer token, not a
// user-chosen password, so it is stored and looked up by a fast cryptographic hash (SHA-256): the hash hides
// the secret at rest and is a fixed-size, index-friendly value to resolve a request by, without the cost of a
// password KDF (argon2/bcrypt buy nothing for a value that is already uniformly random, and their per-key salt
// would defeat the single-probe lookup). The raw token is shown to the operator exactly once at creation and
// never stored.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

const (
	// tokenPrefix marks a Lore secret key. A stable, recognisable prefix lets an operator (and a future secret
	// scanner) spot a key at a glance.
	tokenPrefix = "lore_sk_"
	// randBytes is the token's entropy: 256 bits of crypto/rand, far beyond guessing.
	randBytes = 32
	// prefixLen is how many leading characters of the raw token are stored as the non-secret key_prefix — enough
	// to tell keys apart in a listing without revealing the secret (tokenPrefix plus a few random characters).
	prefixLen = 12
)

// Hash returns the lookup hash of a raw token: the hex-encoded SHA-256 of its bytes. The request path hashes
// the presented token and looks the hash up; the raw token never reaches the database.
func Hash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// New mints a fresh key. It returns the raw token to show the operator ONCE, the hash to store, and the
// non-secret prefix to store for recognition. The token is tokenPrefix followed by 32 random bytes in
// url-safe base64 (no padding), so it is a single copy-pasteable word.
func New() (token, hash, keyPrefix string, err error) {
	b := make([]byte, randBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", "", err
	}
	token = tokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return token, Hash(token), token[:prefixLen], nil
}
