from typing import Any

import pytest

from longmemeval.aggregate import aggregate


def _report(
    acc: float, *, system: str = "lore", subset: str = "dev@aaaa", by_type: dict[str, float] | None = None
) -> dict[str, Any]:
    """The slice of a written report the aggregator reads."""
    return {
        "system": system,
        "provenance": {
            "dataset": "x",
            "dataset_revision": "r",
            "split": "s",
            "n": 70,
            "judge_model": "gpt-4o-2024-08-06",
            "judge_prompt_hash": "h",
            "answerer_model": "claude",
            "extraction_model": "m",
            "generated_at": "t",
            "isolation": "per-question",
            "subset": subset,
        },
        "variance_answer": {
            "overall": {"mean": acc},
            "by_type": {t: {"mean": v} for t, v in (by_type or {"temporal-reasoning": acc}).items()},
        },
    }


def test_mean_and_range_of_independent_runs() -> None:
    agg = aggregate([_report(0.70), _report(0.76)])
    assert agg.mean == pytest.approx(0.73)
    assert (agg.low, agg.high) == (0.70, 0.76)
    assert agg.spread == pytest.approx(0.06)


def test_type_means_come_from_types_every_run_measured() -> None:
    agg = aggregate(
        [
            _report(0.7, by_type={"temporal-reasoning": 0.6, "multi-session": 0.8}),
            _report(0.8, by_type={"temporal-reasoning": 0.8}),
        ]
    )
    assert agg.type_means() == {"temporal-reasoning": pytest.approx(0.7)}


def test_runs_from_different_universes_are_not_averaged() -> None:
    """Averaging a development run with a verdict run would blend two measurements into a number that describes
    neither — refuse it."""
    with pytest.raises(ValueError, match="different universes"):
        aggregate([_report(0.7, subset="dev@aaaa"), _report(0.7, subset="gate-v2@bbbb")])


def test_runs_from_different_systems_are_not_averaged() -> None:
    with pytest.raises(ValueError, match="different systems"):
        aggregate([_report(0.7), _report(0.8, system="mem0")])


def test_nothing_to_aggregate() -> None:
    with pytest.raises(ValueError, match="no reports"):
        aggregate([])
