import json
from pathlib import Path

from longmemeval._types import Provenance, Verdict
from longmemeval.report import (
    VARIANCE_ANSWER,
    VARIANCE_PIPELINE,
    Leaderboard,
    MeanStd,
    SystemReport,
    VarianceResult,
)
from longmemeval.stats import RunStats


def _provenance(n: int, **over: str) -> Provenance:
    base = dict(
        dataset="xiaowu0162/longmemeval-cleaned",
        dataset_revision="abc123",
        split="s",
        n=n,
        judge_model="gpt-4o-2024-08-06",
        judge_prompt_hash="deadbeef" * 8,
        answerer_model="claude-haiku-4-5",
        extraction_model="claude-haiku-4-5",
        generated_at="2031-01-01T00:00:00Z",
    )
    return Provenance(**{**base, **over})  # type: ignore[arg-type]


def _v(qid: str, qtype: str, correct: bool) -> Verdict:
    return Verdict(
        question_id=qid,
        question_type=qtype,
        generated_answer="a",
        gold_answer="g",
        correct=correct,
        judge_model="gpt-4o-2024-08-06",
        rubric_version="longmemeval-official-v1",
    )


def test_mean_std_single_and_multi_trial() -> None:
    assert MeanStd.of([0.9], n=3) == MeanStd(mean=0.9, std=0.0, n=3, trials=1)  # single trial -> std 0
    ms = MeanStd.of([1.0, 0.5], n=2)
    assert ms.mean == 0.75
    assert ms.std > 0.0
    assert ms.trials == 2


def _variance(label: str) -> VarianceResult:
    # The _abs item is correct=False in both trials while the recall items are (mostly) correct — so an
    # overall that WRONGLY folded abstention in would read lower than one that (correctly) excludes it, making
    # the two behaviours distinguishable.
    trial1 = (
        _v("q1", "multi-session", True),
        _v("q2", "temporal-reasoning", True),
        _v("q3_abs", "multi-session", False),
    )
    trial2 = (
        _v("q1", "multi-session", True),
        _v("q2", "temporal-reasoning", False),
        _v("q3_abs", "multi-session", False),
    )
    return VarianceResult(label=label, trials=(trial1, trial2))


def test_abstention_is_excluded_from_overall_and_by_type() -> None:
    v = _variance(VARIANCE_ANSWER)
    overall = v.overall()
    # Recall only: trial1 = q1,q2 = 2/2 = 1.0; trial2 = q1 = 1/2 = 0.5 -> mean 0.75, n=2.
    # (Folding the _abs items in would give (2/3 + 1/3)/2 = 0.5, n=3 — the bug this pins out.)
    assert overall.mean == 0.75
    assert overall.n == 2
    assert overall.trials == 2
    by_type = v.by_type()
    assert "multi-session" in by_type
    assert by_type["multi-session"].mean == 1.0  # only q1 (the _abs item, though typed multi-session, is dropped)
    assert by_type["multi-session"].n == 1
    assert by_type["temporal-reasoning"].mean == 0.5
    assert by_type["temporal-reasoning"].std > 0.0
    # Abstention is its own line and carries the _abs item alone.
    abst = v.abstention()
    assert abst.n == 1
    assert abst.mean == 0.0


def test_gate_passes_when_answer_variance_std_under_target() -> None:
    # Two identical trials -> std 0 -> passes.
    steady = VarianceResult(VARIANCE_ANSWER, ((_v("q1", "t", True),), (_v("q1", "t", True),)))
    report = SystemReport("lore", _provenance(1), steady, cache_hit_rate=0.0, stats=RunStats())
    assert report.passes_gate is True
    # A trial set that swings 100%->0% has std of 0.707 accuracy = 70 pts -> fails the <2pt gate.
    swingy = VarianceResult(VARIANCE_ANSWER, ((_v("q1", "t", True),), (_v("q1", "t", False),)))
    assert SystemReport("lore", _provenance(1), swingy, cache_hit_rate=0.0, stats=RunStats()).passes_gate is False


def test_artifact_embeds_provenance_and_both_protocols(tmp_path: Path) -> None:
    answer = _variance(VARIANCE_ANSWER)
    pipeline = VarianceResult(VARIANCE_PIPELINE, ((_v("q1", "t", True),), (_v("q1", "t", False),)))
    prov = _provenance(3, extraction_mode="economy", system_config="mem0ai 2.0.12 (oss-default)")
    report = SystemReport("mem0", prov, answer, cache_hit_rate=0.5, stats=RunStats(), variance_pipeline=pipeline)
    out = tmp_path / "reports" / "run.json"
    report.write(out)
    data = json.loads(out.read_text("utf-8"))
    assert data["provenance"]["dataset_revision"] == "abc123"
    assert data["provenance"]["extraction_mode"] == "economy"
    assert data["provenance"]["system_config"] == "mem0ai 2.0.12 (oss-default)"
    # Both protocols are present and labelled, never conflated.
    assert data["variance_answer"]["label"] == VARIANCE_ANSWER
    assert data["variance_pipeline"]["label"] == VARIANCE_PIPELINE
    assert data["variance_pipeline"]["n_questions"] == 1  # the subset n, distinct from the answer-variance n=3
    assert "cost" in data


def test_markdown_shows_gate_and_both_variances() -> None:
    answer = _variance(VARIANCE_ANSWER)
    pipeline = _variance(VARIANCE_PIPELINE)
    report = SystemReport(
        "lore", _provenance(3), answer, cache_hit_rate=0.0, stats=RunStats(), variance_pipeline=pipeline
    )
    md = report.markdown()
    assert VARIANCE_ANSWER in md
    assert VARIANCE_PIPELINE in md
    assert "abstention accuracy" in md
    assert "gate" in md.lower()


def test_leaderboard_lists_systems() -> None:
    r1 = SystemReport("lore", _provenance(3), _variance(VARIANCE_ANSWER), cache_hit_rate=0.0, stats=RunStats())
    r2 = SystemReport("mem0", _provenance(3), _variance(VARIANCE_ANSWER), cache_hit_rate=0.0, stats=RunStats())
    md = Leaderboard((r1, r2)).markdown()
    assert "lore" in md
    assert "mem0" in md
