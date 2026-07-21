# LangGraph + Lore: two agents that stay in sync

A [LangGraph](https://langchain-ai.github.io/langgraph/) team where a **researcher** hands off to a
**writer** through Lore's shared memory — and the writer is *guaranteed* to see what the researcher just
wrote, without polling or passing state around by hand.

## The idea

In a multi-agent graph, downstream agents work over what upstream agents produced. If they read from a
store that is only *eventually* consistent, the writer can miss the researcher's latest finding and draft
from stale context. Lore closes that gap with a **read-your-writes** contract:

- The researcher **writes** its finding to the shared run; the write returns a monotonic `seq`.
- The writer **packs** the run with `min_seq` set to that `seq`. The pack is guaranteed to reflect the
  write — as a distilled memory, or as a raw tail until extraction catches up — so the handoff is exact.

`covered_seq` in the response tells the writer how far the distilled view has advanced; the write is never
dropped, even under a tight token budget.

```python
written = lore.write(run_id=run_id, agent_id="researcher", content=finding)
pack = lore.pack(run_id=run_id, query="latest on auth flow", min_seq=written.seq)
#                                                             ^ read-your-writes: the pack reflects the write
```

See [`coordinate.py`](./coordinate.py) for the full two-node graph.

## Run it

You need a running Lore and its API key. Start one with the [repo quickstart](../../README.md#quickstart-self-host),
then:

```bash
export LORE_API_KEY=...     # from ./.lore/credentials
uv run coordinate.py        # or: pip install -r <(uv export) && python coordinate.py
```

The agents' reasoning is a deterministic stub so the example needs no model key — swap the node bodies
for your own model calls. The Lore coordination is the part to keep.

## What's pinned

The SDK (`loregpt`) is installed from PyPI, exactly as you would install it, and `langgraph` is locked to
a concrete version in `uv.lock`. CI compiles the graph and lints on every change, so an SDK or LangGraph
break shows up here first.
