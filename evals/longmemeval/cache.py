"""Judge-decision cache. Keyed on (question_id, answer_hash, judge_model, rubric_version): an unchanged answer
under an unchanged judge and rubric is never re-judged, so re-runs are cheap and the judge call is spent only
on genuinely new (answer, rubric) pairs."""

from __future__ import annotations

import hashlib
import json
from dataclasses import asdict, dataclass
from pathlib import Path


def hash_answer(answer: str) -> str:
    """Stable content hash of an answer — the cache's identity for "the same answer"."""
    return hashlib.sha256(answer.encode("utf-8")).hexdigest()


@dataclass(frozen=True, slots=True)
class JudgeDecision:
    """A cached judge verdict: the binary correctness plus the judge identity that produced it."""

    correct: bool
    reasoning: str
    judge_model: str
    rubric_version: str


@dataclass(frozen=True, slots=True)
class CacheKey:
    """The full identity of a judge decision. A new answer, a new judge model, or a bumped rubric version each
    forces a fresh judgement. `variant` distinguishes otherwise-identical judgements that must NOT share a
    cache entry — a variance trial folds its trial index in here, so trial 2 re-judges the same answer rather
    than being served trial 1's decision (a variance measurement must actually sample N times). It stays "" for
    an ordinary single pass, where re-judging an unchanged answer is pure waste."""

    question_id: str
    answer_hash: str
    judge_model: str
    rubric_version: str
    variant: str = ""


class JudgeCache:
    """A file-backed judge cache. Each decision is a small, browsable JSON file so a run can be audited."""

    def __init__(self, root: Path) -> None:
        self._root = root
        self._root.mkdir(parents=True, exist_ok=True)
        self._hits = 0
        self._misses = 0

    def _path(self, key: CacheKey) -> Path:
        safe_model = key.judge_model.replace("/", "_").replace(":", "_")
        variant = f".{key.variant}" if key.variant else ""
        name = f"{key.question_id}.{key.answer_hash[:16]}.{safe_model}.{key.rubric_version}{variant}.json"
        return self._root / name

    def get(self, key: CacheKey) -> JudgeDecision | None:
        path = self._path(key)
        if not path.exists():
            self._misses += 1
            return None
        self._hits += 1
        data = json.loads(path.read_text("utf-8"))
        return JudgeDecision(
            correct=bool(data["correct"]),
            reasoning=str(data["reasoning"]),
            judge_model=str(data["judge_model"]),
            rubric_version=str(data["rubric_version"]),
        )

    def put(self, key: CacheKey, decision: JudgeDecision) -> None:
        self._path(key).write_text(json.dumps(asdict(decision), indent=2, sort_keys=True), "utf-8")

    @property
    def hits(self) -> int:
        return self._hits

    @property
    def misses(self) -> int:
        return self._misses

    @property
    def hit_rate(self) -> float:
        total = self._hits + self._misses
        return self._hits / total if total else 0.0
