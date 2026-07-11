# RFC 0001 — Read-your-writes for agent teams (the `seq` contract)

- **Status:** 🌱 Draft
- **Discussion:** [lore-gpt/lore#10](https://github.com/lore-gpt/lore/discussions/10)

## Summary

Give multi-agent teams a **read-your-writes guarantee at the API level**: when one agent writes, any
later `pack` request can be guaranteed to reflect that write — either as a distilled memory, or as a raw
tail until extraction catches up. The client always *knows* its consistency state instead of guessing.

## Motivation

Lore's whole reason to exist is "agent B sees what agent A just did." But the natural implementation
breaks exactly that promise:

1. Agent A writes an event → `202 Accepted`. Extraction runs **asynchronously** (it's an LLM call).
2. Agent B, 200 ms later, asks for a context pack.
3. The fact isn't in `memories` yet → **B does not see it.**

Making extraction synchronous would put the write path behind LLM latency and destroy the cost/throughput
wins of batched extraction. So we need consistency *without* a synchronous write path. This is the failure
class MAST attributes ~37% of multi-agent breakdowns to — we must not reproduce it.

## Design

### 1. Server-assigned monotonic `seq` per run

Every event gets a monotonic `seq` within its run, assigned server-side via a single-row atomic increment
(`UPDATE runs SET last_seq = last_seq + 1 ... RETURNING last_seq`). No client clocks, no advisory locks.

`POST /v1/events` returns `{ event_id, seq }`. The SDK carries `seq` on the run handle.

### 2. `min_seq` on pack requests

`POST /v1/pack` optionally accepts `min_seq`. The response is guaranteed to reflect every event up to
`min_seq`:

- events already **extracted** → served as distilled memories, as usual;
- events **not yet extracted** (`seq > covered_seq`) → served as a **raw-tail** section: a clearly
  labeled, separate block of raw event summaries, marked as un-distilled.

So agent B *always* sees agent A's write — distilled if ready, raw if not.

### 3. The response tells you the truth

Every pack returns:

```jsonc
{
  "text": "...",              // the assembled, budget-fit context
  "coveredSeq": 1042,          // everything ≤ this is fully reflected
  "freshnessLagMs": 120,       // write → queryable lag, right now
  "sources": [ /* provenance */ ]
}
```

The client can assert `coveredSeq >= min_seq` and knows its consistency state — it never has to guess.

### 4. Hot working memory bypasses the wait

High-churn keys (e.g. `current_task_status`) live in a synchronous working-memory lane (see the technical
design), so the most critical coordination state is never behind extraction at all.

## Why this is a feature, not just plumbing

None of the incumbents expose this at the API-contract level. "Session guarantees for agent teams" —
`min_seq` in, `covered_seq` + `freshness_lag_ms` out — is a first-class, testable promise. The SLO
(`freshness_lag_ms` p95 < 5 s realtime) is shown to customers on the dashboard.

## Alternatives considered

- **Synchronous extraction** — kills write throughput and cost model. Rejected.
- **Client-side polling until the memory appears** — pushes the consistency problem onto every user and
  gives no guarantee. Rejected.
- **Best-effort "usually fast" with no contract** — this is the incumbent status quo; it's exactly the
  gap we're closing. Rejected.

## Open questions

- API ergonomics: is `min_seq` per-pack enough, or do we also want a run-level "wait for `seq`" mode with
  a timeout?
- Raw-tail budgeting: how much of the token budget may the raw tail consume before it should summarize?
- Cross-run / cross-team causality: `seq` is per-run; do we need a higher-level ordering for cross-run
  reads, or is scope-merge ordering enough? (Multi-region is explicitly out of scope for v1.)

Feedback very welcome — this contract is the core of Lore's category claim, so we want it stress-tested.
