# loregpt

Python SDK for **[Lore](https://github.com/lore-gpt/lore)** — the open-source coordination memory layer
for multi-agent AI systems. Create runs, write events, and fetch read-your-writes context packs.

> 🚧 **Pre-release.** Lore is building in the open toward `v0.1`; this SDK tracks the live API.

## Install

```bash
pip install loregpt
```

Requires Python 3.10+. One dependency, [httpx](https://www.python-httpx.org/), for connection pooling and a
shared sync/async transport — the de-facto standard of the AI-Python ecosystem.

## Usage

```python
lore = LoreClient(api_key=api_key)

run = lore.create_run()
result = lore.write(
    run_id=run.run_id,
    agent_id="researcher",
    content="Auth flow moved to v2 - PR #42 merged",
)

pack = lore.pack(
    run_id=run.run_id,
    query="current state of auth work",
    scopes={"team": "platform"},
    min_seq=result.seq,
    token_budget=2000,
)

covered_seq = pack.covered_seq  # >= result.seq -> read-your-writes, guaranteed
saved_tokens = pack.saved_tokens  # estimated tokens saved by packing
```

`write` takes exactly one of `content` (a string, wrapped as `{"content": ...}`) or `payload` (an opaque
dict); `write_state(run_id=..., agent_id=..., entity=..., predicate=..., value=...)` writes one working-memory
fact seen immediately by a same-run reader.

### Async

`AsyncLoreClient` has the same methods with `await`:

```python
async with AsyncLoreClient(api_key=api_key) as lore:
    run = await lore.create_run()
    result = await lore.write(run_id=run.run_id, agent_id="researcher", content="hello")
    pack = await lore.pack(run_id=run.run_id, query="state of work", min_seq=result.seq)
```

## Errors

Every failure raises a `LoreError`. Server errors carry the machine `code` (discriminate on the class or
`.code`, never string-match) and `.http_status`; transport and parse failures are distinct classes. Requests
are **not** retried (a write is not idempotent) — a failure surfaces so the caller decides.

```python
from loregpt import LoreError, NotFoundError

try:
    pack = lore.pack(run_id=run_id, query="...", min_seq=999)
except NotFoundError:
    ...  # unknown run, or not this key's project
except LoreError as err:
    print(err.code)  # e.g. "min_seq_out_of_range", "model_mismatch", "unauthorized", "connection"
```

`LoreClient(api_key, *, base_url="http://localhost:8080", timeout=..., headers=..., transport=...)` — pass a
custom httpx `transport` for a proxy, instrumentation, or tests.

- Website: **https://loregpt.ai**
- Source & RFCs: **https://github.com/lore-gpt/lore**

License: Apache-2.0
