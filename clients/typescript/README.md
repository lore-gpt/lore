# @loregpt/sdk

TypeScript SDK for **[Lore](https://github.com/lore-gpt/lore)** — the open-source coordination memory
layer for multi-agent AI systems.

> 🚧 **Placeholder release (0.0.1).** The real SDK lands with Lore `v0.1`. This package reserves the name
> and points you to the project.

```ts
// Coming in v0.1:
const lore = new LoreClient({ apiKey });
const { seq } = await lore.write({ runId, agentId: "researcher", content: "…" });
const pack = await lore.pack({ query: "…", scopes: { team: "platform" }, minSeq: seq, tokenBudget: 2000 });
pack.coveredSeq;   // read-your-writes, guaranteed
pack.savedTokens;  // token savings meter
```

- Website & waitlist: **https://loregpt.ai**
- Source & RFCs: **https://github.com/lore-gpt/lore**

License: Apache-2.0
