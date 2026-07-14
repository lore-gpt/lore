package retrieval

import (
	"bytes"
	"math"
	"sort"

	"github.com/jackc/pgx/v5/pgtype"
)

// Reciprocal rank fusion (RRF) combines the ranked outputs of several retrieval legs into one order. Each
// leg returns candidates best-first; a candidate's fused score is the sum, over the legs that surfaced it,
// of 1/(k + rank) where rank is its 1-based position in that leg. RRF fuses by RANK, not by raw score, on
// purpose: the legs speak incomparable units (cosine distance, lexical relevance, graph weight) that no
// normalisation reconciles cleanly, whereas ranks are already commensurable. A document found well by two
// legs outranks one found best by a single leg — the property that makes hybrid retrieval beat either leg
// alone.
const (
	// rrfK is the fusion constant. A larger k flattens the contribution of top ranks (so agreement across
	// legs matters more than any single leg's exact position); 60 is the value the original RRF work settled
	// on and the field default. It is a named constant, not a knob, because per-leg WEIGHTING — tuning how
	// much each leg counts — is an evidence-driven change that belongs with an evaluation harness, not a
	// guess made while the leg set is still growing.
	rrfK = 60
	// legDepth is how many candidates each leg contributes to the fusion. It bounds the work each leg does
	// and the size of the set a later reranking pass reorders. Deeper than the caller's final limit so a
	// document ranked modestly by every leg can still win on agreement.
	legDepth = 50
	// scoreQuantum is the granularity at which fused scores are compared for ordering. Rounding a score to a
	// fixed quantum before comparing makes the final order insensitive to float noise in a score's low bits
	// (which can differ across runs or replicas), so scores equal to within a quantum fall through to the
	// deterministic id tie-break and identical inputs always serialise to identical bytes. This is the single
	// home of scoring-order determinism: the context-pack builder's stable sort rounds through the same helper.
	scoreQuantum = 1e-4
)

// candidate is one memory a leg surfaced. The leg's own score is deliberately dropped here: RRF consumes
// only the candidate's RANK (its position in the leg's best-first list), never the leg's raw score.
type candidate struct {
	id      pgtype.UUID
	content string
	kind    string
}

// HybridResult is one fused memory with its reciprocal-rank-fusion score. Order is by descending Score;
// Score replaces the single-leg cosine Distance, which is meaningless once ranks from several legs are
// combined.
type HybridResult struct {
	ID      pgtype.UUID
	Content string
	Kind    string
	Score   float64
}

// QuantizeScore rounds a fused score to the comparison grid (see scoreQuantum). It is the ONE place scores are
// discretised for ordering — the single determinism home every consumer that sorts scored rows shares: the
// fusion below and the context-pack builder's stable sort both round through it, so a pack orders its memories
// on exactly the grid the fused read ranked them on, immune to float noise in a score's low bits.
func QuantizeScore(f float64) int64 { return int64(math.Round(f / scoreQuantum)) }

// fuse combines the per-leg ranked candidate lists into one order by reciprocal rank fusion. perLeg maps a
// leg name to its best-first candidates; k is the RRF constant. The result is sorted by descending fused
// score, ties broken by ascending memory id — a total order independent of the order the legs were fused
// in, so the same candidate sets always produce byte-identical output.
//
// A leg is expected to surface each memory at most once; should one repeat an id, only its best (first)
// rank counts and the repeat does not advance the rank counter, so a duplicate can never inflate a score.
func fuse(perLeg map[string][]candidate, k int) []HybridResult {
	type accum struct {
		content string
		kind    string
		score   float64
	}
	scores := make(map[pgtype.UUID]*accum)

	// Fold the legs in a fixed (sorted) name order so the first-seen content/kind for an id is chosen
	// deterministically. The RRF sum is order-independent regardless; this only pins which identical copy of
	// a row's content is retained.
	names := make([]string, 0, len(perLeg))
	for name := range perLeg {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		seen := make(map[pgtype.UUID]struct{}, len(perLeg[name]))
		rank := 0 // distinct-candidate rank within this leg, 0-based (RRF is 1-based, hence k+rank+1)
		for _, c := range perLeg[name] {
			if _, dup := seen[c.id]; dup {
				continue // a repeat within one leg counts only its best rank and does not advance the rank
			}
			seen[c.id] = struct{}{}
			a := scores[c.id]
			if a == nil {
				a = &accum{content: c.content, kind: c.kind}
				scores[c.id] = a
			}
			a.score += 1.0 / float64(k+rank+1)
			rank++
		}
	}

	out := make([]HybridResult, 0, len(scores))
	for id, a := range scores {
		out = append(out, HybridResult{ID: id, Content: a.content, Kind: a.kind, Score: a.score})
	}
	sort.Slice(out, func(i, j int) bool {
		qi, qj := QuantizeScore(out[i].Score), QuantizeScore(out[j].Score)
		if qi != qj {
			return qi > qj // higher fused score first
		}
		// Equal to within a quantum: break the tie by id so the order is total and deterministic.
		a, b := out[i].ID, out[j].ID
		return bytes.Compare(a.Bytes[:], b.Bytes[:]) < 0
	})
	return out
}
