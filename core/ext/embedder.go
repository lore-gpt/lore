package ext

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"
)

// FixtureEmbedder is a deterministic, offline Embedder for local runs and tests. It maps each text to a
// fixed-dimension unit vector derived from its SHA-256 digest, so the same text always yields the same
// vector with no API key and no network. It lets the whole write→read path run and be tested offline;
// a downstream build swaps in a real embedding provider behind the Embedder interface.
type FixtureEmbedder struct{}

const (
	// fixtureEmbedDim is small on purpose: enough to exercise vector storage and cosine ordering while
	// keeping test vectors cheap to build. A real model's dimension is far larger.
	fixtureEmbedDim = 8
	// fixtureEmbedModelID is the model space fixture vectors are stored under. Reads that query this
	// model space see them; a project on a different active model does not.
	fixtureEmbedModelID = "fixture-embed-v1"
)

// Dim reports the fixed dimension of every vector Embed returns.
func (FixtureEmbedder) Dim() int { return fixtureEmbedDim }

// ModelID identifies the fixture embedding model; embeddings are stored under it.
func (FixtureEmbedder) ModelID() string { return fixtureEmbedModelID }

// Embed returns one unit vector per text, in order, each of length Dim(). It is pure and deterministic
// and never fails: the vector is derived from the text's SHA-256 digest, then L2-normalized so cosine
// distance is well-behaved.
func (FixtureEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = fixtureVector(t)
	}
	return out, nil
}

// fixtureVector deterministically maps a string to a unit vector of length fixtureEmbedDim. Each
// component is seeded from an independent SHA-256 of (component index, text) and mapped to [-1, 1); the
// whole vector is then L2-normalized. A degenerate zero vector (vanishingly unlikely) falls back to the
// first basis vector so the result is always unit length.
func fixtureVector(s string) []float32 {
	v := make([]float32, fixtureEmbedDim)
	var sumSq float64
	for i := range v {
		h := sha256.New()
		var idx [8]byte
		binary.BigEndian.PutUint64(idx[:], uint64(i))
		h.Write(idx[:])
		h.Write([]byte(s))
		sum := h.Sum(nil)
		// Map the leading 8 bytes to [-1, 1).
		u := binary.BigEndian.Uint64(sum[:8])
		f := float64(u)/float64(math.MaxUint64)*2 - 1
		v[i] = float32(f)
		sumSq += f * f
	}
	norm := math.Sqrt(sumSq)
	if norm == 0 {
		v[0] = 1
		return v
	}
	for i := range v {
		v[i] = float32(float64(v[i]) / norm)
	}
	return v
}
