"""The memory-system-under-test interface. Every system the harness scores — Lore, Mem0, later Graphiti —
implements this one ABC, so adding a competitor is plugging in an adapter, never a harness change.

An adapter's job is MEMORY only: ingest a conversation, then retrieve the relevant context for a question. It
does NOT answer. Answering (and judging) are shared harness steps applied identically to every system's
retrieved context — that shared answerer is what makes a cross-system comparison a parity comparison rather
than a comparison of each vendor's bundled answer model."""

from __future__ import annotations

from abc import ABC, abstractmethod
from collections.abc import Sequence

from .._types import Session


class MemorySystem(ABC):
    """A memory system under evaluation: ingest a conversation history, then retrieve context for a question."""

    @property
    @abstractmethod
    def name(self) -> str:
        """Stable identifier used in reports, e.g. "lore"."""

    @abstractmethod
    def ingest(self, sessions: Sequence[Session]) -> None:
        """Ingest a timestamped multi-session conversation history into a FRESH, isolated memory scope (a new
        run/user per call, so re-ingesting the same history — as the pipeline-variance protocol does — never
        contaminates a prior ingestion). Blocks until the history is durably stored AND retrievable (each
        adapter owns any async-distillation wait, so `retrieve` sees a settled memory)."""

    @abstractmethod
    def retrieve(self, question: str, question_date: str) -> str:
        """Recall the memory relevant to the question and return it as a plain-text context block (no answer
        generation — the shared answerer turns this context into an answer). `question_date` is the wall-clock
        the question is asked at (some question types are temporal)."""
