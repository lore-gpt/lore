# RFC 0002 — `coordination-bench`: a benchmark for multi-agent memory

- **Status:** 🌱 Draft
- **Discussion:** (open a thread in Discussions and link it here)

## Summary

Existing memory benchmarks (LongMemEval, LoCoMo) measure **single-user, single-agent** conversational
recall. None of them measure whether a **team** of agents stays coordinated. This RFC proposes
`coordination-bench`: an open-source harness with scenarios that stress the five axes of multi-agent
memory, published with reproducible results for Lore and comparable systems — under the **same judge**.

## Motivation

- Benchmark headlines are meaningless across different judges: an independent run scored one system 63.8
  and another 49.0 on the same test that vendors reported ~94 on. So step one is **same-judge
  reproduction** of the existing parity benchmarks.
- But parity isn't the category. The interesting question — *"is my agent team working from the same
  reality?"* — has no benchmark. We propose to define it, publicly and fairly (including tests where
  temporal-graph systems should look good).

## The five axes (derived from MAST failure modes)

1. **Cross-agent recall** — does agent B retrieve what agent A wrote?
2. **Concurrent-write consistency** — two agents write conflicting facts; is the resolution correct and
   deterministic?
3. **Update propagation / staleness** — when a fact changes, do later reads reflect it (and not the stale
   value)?
4. **ACL leakage rate** — does scoped/quarantined content leak across agents or teams? (Target: 0.)
5. **N-agent task token cost** — total tokens to complete a coordinated task vs a raw-history baseline.

## Design sketch

- 50–100 scenarios, each a scripted multi-agent run with a ground-truth expectation per axis.
- Open harness; anyone can run it against any system via an adapter. Third-party result PRs welcome.
- Published results table for Lore + Mem0 + Zep, same judge, harness linked. Where a competitor is
  stronger on an axis (e.g. deep temporal reasoning), we say so — trust is the point.
- Lives in its own repo (`loregpt/coordination-bench`) at launch so a would-be community standard doesn't
  sit inside a vendor's product repo. Develops in this monorepo under `evals/coordination/` until then.

## Open questions

- Judge model choice and cost ceiling (needs a variance study: same set, 3 runs, report std).
- How adversarial should the ACL-leakage scenarios be?
- Scoring: per-axis scores vs a single composite — and how to keep a composite from hiding a bad axis.

This is as much a distribution asset as a measurement tool — but it only earns trust if the methodology is
boringly fair. Design feedback wanted, especially from people who'll try to poke holes in the fairness.
