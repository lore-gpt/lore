"""Two LangGraph agents coordinating through Lore's shared memory.

A *researcher* writes what it found to a shared run; a *writer* then picks up exactly where the
researcher left off by packing the run with ``min_seq`` set to the researcher's write. Lore's
read-your-writes guarantee means the writer is *guaranteed* to see that write — as a distilled memory,
or as a raw tail until extraction catches up — with no polling, no shared globals, no "did it land yet?".

The agents' reasoning here is a deterministic stub, so the example needs no model key; swap the node
bodies for your LLM calls. The Lore coordination is the point.

Prerequisites: a running Lore (see the repo README quickstart) and its API key:

    export LORE_API_KEY=...     # from ./.lore/credentials
    python coordinate.py
"""

from __future__ import annotations

import os
from typing import TypedDict

from langgraph.graph import END, START, StateGraph
from loregpt import LoreClient


class TeamState(TypedDict):
    run_id: str
    topic: str
    handoff_seq: int  # the seq the researcher wrote — the writer reads from here
    brief: str


def build_team(lore: LoreClient):
    """Wire a two-node LangGraph whose nodes hand off through Lore, not through shared state."""

    def researcher(state: TeamState) -> dict[str, int]:
        # A real agent would reason here; what matters is that it WRITES its finding to the shared
        # run and keeps the seq the write returns.
        finding = f"{state['topic']}: migrated to v2; the legacy path is deprecated."
        written = lore.write(run_id=state["run_id"], agent_id="researcher", content=finding)
        return {"handoff_seq": written.seq}

    def writer(state: TeamState) -> dict[str, str]:
        # Passing min_seq = the researcher's seq makes this pack reflect that write, guaranteed.
        # covered_seq reports how far the distilled view has advanced.
        pack = lore.pack(
            run_id=state["run_id"],
            query=f"latest on {state['topic']}",
            min_seq=state["handoff_seq"],
        )
        brief = f"Writer draft, built on the shared run (covered_seq={pack.covered_seq}):\n\n{pack.text}"
        return {"brief": brief}

    graph = StateGraph(TeamState)
    graph.add_node("researcher", researcher)
    graph.add_node("writer", writer)
    graph.add_edge(START, "researcher")
    graph.add_edge("researcher", "writer")
    graph.add_edge("writer", END)
    return graph.compile()


def main() -> None:
    lore = LoreClient(api_key=os.environ["LORE_API_KEY"])
    run = lore.create_run()
    team = build_team(lore)
    final = team.invoke({"run_id": run.run_id, "topic": "auth flow", "handoff_seq": 0, "brief": ""})
    print(final["brief"])


if __name__ == "__main__":
    main()
