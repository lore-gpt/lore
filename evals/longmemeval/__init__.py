"""LongMemEval harness: score memory systems through one pinned, cached judge with the same-judge discipline."""

from ._types import Provenance, Question, Session, Turn, Verdict
from .adapters import MemorySystem
from .adapters.lore import DistillationTimeout, LoreAdapter, LoreLike
from .answerer import DEFAULT_ANSWERER_MODEL, Answerer, anthropic_answerer
from .cache import CacheKey, JudgeCache, JudgeDecision, hash_answer
from .judge import (
    DEFAULT_JUDGE_MODEL,
    PROMPT_HASH,
    RUBRIC_VERSION,
    Judge,
    build_grading_prompt,
    openai_judge,
)
from .loader import (
    DATASET_REPO,
    DATASET_REVISION,
    deterministic_subset,
    download_split,
    load_questions,
    parse_question,
)
from .report import SystemSummary, TrialReport, aggregate
from .runner import run_trial

__version__ = "0.1.0"

__all__ = [
    "DATASET_REPO",
    "DATASET_REVISION",
    "DEFAULT_ANSWERER_MODEL",
    "DEFAULT_JUDGE_MODEL",
    "PROMPT_HASH",
    "RUBRIC_VERSION",
    "Answerer",
    "CacheKey",
    "DistillationTimeout",
    "Judge",
    "JudgeCache",
    "JudgeDecision",
    "LoreAdapter",
    "LoreLike",
    "MemorySystem",
    "Provenance",
    "Question",
    "Session",
    "SystemSummary",
    "TrialReport",
    "Turn",
    "Verdict",
    "__version__",
    "aggregate",
    "anthropic_answerer",
    "build_grading_prompt",
    "deterministic_subset",
    "download_split",
    "hash_answer",
    "load_questions",
    "openai_judge",
    "parse_question",
    "run_trial",
]
