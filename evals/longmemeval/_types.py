"""Core value types for the LongMemEval harness. Frozen dataclasses; the dataset shapes mirror the official
LongMemEval JSON, and the result/provenance shapes carry the same-judge reproducibility chain."""

from __future__ import annotations

from dataclasses import dataclass


@dataclass(frozen=True, slots=True)
class Turn:
    """One message in a session."""

    role: str  # "user" | "assistant"
    content: str
    has_answer: bool = False  # official evidence marker (true on turns that contain the answer)


@dataclass(frozen=True, slots=True)
class Session:
    """One timestamped conversation session (a list of turns)."""

    session_id: str
    date: str  # the session's timestamp, as the dataset records it
    turns: tuple[Turn, ...]


@dataclass(frozen=True, slots=True)
class Question:
    """One LongMemEval instance: a question over a haystack of sessions, with a gold answer."""

    question_id: str
    question_type: str
    question: str
    answer: str
    question_date: str
    sessions: tuple[Session, ...]

    @property
    def is_abstention(self) -> bool:
        """Abstention (false-premise) instances have a question_id suffixed `_abs`; the gold behaviour is to
        decline to answer."""
        return self.question_id.endswith("_abs")


@dataclass(frozen=True, slots=True)
class Verdict:
    """One judged answer: the official judge's binary correctness plus the provenance to reproduce it."""

    question_id: str
    question_type: str
    generated_answer: str
    gold_answer: str
    correct: bool
    judge_model: str
    rubric_version: str


@dataclass(frozen=True, slots=True)
class Provenance:
    """The reproducibility chain stamped onto every run report — so a score can be re-derived, and two runs are
    comparable only when every field matches (the same-judge discipline made explicit)."""

    dataset: str  # HuggingFace repo id
    dataset_revision: str  # pinned commit/revision
    split: str
    n: int
    judge_model: str
    judge_prompt_hash: str  # hash of the verbatim official judge prompts
    answerer_model: str
    extraction_model: str
    generated_at: str  # stamped by the caller (the library never reads the clock)
    # The system-under-test's ingestion mode and configuration — the fairness record. A score is only
    # defensible next to how the system was driven: for Lore, whether ingestion ran realtime or the economy
    # (batched) extraction mode; for a competitor, its package version + the configuration label the run used
    # (default vs tuned). Empty when not applicable.
    extraction_mode: str = ""
    system_config: str = ""
    # The embedding model that produced the retrieval vector space, completing the provenance chain
    # (extraction + answerer + judge + embedding). For Lore it is the composed embedder's model@dim identity
    # (e.g. "text-embedding-3-small@1536" or the offline "fixture-embed-v1@64"); empty when not applicable.
    embedding_model: str = ""
