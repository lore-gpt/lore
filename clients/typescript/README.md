# @loregpt/sdk

TypeScript SDK for **[Lore](https://github.com/lore-gpt/lore)** — the open-source coordination memory
layer for multi-agent AI systems. Create runs, write events, and fetch read-your-writes context packs.

> 🚧 **Pre-release.** Lore is building in the open toward `v0.1`; this SDK tracks the live API.

**Compatibility.** SDK `0.1.x` targets the Lore **v1** API (server `≥ 0.0.1`). The SDK and the server version
independently — match on the API version, not the release number.

## Install

```bash
npm install @loregpt/sdk
```

Requires Node 18+. Zero runtime dependencies (HTTP uses the platform-native `fetch`). ESM only.

## Usage

```ts
import { LoreClient } from "@loregpt/sdk";

const lore = new LoreClient({ apiKey: process.env.LORE_API_KEY! });

// A run groups a stream of events; the project comes from your key.
const { runId } = await lore.createRun();

// Append an event. Pass `content` (a string) or `payload` (an object) — not both.
const { seq } = await lore.write({
  runId,
  agentId: "researcher",
  content: "Auth flow moved to v2 — PR #42 merged",
});

// Pack read-your-writes context. `minSeq` guarantees the pack reflects the event you just wrote.
const pack = await lore.pack({ runId, query: "current state of auth work", minSeq: seq });
pack.coveredSeq; // ≥ seq → read-your-writes, guaranteed
pack.savedTokens; // estimated tokens saved by packing
```

`writeState` writes one working-memory fact (a `kind:"state"` event) that a same-run reader sees immediately:

```ts
await lore.writeState({ runId, agentId: "researcher", entity: "auth-service", predicate: "status", value: "up" });
```

## Errors

Every failure throws a typed `LoreError`. API errors carry the server's machine `code` (discriminate on it,
never string-match) and the HTTP status; transport and parse failures are distinct classes. Requests are
**not** retried (a write is not idempotent) — a failure surfaces so the caller decides.

```ts
import { LoreError } from "@loregpt/sdk";

try {
  await lore.pack({ runId, query: "…", minSeq: 999 });
} catch (err) {
  if (err instanceof LoreError) {
    switch (err.code) {
      case "not_found": /* unknown run, or not this key's project */ break;
      case "min_seq_out_of_range": /* asked for a seq the run hasn't reached */ break;
      case "model_mismatch": /* server embedder misconfigured */ break;
      case "unauthorized": /* bad or revoked key */ break;
      case "connection": /* the request never reached the server */ break;
      default: /* other/unknown code, or a parse failure */ break;
    }
  } else throw err;
}
```

## Options

`new LoreClient({ apiKey, baseUrl?, fetch?, headers?, timeoutMs? })` — `baseUrl` defaults to
`http://localhost:8080`, `timeoutMs` to 30000, and `fetch` to the global `fetch` (inject one for a custom
transport, a proxy, or tests).

- Website: **https://loregpt.ai**
- Source & RFCs: **https://github.com/lore-gpt/lore**

License: Apache-2.0
