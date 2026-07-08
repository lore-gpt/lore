# loregpt

Python SDK for **[Lore](https://github.com/loregpt/lore)** — the open-source coordination memory layer
for multi-agent AI systems.

> 🚧 **Placeholder release (0.0.1).** The real SDK lands with Lore `v0.1`. This package reserves the name
> and points you to the project.

```python
# Coming in v0.1:
lore = LoreClient(api_key=...)
res = lore.write(run_id=run_id, agent_id="researcher", content="…")
pack = lore.pack(query="…", scopes={"team": "platform"}, min_seq=res.seq, token_budget=2000)
pack.covered_seq   # read-your-writes, guaranteed
pack.saved_tokens  # token savings meter
```

- Website & waitlist: **https://loregpt.ai**
- Source & RFCs: **https://github.com/loregpt/lore**

License: Apache-2.0
