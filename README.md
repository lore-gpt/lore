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

## Quickstart (self-host)

Today Lore runs as a skeleton: a server that accepts events, persists them, and
enqueues a (stub) extraction job, and a worker that drains it. The write →
consolidate → pack API shown above lands in `v0.1`.

**Prerequisites:** Docker (with Compose) and [Task](https://taskfile.dev). Go 1.26+
is only needed to build or test outside containers.

```bash
git clone https://github.com/lore-gpt/lore
cd lore
task compose:up        # builds the image, starts the stack, waits until healthy
```

> **Port 8080 already in use?** (a local Apache/nginx, say) — pick a free host
> port; the container still listens on 8080:
> `LORE_HTTP_PORT=18080 task compose:up`, then use that port in the URLs below.

Check health — unauthenticated, so orchestrators can probe it:

```bash
curl localhost:8080/healthz
# {"status":"ok","version":"0.0.0-dev","db":"ok","queue":"ok"}
```

Append an event. Phase 0 has no run-creation endpoint yet, so seed one run
directly, then post to it:

```bash
RUN_ID=$(docker compose -f infra/docker-compose.yml exec -T paradedb \
  psql -U lore -d lore -tA -c "WITH o AS (INSERT INTO organizations(name) VALUES('demo') RETURNING id), p AS (INSERT INTO projects(org_id,name) SELECT id,'demo' FROM o RETURNING id), r AS (INSERT INTO runs(project_id) SELECT id FROM p RETURNING id) SELECT id FROM r;")

curl -X POST localhost:8080/v1/events \
  -H "Authorization: Bearer local-dev-key" \
  -H "Content-Type: application/json" \
  -d "{\"run_id\":\"$RUN_ID\",\"agent_id\":\"researcher\",\"payload\":{\"note\":\"hello memory\"}}"
# {"event_id":"..."}   (HTTP 202)
```

Within seconds the worker drains the job:

```bash
docker compose -f infra/docker-compose.yml logs lore-worker | grep "extract stub"
# ... INFO extract stub: event received event_id=...
```

Tear it down:

```bash
task compose:down
```

### Configuration

`task compose:up` runs with working defaults. To run the binary outside Compose,
copy [`.env.example`](.env.example) to `.env` and set:

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `LORE_DATABASE_URL` | yes | — | Postgres (ParadeDB) connection string |
| `LORE_API_KEY` | yes | — | bearer token required on `/v1/*` |
| `LORE_ADDR` | no | `:8080` | HTTP listen address |
| `LORE_VALKEY_URL` | no | — | Valkey URL (started by Compose, reserved for `v0.1`) |

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
