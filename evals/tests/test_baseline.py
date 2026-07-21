from pathlib import Path

from longmemeval import Baseline, Provenance, Universe, sanity_ok
from longmemeval.baseline import GATE_MARGIN, PUBLISHED_MEM0_BAND, SANITY_FLOOR


def _universe(**over: object) -> Universe:
    base = dict(
        dataset_revision="3f1a9c0",
        judge_model="gpt-4o-2024-08-06",
        judge_prompt_hash="deadbeef",
        answerer_model="claude",
        n=50,
        protocol="variance_answer",
    )
    base.update(over)
    return Universe(**base)  # type: ignore[arg-type]


def test_sanity_floor_is_one_sided() -> None:
    low, high = PUBLISHED_MEM0_BAND
    assert low < high
    assert not sanity_ok(SANITY_FLOOR - 0.01)  # below the floor → not lockable
    assert sanity_ok(SANITY_FLOOR)  # at the floor → lockable
    assert sanity_ok(high + 0.10)  # above the published band → still fine (no upper alarm)


def test_gate_is_one_sided_around_the_reference() -> None:
    base = Baseline(reference_accuracy=0.80, universe=_universe(), measured_at="t")
    assert base.passes(0.80 - GATE_MARGIN)  # exactly reference - margin → pass
    assert base.passes(0.95)  # far above → pass (exceeding is a win)
    assert not base.passes(0.80 - GATE_MARGIN - 0.001)  # just below the band → fail


def test_universe_must_match_for_a_baseline_to_apply() -> None:
    base = Baseline(reference_accuracy=0.80, universe=_universe(), measured_at="t")
    assert base.applies_to(_universe())
    assert not base.applies_to(_universe(dataset_revision="other"))  # a different revision invalidates it
    assert not base.applies_to(_universe(n=10))  # a different n is a different universe


def test_universe_from_provenance_carries_the_key_fields() -> None:
    prov = Provenance(
        dataset="x",
        dataset_revision="3f1a9c0",
        split="s",
        n=50,
        judge_model="gpt-4o-2024-08-06",
        judge_prompt_hash="deadbeef",
        answerer_model="claude",
        extraction_model="haiku",
        generated_at="t",
    )
    assert Universe.of(prov, "variance_answer") == _universe()


def test_baseline_round_trips_through_disk(tmp_path: Path) -> None:
    base = Baseline(reference_accuracy=0.83, universe=_universe(), measured_at="2026-07-21T00:00:00Z")
    path = tmp_path / "mem0.json"
    assert Baseline.load(path) is None  # absent → None
    base.save(path)
    assert Baseline.load(path) == base
