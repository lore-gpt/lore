package ext

import (
	"context"
	"math"
	"testing"
)

// TestFixtureEmbedderDeterministicAndShaped pins the fixture's contract: one vector per input in order,
// each of the declared dimension and unit length, deterministic (identical text → identical vector) and
// distinguishing (different text → different vector). The read path relies on all of these.
func TestFixtureEmbedderDeterministicAndShaped(t *testing.T) {
	e := FixtureEmbedder{}
	ctx := context.Background()

	if e.Dim() != fixtureEmbedDim {
		t.Fatalf("Dim() = %d, want %d", e.Dim(), fixtureEmbedDim)
	}
	if e.ModelID() != fixtureEmbedModelID {
		t.Fatalf("ModelID() = %q, want %q", e.ModelID(), fixtureEmbedModelID)
	}

	// Index 0 and 2 are the same text, index 1 differs.
	texts := []string{"auth is done", "auth is pending", "auth is done"}
	got, err := e.Embed(ctx, texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != len(texts) {
		t.Fatalf("Embed returned %d vectors, want one per input (%d)", len(got), len(texts))
	}
	for i, v := range got {
		if len(v) != e.Dim() {
			t.Errorf("vector %d has length %d, want Dim() %d", i, len(v), e.Dim())
		}
		var sumSq float64
		for _, f := range v {
			sumSq += float64(f) * float64(f)
		}
		if norm := math.Sqrt(sumSq); math.Abs(norm-1) > 1e-6 {
			t.Errorf("vector %d L2 norm = %v, want ~1 (unit length)", i, norm)
		}
	}

	if !equalVec(got[0], got[2]) {
		t.Error("identical text produced different vectors; the embedder is not deterministic")
	}
	if equalVec(got[0], got[1]) {
		t.Error("distinct text produced identical vectors")
	}

	// A second, separate call reproduces the vector exactly.
	again, err := e.Embed(ctx, []string{"auth is done"})
	if err != nil {
		t.Fatalf("Embed again: %v", err)
	}
	if !equalVec(got[0], again[0]) {
		t.Error("a repeated Embed of the same text did not reproduce the vector")
	}
}

// TestFixtureEmbedderEmptyInput proves an empty batch is a clean no-op: no vectors, no error.
func TestFixtureEmbedderEmptyInput(t *testing.T) {
	got, err := FixtureEmbedder{}.Embed(context.Background(), nil)
	if err != nil {
		t.Fatalf("Embed(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Embed(nil) returned %d vectors, want 0", len(got))
	}
}

func equalVec(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
