"""Named, disjoint question subsets of the LongMemEval-S split.

Why named subsets instead of "the first n". Choosing questions by count alone ties every measurement to one list:
the n=50 set was the only n=50 set there could be, so a later measurement could not use fresh questions, and any
tuning done against it was tuning against the very questions that would grade it. Named subsets divide the split
into disjoint parts with distinct jobs:

- ``gate-v1``  the 50 questions every earlier n=50 measurement used, kept so those runs stay reproducible.
- ``dev``      where iteration happens: measure, change, measure again, as often as needed.
- ``gate-v2``  read once, for a verdict, after development is finished.
- ``holdout``  everything else, untouched.

Selection is mechanical, so nobody chooses questions. Within each question type, questions are taken in
``question_id`` order — the ids are hashes, so that order is effectively random yet reproducible — with per-type
counts fixed in advance below. The resulting lists are committed as files, so a subset names the same questions on
every machine even if this code changes later; :func:`build_subsets` regenerates them from the dataset and a test
checks that the two agree.

Sealed subsets. A subset whose name starts with ``gate-`` is sealed: a real run refuses it unless the caller says,
explicitly, that this is the read (see ``guards.subset_seal_blocker``). Development tooling looks at gold answers
question by question; a sealed subset must never be looked at that way before its verdict.
"""

from __future__ import annotations

import hashlib
import json
from collections import Counter, defaultdict
from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from pathlib import Path

from ._types import Question

SUBSETS_DIR = Path(__file__).resolve().parent.parent / "subsets"
SEALED_PREFIX = "gate-"
SUBSET_NAMES = ("gate-v1", "dev", "gate-v2", "holdout")

# Per-type counts for a verdict set: the split's own type proportions at 50 questions, which is exactly the
# composition of gate-v1 (round(50 * type_count / 500) per type, summing to 50 without adjustment). gate-v2 uses the
# same counts, so its number is comparable to gate-v1's.
GATE_QUOTAS: Mapping[str, int] = {
    "knowledge-update": 8,
    "multi-session": 13,
    "single-session-assistant": 6,
    "single-session-preference": 3,
    "single-session-user": 7,
    "temporal-reasoning": 13,
}

# Per-type counts for the development set: the split's proportions at 70 questions, with the two smallest types
# raised (single-session-assistant 8 -> 9, single-session-preference 4 -> 6) and the two largest trimmed to pay for
# it. A type measured on three or four questions moves in steps of a quarter or more, which is too coarse to tell
# whether a change helped it.
DEV_QUOTAS: Mapping[str, int] = {
    "knowledge-update": 11,
    "multi-session": 17,
    "single-session-assistant": 9,
    "single-session-preference": 6,
    "single-session-user": 10,
    "temporal-reasoning": 17,
}


@dataclass(frozen=True, slots=True)
class Subset:
    """A named list of questions from one split at one dataset revision."""

    name: str
    split: str
    dataset_revision: str
    # (question_id, question_type), sorted by question_id. The type is stored so a subset's composition can be
    # checked without the dataset at hand.
    questions: tuple[tuple[str, str], ...]

    @property
    def ids(self) -> tuple[str, ...]:
        return tuple(qid for qid, _ in self.questions)

    @property
    def digest(self) -> str:
        """The first 16 hex chars of the sha256 of the sorted id list. Two subsets with the same name but different
        questions have different digests, which is what keeps a reference from one applying to the other."""
        return hashlib.sha256("\n".join(sorted(self.ids)).encode("utf-8")).hexdigest()[:16]

    @property
    def key(self) -> str:
        """``name@digest`` — the identity recorded in a report's provenance and in the measurement universe."""
        return f"{self.name}@{self.digest}"

    @property
    def sealed(self) -> bool:
        return self.name.startswith(SEALED_PREFIX)

    def composition(self) -> dict[str, int]:
        return dict(Counter(qtype for _, qtype in self.questions))


def load_subset(name: str, directory: Path = SUBSETS_DIR) -> Subset:
    """Load a committed subset by name."""
    path = directory / f"{name}.json"
    if not path.exists():
        raise ValueError(f"unknown subset {name!r}: no {path}")
    raw = json.loads(path.read_text("utf-8"))
    return Subset(
        name=str(raw["name"]),
        split=str(raw["split"]),
        dataset_revision=str(raw["dataset_revision"]),
        questions=tuple((str(q["question_id"]), str(q["question_type"])) for q in raw["questions"]),
    )


def select(questions: Sequence[Question], subset: Subset) -> list[Question]:
    """The questions a subset names, in its (question_id) order. Fails loudly if any is missing, which means the
    subset was built from a different split or revision than the one loaded."""
    by_id = {q.question_id: q for q in questions}
    missing = [qid for qid in subset.ids if qid not in by_id]
    if missing:
        raise ValueError(
            f"subset {subset.name!r} names {len(missing)} question(s) not in the loaded split "
            f"(e.g. {missing[0]!r}) — it was built for {subset.split}@{subset.dataset_revision}"
        )
    return [by_id[qid] for qid in subset.ids]


def take_by_type(pool: Sequence[Question], quotas: Mapping[str, int]) -> list[Question]:
    """Take ``quotas[type]`` questions of each type from ``pool``, in question_id order within the type."""
    by_type: dict[str, list[Question]] = defaultdict(list)
    for q in pool:
        by_type[q.question_type].append(q)
    picked: list[Question] = []
    for qtype, count in quotas.items():
        group = sorted(by_type.get(qtype, []), key=lambda q: q.question_id)
        if len(group) < count:
            raise ValueError(f"only {len(group)} {qtype!r} questions left; {count} are needed")
        picked.extend(group[:count])
    return sorted(picked, key=lambda q: q.question_id)


def build_subsets(questions: Sequence[Question]) -> dict[str, list[Question]]:
    """Divide the split into the named subsets. Each draws only from what the earlier ones left, so the four are
    disjoint and together cover every question exactly once."""
    gate_v1 = take_by_type(questions, GATE_QUOTAS)
    used = {q.question_id for q in gate_v1}
    dev = take_by_type([q for q in questions if q.question_id not in used], DEV_QUOTAS)
    used |= {q.question_id for q in dev}
    gate_v2 = take_by_type([q for q in questions if q.question_id not in used], GATE_QUOTAS)
    used |= {q.question_id for q in gate_v2}
    holdout = sorted((q for q in questions if q.question_id not in used), key=lambda q: q.question_id)
    return {"gate-v1": gate_v1, "dev": dev, "gate-v2": gate_v2, "holdout": holdout}


def subset_document(name: str, questions: Sequence[Question], split: str, dataset_revision: str) -> str:
    """The committed JSON form of a subset: ids and types only — never question text or answers."""
    doc = {
        "name": name,
        "split": split,
        "dataset_revision": dataset_revision,
        "composition": dict(sorted(Counter(q.question_type for q in questions).items())),
        "questions": [{"question_id": q.question_id, "question_type": q.question_type} for q in questions],
    }
    return json.dumps(doc, indent=2) + "\n"
