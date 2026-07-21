from collections.abc import Sequence
from pathlib import Path

from longmemeval import Baseline, GateOutcome, Provenance, Universe, decide_baseline, sanity_ok
from longmemeval.baseline import GATE_MARGIN, PUBLISHED_MEM0_BAND, SANITY_FLOOR


def _kinds(outcomes: Sequence[GateOutcome]) -> dict[str, str]:
    return {o.system: o.kind for o in outcomes}


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


def test_decide_locks_mem0_then_gates_lore_in_one_run() -> None:
    u = _universe()
    to_save, outcomes = decide_baseline(mem0=(0.82, u), lore=(0.80, u), existing=None, now="t")
    assert to_save is not None
    assert to_save.reference_accuracy == 0.82
    kinds = _kinds(outcomes)
    assert kinds["mem0"] == "locked"
    # lore 0.80 >= 0.82 - 0.10 → passes, against the reference locked in this same run
    assert kinds["lore"] == "gate_pass"


def test_decide_does_not_lock_a_below_floor_reference() -> None:
    to_save, outcomes = decide_baseline(mem0=(0.50, _universe()), lore=None, existing=None, now="t")
    assert to_save is None
    assert _kinds(outcomes)["mem0"] == "not_locked_sanity"


def test_decide_leaves_an_already_locked_reference_untouched() -> None:
    u = _universe()
    existing = Baseline(reference_accuracy=0.88, universe=u, measured_at="old")
    # Even a below-floor re-measurement never moves an already-locked reference for the same universe.
    to_save, outcomes = decide_baseline(mem0=(0.60, u), lore=None, existing=existing, now="t")
    assert to_save is None
    assert _kinds(outcomes)["mem0"] == "already_locked"


def test_decide_fails_lore_below_band_and_flags_a_different_universe() -> None:
    u = _universe()
    existing = Baseline(reference_accuracy=0.90, universe=u, measured_at="old")
    # lore 0.79 < 0.90 - 0.10 → fail.
    _, out_fail = decide_baseline(mem0=None, lore=(0.79, u), existing=existing, now="t")
    assert _kinds(out_fail)["lore"] == "gate_fail"
    # A lore run in a different universe (here, a different n) has no applicable baseline.
    _, out_none = decide_baseline(mem0=None, lore=(0.95, _universe(n=10)), existing=existing, now="t")
    assert _kinds(out_none)["lore"] == "no_baseline"
