from pathlib import Path

import pytest

from longmemeval.loader import DATASET_REVISION, deterministic_subset, load_questions
from longmemeval.subsets import (
    DEV_QUOTAS,
    GATE_QUOTAS,
    SUBSET_NAMES,
    Subset,
    build_subsets,
    load_subset,
    subset_document,
)

# The real split is downloaded on demand into a gitignored cache, so the tests that need it run where it exists and
# are skipped in a keyless CI checkout. Everything else here checks the committed files on their own.
DATASET = Path(__file__).resolve().parent.parent / "judge_cache" / "hf" / "longmemeval_s_cleaned.json"
needs_dataset = pytest.mark.skipif(not DATASET.exists(), reason="the LongMemEval-S split is not cached locally")


def _all() -> dict[str, Subset]:
    return {name: load_subset(name) for name in SUBSET_NAMES}


def test_committed_subsets_are_disjoint_and_cover_the_split() -> None:
    subsets = _all()
    ids = [qid for s in subsets.values() for qid in s.ids]
    assert len(ids) == len(set(ids)), "a question in two subsets would let development touch a verdict set"
    assert len(ids) == 500


def test_committed_subsets_name_the_pinned_revision_and_split() -> None:
    for subset in _all().values():
        assert subset.split == "s"
        assert subset.dataset_revision == DATASET_REVISION


def test_verdict_sets_share_one_composition_and_dev_has_its_own() -> None:
    subsets = _all()
    assert subsets["gate-v1"].composition() == dict(GATE_QUOTAS)
    assert subsets["gate-v2"].composition() == dict(GATE_QUOTAS)
    assert subsets["dev"].composition() == dict(DEV_QUOTAS)
    assert sum(GATE_QUOTAS.values()) == 50 and sum(DEV_QUOTAS.values()) == 70


def test_only_gate_subsets_are_sealed() -> None:
    sealed = {name for name, s in _all().items() if s.sealed}
    assert sealed == {"gate-v1", "gate-v2"}


def test_digest_follows_the_questions_not_the_name() -> None:
    a = Subset("dev", "s", "r", (("q1", "t"), ("q2", "t")))
    reordered = Subset("dev", "s", "r", (("q2", "t"), ("q1", "t")))
    different = Subset("dev", "s", "r", (("q1", "t"), ("q3", "t")))
    assert a.digest == reordered.digest, "the digest names a set of questions, not an ordering"
    assert a.digest != different.digest, "same name, different questions must not look like the same subset"
    assert a.key == f"dev@{a.digest}"


def test_unknown_subset_is_named() -> None:
    with pytest.raises(ValueError, match="unknown subset"):
        load_subset("nope")


@needs_dataset
def test_gate_v1_is_exactly_the_old_fifty() -> None:
    """Every earlier n=50 run measured these questions; keeping them as gate-v1 is what keeps those runs
    reproducible."""
    questions = load_questions(DATASET)
    assert list(load_subset("gate-v1").ids) == [q.question_id for q in deterministic_subset(questions, 50)]


@needs_dataset
def test_committed_files_are_what_the_rule_produces() -> None:
    questions = load_questions(DATASET)
    built = build_subsets(questions)
    for name in SUBSET_NAMES:
        committed = (Path(__file__).resolve().parent.parent / "subsets" / f"{name}.json").read_text("utf-8")
        assert committed == subset_document(name, built[name], "s", DATASET_REVISION), name
