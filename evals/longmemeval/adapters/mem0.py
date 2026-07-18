"""The Mem0 adapter: drive Mem0 OSS as a memory system. Ingest each session as a Mem0 `add` (Mem0 runs its own
LLM fact-extraction on write), then retrieve with a `search` and return the recalled memories as a context
block. Answering is the shared harness step over that context — Mem0 supplies memory, not answers, exactly like
the Lore adapter's retrieve→shared-answerer split, so the two are compared under one answerer + one judge.

The adapter drives the in-process `mem0.Memory` class (embedded vector store, no server/Docker); the real
class is wired in the smoke script and imported lazily. Tests inject a `Mem0Like` fake, so the flow — session→
add mapping, per-ingest isolation, search→sorted-context — is verified with no key, no vector store, no
network."""

from __future__ import annotations

from collections.abc import Sequence
from typing import Any, Protocol

from .._types import Session
from .base import MemorySystem

# The Mem0 config the run used is recorded in the report (the fairness template): a score is only defensible
# next to the version and configuration that produced it. This label names OUR harness's choice; the smoke
# script records the mem0ai version alongside it.
DEFAULT_CONFIG_LABEL = "oss-default"


class Mem0Like(Protocol):
    """The slice of the Mem0 client the adapter uses (mem0ai 2.x). `add` takes `user_id` as a keyword arg (NOT
    `timestamp` — that is a Platform-only temporal feature the OSS `Memory` rejects); `search` takes
    `filters={"user_id": ...}` + `top_k` (the 2.x shape — the pre-v3 positional/`limit=` form is gone). Both
    return {"results": [...]}."""

    def add(self, messages: list[dict[str, str]], *, user_id: str) -> dict[str, Any]: ...

    def search(self, query: str, *, filters: dict[str, Any], top_k: int = ...) -> dict[str, Any]: ...


class Mem0Adapter(MemorySystem):
    def __init__(self, client: Mem0Like, *, top_k: int = 20, config_label: str = DEFAULT_CONFIG_LABEL) -> None:
        self._client = client
        self.top_k = top_k  # retrieval breadth — recorded in the fairness record
        self.config_label = config_label
        self._ingest_count = 0
        self._user_id: str | None = None

    @property
    def name(self) -> str:
        return "mem0"

    def ingest(self, sessions: Sequence[Session]) -> None:
        # A fresh user_id per ingest isolates this history's memory from any prior ingestion (the pipeline-
        # variance protocol re-ingests the same question repeatedly — each pass must start clean).
        self._ingest_count += 1
        self._user_id = f"lme-{self._ingest_count}"
        for session in sessions:
            # One add per session, in dataset order (the same order the Lore adapter ingests in — keeping the
            # two adapters' ingestion order identical keeps the comparison a parity comparison). Mem0 extracts
            # facts from the message list; the session date rides IN the message text (prefix) so a temporal
            # question can reason over it. We do NOT pass a `timestamp` kwarg — that is a Platform-only feature
            # the OSS Memory rejects, and the in-text date carries the same information.
            messages: list[dict[str, str]] = []
            prefix = f"[{session.date}] " if session.date else ""
            for turn in session.turns:
                if not turn.content.strip():
                    continue
                messages.append({"role": turn.role, "content": f"{prefix}{turn.content}"})
            if not messages:
                continue
            self._client.add(messages, user_id=self._user_id)

    def retrieve(self, question: str, question_date: str) -> str:
        if self._user_id is None:
            raise RuntimeError("retrieve() called before ingest()")
        response = self._client.search(question, filters={"user_id": self._user_id}, top_k=self.top_k)
        results = response.get("results", [])
        # Present recalled memories oldest-first so the answerer reads them in conversational time order.
        ordered = sorted(results, key=lambda r: str(r.get("created_at", "")))
        return "\n".join(str(r.get("memory", "")) for r in ordered if r.get("memory"))
