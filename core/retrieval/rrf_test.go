package retrieval

import (
	"math"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// mkID builds a deterministic uuid whose byte order follows n, so id tie-breaks are predictable.
func mkID(n byte) pgtype.UUID {
	var b [16]byte
	b[15] = n
	return pgtype.UUID{Bytes: b, Valid: true}
}

func indexOf(results []HybridResult, id pgtype.UUID) int {
	for i, r := range results {
		if r.ID == id {
			return i
		}
	}
	return -1
}

func scoreOf(results []HybridResult, id pgtype.UUID) float64 {
	for _, r := range results {
		if r.ID == id {
			return r.Score
		}
	}
	return -1
}

func sameOrder(a, b []HybridResult) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || quantizeScore(a[i].Score) != quantizeScore(b[i].Score) {
			return false
		}
	}
	return true
}

// TestFuseTwoLegAgreement proves the core value of rank fusion: a document surfaced by two legs outranks one
// surfaced by a single leg even at that leg's very top rank. Here the single-leg doc is rank 1 in dense
// only, while the two-leg doc is rank 2 in dense AND rank 2 in lexical — its two reciprocal-rank
// contributions sum past the single top rank.
func TestFuseTwoLegAgreement(t *testing.T) {
	idBoth, idSingle, idOther := mkID(1), mkID(2), mkID(9)
	perLeg := map[string][]candidate{
		"dense":   {{id: idSingle}, {id: idBoth}}, // ranks 1, 2
		"lexical": {{id: idOther}, {id: idBoth}},  // idBoth is rank 2; idOther is a lexical-only rank 1
	}
	got := fuse(perLeg, rrfK)

	posBoth, posSingle := indexOf(got, idBoth), indexOf(got, idSingle)
	if posBoth < 0 || posSingle < 0 {
		t.Fatalf("missing results: both=%d single=%d in %+v", posBoth, posSingle, got)
	}
	if posBoth >= posSingle {
		t.Errorf("two-leg doc at pos %d must outrank single-leg rank-1 doc at pos %d", posBoth, posSingle)
	}

	// The mechanism, not just the outcome: idBoth's score must be the SUM of its two reciprocal ranks (rank 2
	// in each leg), proving BOTH legs contributed — a mutant that drops one leg's contribution would fail here.
	wantBoth := 2.0 / float64(rrfK+2)
	if s := scoreOf(got, idBoth); math.Abs(s-wantBoth) > 1e-9 {
		t.Errorf("idBoth fused score = %v, want %v (both legs' reciprocal ranks summed)", s, wantBoth)
	}
	// Isolation: with ONLY the dense leg, idBoth (rank 2) must LOSE to idSingle (rank 1) — so it is the second
	// leg's contribution that flips the order, not something intrinsic to idBoth.
	denseOnly := fuse(map[string][]candidate{"dense": perLeg["dense"]}, rrfK)
	if indexOf(denseOnly, idSingle) >= indexOf(denseOnly, idBoth) {
		t.Errorf("dense-only: idSingle (rank 1) must outrank idBoth (rank 2); got single=%d both=%d",
			indexOf(denseOnly, idSingle), indexOf(denseOnly, idBoth))
	}
}

// TestFuseWithinLegDuplicate proves a leg that repeats an id is scored once at its best rank, never
// double-counted (the defensive dedup in fuse).
func TestFuseWithinLegDuplicate(t *testing.T) {
	id := mkID(1)
	got := fuse(map[string][]candidate{"lexical": {{id: id}, {id: id}}}, rrfK)
	if len(got) != 1 {
		t.Fatalf("duplicate id fused to %d results, want 1", len(got))
	}
	want := 1.0 / float64(rrfK+1) // best (first) rank only, not 1/(k+1)+1/(k+2)
	if math.Abs(got[0].Score-want) > 1e-9 {
		t.Errorf("duplicate id score = %v, want %v (best rank only, no double-count)", got[0].Score, want)
	}
}

// TestFusePermutationDeterminism proves the fused order is a total, input-order-independent function of the
// candidate sets: three docs each surfaced by exactly one leg at rank 1 get identical scores, so the order
// is decided purely by the id tie-break — and repeated fusions (over a map, whose iteration order Go
// randomises) must produce byte-identical output every time.
func TestFusePermutationDeterminism(t *testing.T) {
	idA, idB, idC := mkID(1), mkID(2), mkID(3)
	perLeg := map[string][]candidate{
		"dense":   {{id: idC, content: "c", kind: "semantic"}},
		"lexical": {{id: idA, content: "a", kind: "semantic"}},
		"entity":  {{id: idB, content: "b", kind: "semantic"}},
	}

	var first []HybridResult
	for i := 0; i < 25; i++ {
		got := fuse(perLeg, rrfK)
		if first == nil {
			first = got
			continue
		}
		if !sameOrder(first, got) {
			t.Fatalf("run %d order %+v differs from first %+v", i, got, first)
		}
	}

	// The three scores must be EQUAL after quantization — that is what forces the id tie-break, and pins that
	// quantizeScore is actually used in the comparator (a raw-float compare would leave the order unstable
	// across runs and fail the determinism loop above).
	if len(first) != 3 {
		t.Fatalf("got %d results, want 3", len(first))
	}
	q0 := quantizeScore(first[0].Score)
	for _, r := range first {
		if quantizeScore(r.Score) != q0 {
			t.Fatalf("scores not all equal after quantize (%v) — the id tie-break is not the deciding factor", first)
		}
	}

	// Equal scores resolve to ascending id.
	want := []pgtype.UUID{idA, idB, idC}
	for i, id := range want {
		if first[i].ID != id {
			t.Errorf("pos %d id = %v, want %v (equal scores break to ascending id)", i, first[i].ID, id)
		}
	}
}

// TestQuantizeScore pins the determinism grid: scores within one quantum collapse to the same bucket (so
// they fall to the id tie-break), and scores a quantum apart do not.
func TestQuantizeScore(t *testing.T) {
	if quantizeScore(0.010001) != quantizeScore(0.010002) {
		t.Errorf("scores within a quantum must share a bucket")
	}
	if quantizeScore(0.0100) == quantizeScore(0.0102) {
		t.Errorf("scores two quanta apart must differ")
	}
}
