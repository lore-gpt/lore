# Lore

**Open-source coordination memory layer for multi-agent AI systems.**

> *Mem0 remembers your user. Zep knows what's true now.*
> ***Lore keeps your agent team in sync*** *— with consistency guarantees, access control, and a token bill that goes down.*

> 🚧 **Building in the open.** Lore is pre-release. The design is public and evolving through RFCs; `v0.1` lands soon.
> **[→ Join the waitlist](https://loregpt.ai)** for early access and design-partner slots.

---

## Why

Multi-agent systems mostly don't fail because agents can't reason — they fail because agents work over
inconsistent copies of shared state. *(MAST, 1,600+ annotated traces: **36.9%** of multi-agent failures
are inter-agent misalignment; teams burn ~40% of compute re-establishing context.)*

Lore is the shared memory that keeps a **team** of agents working from one reality:

- **Consistency you can call** — `seq` tokens, `covered_seq`, `freshness_lag_ms`: read-your-writes as an
  API contract, not a blog promise. If agent A wrote it, agent B's next pack contains it.
- **Governance built in** — per-agent ACL compiled to SQL, trust tiers with quarantine, mandatory
  provenance, human-approved curation.
- **A token bill that goes down** — deterministic, budget-fit context packs maximize prompt-cache hits;
  a built-in meter reports tokens and dollars saved versus raw history.
- **Real open source** — not a library you operate around, a full server. `docker run`, one Go binary,
  Postgres inside. Apache-2.0.

## How it works

1. **Write** — agents stream events; nothing blocks.
2. **Consolidate** — facts become versioned claims; conflicts resolved by policy, not luck.
3. **Pack** — one budget-fit, provenance-tagged, deterministic context block.

```ts
const lore = new LoreClient({ apiKey });
const { seq } = await lore.write({
  runId, agentId: "researcher",
  content: "Auth flow moved to v2 — PR #42 merged",
});

const pack = await lore.pack({
  query: "current state of auth work",
  scopes: { team: "platform" }, minSeq: seq, tokenBudget: 2000,
});

pack.coveredSeq   // ≥ seq → read-your-writes, guaranteed
pack.savedTokens  // the number your CFO will ask about
```

## Works with

SDKs for **TypeScript** and **Python**, plus an **MCP server** for everything else (Claude Code, Cursor,
and any MCP client). Framework-neutral by design: LangGraph, CrewAI, AutoGen, Claude Agent SDK — no
framework shares memory with a competitor's agent; Lore does.

## Status & roadmap

Lore is being built in the open. Current focus: **`v0.1` MVP** — write → consolidate → pack, hybrid
recall, MCP server + TS/Python SDKs, minimal inspector.

- 🗺️ **Design & RFCs:** [`docs/rfcs/`](docs/rfcs) — the read-your-writes contract and the coordination
  benchmark are being designed in the open. Feedback wanted.
- 💬 **Discussion:** [GitHub Discussions](../../discussions)
- 📊 **Category framing:** `vs Mem0` / `vs Zep` comparisons at [loregpt.ai/compare](https://loregpt.ai) *(soon)*

## Open source & what's paid

The full server is **Apache-2.0**: write/read pipeline, scope model, MCP server, SDKs, basic inspector.
A hosted cloud and advanced governance (advanced ACL, curation workflow, analytics) fund the project.
The boundary is public and stable — no surprises. See
[the OSS and paid boundary](CONTRIBUTING.md#the-oss-and-paid-boundary).

## Contributing

RFCs, issues, and early design feedback are welcome — start with [CONTRIBUTING.md](CONTRIBUTING.md).
Found a security issue? See [SECURITY.md](SECURITY.md).

## License

[Apache-2.0](LICENSE) © The LoreGPT Authors
