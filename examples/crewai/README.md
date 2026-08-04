# CrewAI + Lore: a crew that starts where its last run left off

A [CrewAI](https://docs.crewai.com/) crew runs twice. The second run is a **different run** that never
saw the first — and it opens with what the first one established.

## The idea

CrewAI already coordinates *within* a run. `Task(context=[earlier_task])` feeds one task's output into
the next, and that works; this example uses it, unchanged, for the scout → planner handoff. The gap is
elsewhere: that context lives in the process and lasts exactly one `kickoff()`. When the crew returns,
it is gone. The next run starts from nothing, and whatever the crew worked out has to be re-derived or
hand-carried in the prompt.

Lore gives the crew a memory that outlives a run:

- The first run writes what it established to a shared run; extraction distils it into memories.
- The second run **packs** — and gets those memories back, because a pack's distilled recall is scoped
  to the project, not to one run.

```python
recalled = lore.pack(run_id=second_run.run_id, query=topic)   # no min_seq
build_crew(prior=recalled.text).kickoff(inputs={"topic": topic})
```

**This is recall across runs, not read-your-writes within one.** `min_seq` asks "reflect the write I
just made on *this* run", which is a different question and not the one being asked here — the second
run has made no writes at all. You can watch the distinction in the output: the second run's pack comes
back with `covered_seq=0`, because coverage describes the run you are packing, while the recall arrives
anyway. (For the `min_seq` guarantee, see the [LangGraph example](../langgraph/) — it shows the other
half.)

Between the two runs the example **polls `covered_seq`** rather than sleeping. Consolidation is
asynchronous, so "has extraction caught up?" is a question with an answer, and waiting on the answer is
both faster and more honest than guessing at a duration. Only the second run needs that wait: a
distilled memory is what crosses a run boundary, whereas within a run `min_seq` makes a write visible
immediately whether or not extraction has caught up.

See [`remember.py`](./remember.py) for the whole thing.

## Run it

You need a running Lore and its API key. Start one with the [repo quickstart](../../README.md#quickstart-self-host),
then:

```bash
export LORE_API_KEY=...     # from ./.lore/credentials
uv run remember.py
```

The agents' reasoning is a deterministic stub (`OfflineLLM`), so this needs no model key — swap it for
a configured LLM and the Lore parts are unchanged. CrewAI's telemetry is disabled in the script, with
`setdefault`, so exporting the variables yourself still wins.

Expect output like:

```
run 1: b34edee6-…
run 1: distilled through seq 2
run 2: 9ff5cb36-…
run 2: recalled 4 memories from earlier runs (covered_seq=0 — this run has written nothing yet)
```

## What's pinned

The SDK (`loregpt`) is installed from PyPI, exactly as you would install it. `crewai` is pinned to an
exact version rather than a range: its `BaseLLM` surface changed inside the 1.x line, so a floating
minor could break the offline stub with no change here. CI builds the crew and lints on every change,
so an SDK or CrewAI break shows up here first.
