"""The memory-system-under-test interface. Every system the harness scores — Lore now, Mem0/Zep later —
implements this one ABC, so adding a competitor is plugging in an adapter, never a harness change."""

from __future__ import annotations

from abc import ABC, abstractmethod
from collections.abc import Sequence

from .._types import Session


class MemorySystem(ABC):
    """A memory system under evaluation: ingest a conversation history, then answer a question from memory."""

    @property
    @abstractmethod
    def name(self) -> str:
        """Stable identifier used in reports, e.g. "lore"."""

    @abstractmethod
    def ingest(self, sessions: Sequence[Session]) -> None:
        """Ingest a timestamped multi-session conversation history into the system's memory. Blocks until the
        history is durably stored AND retrievable (each adapter is responsible for any async-distillation
        wait, so `answer` sees a settled memory)."""

    @abstractmethod
    def answer(self, question: str, question_date: str) -> str:
        """Recall relevant memory for the question and produce a natural-language answer. `question_date` is the
        wall-clock the question is asked at (some question types are temporal)."""
