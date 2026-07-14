package pack

import (
	"bytes"
	"sort"

	"github.com/lore-gpt/lore/core/retrieval"
	"github.com/lore-gpt/lore/core/workmem"
)

// sortMems orders one section's memories deterministically: by descending fused score, ties broken by
// ascending memory id. The score is quantised through retrieval.QuantizeScore — the SAME grid the hybrid read
// fused on — so the pack's order is a stable function of the scores, immune to float noise in a score's low
// bits, and identical inputs always render to identical bytes.
func sortMems(items []memItem) {
	sort.Slice(items, func(i, j int) bool {
		qi, qj := retrieval.QuantizeScore(items[i].score), retrieval.QuantizeScore(items[j].score)
		if qi != qj {
			return qi > qj // higher fused score first
		}
		return bytes.Compare(items[i].id.Bytes[:], items[j].id.Bytes[:]) < 0
	})
}

// sortEntries orders live working-memory facts deterministically: freshest first (highest run seq), ties
// broken by subject (entity, then predicate), so the working section is stable regardless of the cache's
// hash-iteration order.
func sortEntries(entries []workmem.Entry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Value.Seq != entries[j].Value.Seq {
			return entries[i].Value.Seq > entries[j].Value.Seq // freshest first
		}
		if entries[i].Entity != entries[j].Entity {
			return entries[i].Entity < entries[j].Entity
		}
		return entries[i].Predicate < entries[j].Predicate
	})
}

// estTokens is the v0 token estimate for a string: its characters divided by charsPerToken, rounded up. It is
// a coarse heuristic, not a real tokenizer — enough to report an approximate saving and to fit a coarse token
// budget. A real per-model tokenizer (and mid-content trimming) lands in a later increment; swapping the
// estimator changes only the numbers reported, never the pack's content or order.
func estTokens(s string) int {
	if len(s) == 0 {
		return 0
	}
	return (len(s) + charsPerToken - 1) / charsPerToken
}

// budgetFit trims the distilled sections to a coarse token budget. Walking the sections in priority order, and
// each in its already-sorted order, it keeps a memory WHOLE while the running estimate stays within budget and
// stops at the first memory that would exceed it — never splitting a memory mid-content (sentence-level
// trimming is a later increment). A non-positive budget is unbounded (everything kept). The working section
// and the raw tail are correctness content — coordination state and the read-your-writes window — and are NOT
// subject to the budget; it governs only the distilled recall. It returns the kept sections and whether
// anything was dropped.
func budgetFit(sections map[string][]memItem, order []string, budget int) (map[string][]memItem, bool) {
	if budget <= 0 {
		return sections, false
	}
	kept := make(map[string][]memItem, len(order))
	used, stopped, dropped := 0, false, false
	for _, sec := range order {
		for _, it := range sections[sec] {
			cost := estTokens(it.content)
			if stopped || used+cost > budget {
				stopped = true // stop at the first memory that would exceed; whole-item, never split
				dropped = true
				continue
			}
			kept[sec] = append(kept[sec], it)
			used += cost
		}
	}
	return kept, dropped
}
