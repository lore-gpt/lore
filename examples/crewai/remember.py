"""A CrewAI crew that starts where its last run left off.

CrewAI already coordinates *within* a run: ``Task(context=[earlier_task])`` feeds one task's output
into the next, and that works well. But that context lives in the process and lasts one ``kickoff()``
— when the crew returns, it is gone. The next run starts from nothing, and anything the crew figured
out has to be re-derived or hand-carried in the prompt.

This example gives the crew a memory that outlives a run. The first kickoff writes what its scout
found to Lore. The second kickoff is a **different run** that never saw the first, and it opens with
what the first learned, because a pack's distilled recall is scoped to the project rather than to one
run.

That is a different guarantee from the one the LangGraph example shows, and the difference matters:

    lore.pack(run_id=second_run, query=topic)              # recall across runs
    lore.pack(run_id=same_run, query=topic, min_seq=seq)   # read-your-writes within one run

There is no ``min_seq`` below. ``min_seq`` asks "reflect the write I just made on THIS run", which is
not what is being asked here — the second run has made no writes at all. Watch its ``covered_seq``
come back as 0 while the recall still arrives: coverage describes the run you are packing, recall
does not.

The agents' reasoning is a deterministic stub, so this runs with no model key; swap ``OfflineLLM`` for
a real one and the Lore parts are unchanged.

Prerequisites: a running Lore (see the repo README quickstart) and its API key:

    export LORE_API_KEY=...     # from ./.lore/credentials
    uv run remember.py
"""

from __future__ import annotations

import os
import time
from typing import Any

# CrewAI's telemetry is on by default and posts to its own collector. An example someone runs to
# evaluate Lore should not quietly send anything anywhere, so it is turned off here — with setdefault,
# so anyone who does want it can still export these and be obeyed. This has to happen before the
# import below, which is why that import is not at the top of the file.
os.environ.setdefault("CREWAI_DISABLE_TELEMETRY", "true")
os.environ.setdefault("CREWAI_TRACING_ENABLED", "false")

from crewai import Agent, BaseLLM, Crew, Process, Task  # noqa: E402
from loregpt import LoreClient  # noqa: E402

TOPIC = "the auth service migration"

# What each agent "reasons". Keyed by role so the stub can answer as whichever agent is calling — the
# LLM receives from_agent, so this needs no global state and no prompt parsing.
STUB_RESPONSES = {
    "Scout": (
        "The auth service migrated to OAuth v2. The legacy token path is deprecated and callers "
        "still using it will start failing once it is removed."
    ),
    "Planner": (
        "Plan: audit callers for the deprecated token path first, then schedule its removal. "
        "This builds on what the earlier run already established, which is in the context above."
    ),
}


class OfflineLLM(BaseLLM):
    """A canned-response LLM, so the example runs with no model key and no network.

    Its answers depend only on which agent is asking, which keeps the run deterministic — the point
    of the example is the memory between runs, and a real model would make that harder to see, not
    easier. Replace it with any configured LLM and nothing else here changes.
    """

    def call(
        self,
        messages: Any,
        tools: Any = None,
        callbacks: Any = None,
        available_functions: Any = None,
        from_task: Any = None,
        from_agent: Any = None,
        response_model: Any = None,
    ) -> str:
        role = getattr(from_agent, "role", "")
        return STUB_RESPONSES.get(role, "No response for this role.")


def build_crew(prior: str, *, llm: BaseLLM | None = None) -> Crew:
    """Wire a two-agent crew that reports on a topic and plans what to do about it.

    ``prior`` is what a previous run learned, or "" on a first run. It is interpolated into the task
    description, so the crew simply reads it as context — nothing about the agents knows or cares
    that it came from another run.

    Note there is no Lore call in here. ``Task(callback=...)`` looks like the natural seam, but
    CrewAI warns that only a module-level named function can be a callback (anything else blocks
    checkpointing), and a module-level function cannot carry the run it should write to without
    module-level state. Writing after ``kickoff()`` instead keeps the crew a plain crew and puts the
    Lore calls where a reader can see them.
    """
    llm = llm or OfflineLLM(model="offline/stub")

    scout = Agent(
        role="Scout",
        goal="Establish the current state of {topic}",
        backstory="You find out what is actually true right now and state it plainly.",
        llm=llm,
    )
    planner = Agent(
        role="Planner",
        goal="Decide what to do next about {topic}",
        backstory="You turn findings into a short, ordered plan.",
        llm=llm,
    )

    brief = "Report the current state of {topic}."
    if prior:
        brief += f"\n\nFrom an earlier run:\n{prior}"

    investigate = Task(
        description=brief,
        expected_output="A short statement of the current state.",
        agent=scout,
    )
    plan = Task(
        description="Given the scout's report, say what to do next about {topic}.",
        expected_output="A short, ordered plan.",
        agent=planner,
        # CrewAI's own within-run handoff: the planner sees the scout's output directly. Lore is not
        # involved here and does not need to be — this part already works.
        context=[investigate],
    )

    return Crew(agents=[scout, planner], tasks=[investigate, plan], process=Process.sequential, verbose=False)


def wait_until_distilled(lore: LoreClient, run_id: str, seq: int, *, timeout_s: float = 30.0) -> int:
    """Block until extraction has distilled the run up to ``seq``, and report where it got to.

    This is a poll rather than a sleep on purpose. Consolidation is asynchronous, so "has it caught
    up" is a question with an answer — ``covered_seq`` — and guessing at a duration instead would be
    both slower and occasionally wrong. Only the SECOND run needs this: a distilled memory is what
    crosses a run boundary, whereas within a run ``min_seq`` would make the write visible immediately
    whether or not extraction had caught up.
    """
    deadline = time.monotonic() + timeout_s
    while True:
        covered = lore.pack(run_id=run_id, query=TOPIC).covered_seq
        if covered >= seq:
            return covered
        if time.monotonic() >= deadline:
            raise TimeoutError(
                f"extraction did not reach seq {seq} within {timeout_s:.0f}s (covered_seq={covered}); "
                "is the worker running?"
            )
        time.sleep(0.5)


def record(lore: LoreClient, run_id: str, result: Any) -> int:
    """Write each task's output to the shared run, and return the last seq written.

    The payload uses the ``memory`` key, which is what the extractor distils into a retrievable
    memory. A bare note would still be stored, and would still show up in this run's own pack as
    recent raw activity — but it would not survive into another run, which is the whole point here.
    """
    seq = 0
    for task_output in result.tasks_output:
        written = lore.write(
            run_id=run_id,
            agent_id=str(task_output.agent),
            payload={"memory": task_output.raw},
        )
        seq = written.seq
    return seq


def main() -> None:
    lore = LoreClient(api_key=os.environ["LORE_API_KEY"])

    # --- First run: the crew works, and what it establishes is written to the shared run. ---
    first = lore.create_run()
    print(f"run 1: {first.run_id}")
    first_result = build_crew(prior="").kickoff(inputs={"topic": TOPIC})
    last_seq = record(lore, first.run_id, first_result)

    covered = wait_until_distilled(lore, first.run_id, last_seq)
    print(f"run 1: distilled through seq {covered}")

    # --- Second run: a fresh run that has seen nothing, opening with what the first learned. ---
    second = lore.create_run()
    print(f"run 2: {second.run_id}")
    recalled = lore.pack(run_id=second.run_id, query=TOPIC)
    print(
        f"run 2: recalled {len(recalled.sources)} memories from earlier runs "
        f"(covered_seq={recalled.covered_seq} — this run has written nothing yet)"
    )

    result = build_crew(prior=recalled.text).kickoff(inputs={"topic": TOPIC})
    record(lore, second.run_id, result)
    print("\n--- second run's plan ---")
    print(result.raw)


if __name__ == "__main__":
    main()
