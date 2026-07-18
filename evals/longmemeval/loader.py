"""Load LongMemEval questions. Two sources feed the same parser: a tiny clean-room fixture committed to the
repo (for keyless unit tests), and the real dataset fetched from HuggingFace at a pinned revision (for the
secret-gated smoke/full runs — never committed, cached locally under a gitignored directory)."""

from __future__ import annotations

import json
from collections import defaultdict
from collections.abc import Mapping, Sequence
from pathlib import Path
from typing import Any

from ._types import Question, Session, Turn

# The dataset is fetched at runtime, never vendored into the repo. Pin the revision so a score is reproducible;
# the report records exactly this repo+revision. NOTE: revision is "main" as a placeholder — pin it to a commit
# SHA before the first real run (a slice gap, surfaced, not silently shipped).
DATASET_REPO = "xiaowu0162/longmemeval-cleaned"
DATASET_REVISION = "main"
SPLIT_FILES = {
    "s": "longmemeval_s_cleaned.json",
    "m": "longmemeval_m_cleaned.json",
    "oracle": "longmemeval_oracle.json",
}


def parse_question(raw: Mapping[str, Any]) -> Question:
    """Parse one raw LongMemEval instance into a Question. Tolerant of a missing session_ids/dates array (some
    variants omit them) by synthesising positional ids/empty dates."""
    haystack = raw["haystack_sessions"]
    session_ids = raw.get("haystack_session_ids") or [f"session_{i}" for i in range(len(haystack))]
    dates = raw.get("haystack_dates") or ["" for _ in haystack]
    sessions = tuple(
        Session(
            session_id=str(sid),
            date=str(date),
            turns=tuple(
                Turn(role=str(t["role"]), content=str(t["content"]), has_answer=bool(t.get("has_answer", False)))
                for t in turns_raw
            ),
        )
        for sid, date, turns_raw in zip(session_ids, dates, haystack, strict=True)
    )
    return Question(
        question_id=str(raw["question_id"]),
        question_type=str(raw["question_type"]),
        question=str(raw["question"]),
        answer=str(raw.get("answer", "")),
        question_date=str(raw.get("question_date", "")),
        sessions=sessions,
    )


def load_questions(path: Path) -> list[Question]:
    """Load and parse a LongMemEval JSON file (a top-level list of instances)."""
    raw = json.loads(path.read_text("utf-8"))
    if not isinstance(raw, list):
        raise ValueError(f"{path}: expected a top-level JSON list of questions")
    return [parse_question(item) for item in raw]


def download_split(split: str, cache_dir: Path, revision: str = DATASET_REVISION) -> Path:
    """Download a dataset split from HuggingFace at the pinned revision into a local (gitignored) cache and
    return the file path. Lazy-imports huggingface_hub so the module loads without the optional dependency."""
    if split not in SPLIT_FILES:
        raise ValueError(f"unknown split {split!r}; expected one of {sorted(SPLIT_FILES)}")
    from huggingface_hub import hf_hub_download  # lazy: only the real-run path needs it

    cache_dir.mkdir(parents=True, exist_ok=True)
    local = hf_hub_download(
        repo_id=DATASET_REPO,
        filename=SPLIT_FILES[split],
        revision=revision,
        repo_type="dataset",
        local_dir=str(cache_dir),
    )
    return Path(local)


def deterministic_subset(questions: Sequence[Question], n: int) -> list[Question]:
    """A stable n-question subset, stratified by question_type and sorted by question_id within each type, so
    the same n questions are chosen on every machine and every run (the smoke number never jitters)."""
    if n >= len(questions):
        return sorted(questions, key=lambda q: q.question_id)

    by_type: dict[str, list[Question]] = defaultdict(list)
    for q in questions:
        by_type[q.question_type].append(q)
    for qs in by_type.values():
        qs.sort(key=lambda q: q.question_id)

    total = len(questions)
    picked: list[Question] = []
    for qtype in sorted(by_type):
        group = by_type[qtype]
        take = round(n * len(group) / total)
        picked.extend(group[:take])

    # Rounding can over/undershoot n; trim or top up deterministically from the id-sorted remainder.
    picked.sort(key=lambda q: q.question_id)
    if len(picked) > n:
        return picked[:n]
    if len(picked) < n:
        chosen = {q.question_id for q in picked}
        remainder = sorted((q for q in questions if q.question_id not in chosen), key=lambda q: q.question_id)
        picked.extend(remainder[: n - len(picked)])
        picked.sort(key=lambda q: q.question_id)
    return picked
