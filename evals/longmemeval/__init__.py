"""LongMemEval harness: score memory systems through one pinned, cached judge with the same-judge discipline.

An adapter is memory-only (ingest + retrieve); the shared answerer and the shared judge are applied identically
to every system, so a cross-system score is a parity comparison. Full runs go through the Batch API; results
are reported under two labelled variance protocols (a gated answer-variance and a non-gated pipeline-variance).
"""

from ._types import Provenance, Question, Session, Turn, Verdict
from .adapters import (
    DistillationTimeout,
    LoreAdapter,
    LoreLike,
    Mem0Adapter,
    Mem0Like,
    MemorySystem,
)
from .answerer import (
    ANSWER_SYSTEM,
    DEFAULT_ANSWERER_MODEL,
    Answerer,
    anthropic_answerer,
    build_answer_prompt,
)
from .batch import (
    AnthropicBatchProvider,
    BatchError,
    BatchOutcome,
    BatchProvider,
    BatchRequest,
    BatchStatus,
    OpenAIBatchProvider,
    ResumeStore,
    run_batch,
)
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
from .report import (
    VARIANCE_ANSWER,
    VARIANCE_PIPELINE,
    Leaderboard,
    MeanStd,
    SystemReport,
    VarianceResult,
)
from .runner import (
    run_trial,
    run_trial_batched,
    run_variance_pipeline,
    run_variance_pipeline_batched,
    run_variance_reuse_ingest,
    run_variance_reuse_ingest_batched,
)
from .stats import RunStats, estimate_tokens

__version__ = "0.1.0"

__all__ = [
    "ANSWER_SYSTEM",
    "DATASET_REPO",
    "DATASET_REVISION",
    "DEFAULT_ANSWERER_MODEL",
    "DEFAULT_JUDGE_MODEL",
    "PROMPT_HASH",
    "RUBRIC_VERSION",
    "VARIANCE_ANSWER",
    "VARIANCE_PIPELINE",
    "Answerer",
    "AnthropicBatchProvider",
    "BatchError",
    "BatchOutcome",
    "BatchProvider",
    "BatchRequest",
    "BatchStatus",
    "CacheKey",
    "DistillationTimeout",
    "Judge",
    "JudgeCache",
    "JudgeDecision",
    "Leaderboard",
    "LoreAdapter",
    "LoreLike",
    "MeanStd",
    "Mem0Adapter",
    "Mem0Like",
    "MemorySystem",
    "OpenAIBatchProvider",
    "Provenance",
    "Question",
    "ResumeStore",
    "RunStats",
    "Session",
    "SystemReport",
    "Turn",
    "VarianceResult",
    "Verdict",
    "__version__",
    "anthropic_answerer",
    "build_answer_prompt",
    "build_grading_prompt",
    "deterministic_subset",
    "download_split",
    "estimate_tokens",
    "hash_answer",
    "load_questions",
    "openai_judge",
    "parse_question",
    "run_batch",
    "run_trial",
    "run_trial_batched",
    "run_variance_pipeline",
    "run_variance_pipeline_batched",
    "run_variance_reuse_ingest",
    "run_variance_reuse_ingest_batched",
]
