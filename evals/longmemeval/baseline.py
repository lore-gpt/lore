"""The measurement baseline: a locked Mem0 reference accuracy that Lore is gated against, valid only within
one measurement universe (dataset revision + judge + answerer + n + protocol). The gate is one-sided — Lore
passes at or above the reference minus a margin; exceeding it is a win, not a failure. A baseline is locked
only when the measured Mem0 reference is sanity-consistent with published Mem0 LongMemEval numbers, so a broken
harness cannot lock a bogus low reference that then passes everything.

Accuracies here are fractions (0..1), matching the report's overall mean; the published band and the gate
margin are in the same units."""

from __future__ import annotations

import json
from dataclasses import asdict, dataclass
from pathlib import Path

from ._types import Provenance

# Published Mem0 LongMemEval accuracy points, kept ONLY as a coarse sanity band. Verified 2026-07-21:
#   - 0.944 — Mem0's own research page (vendor claim; tuned config, mean ~6,787 tokens/call; "500 questions,
#             6 categories"; judge undisclosed): https://mem0.ai/research
#   - 0.85  — an independent editorial estimate (self-described as approximate; the same page lists Zep ~0.82;
#             variant and judge unspecified): https://mempalace.tech/benchmarks
# Both are over the LongMemEval 500-question set (6 categories); the -s/-m variants differ only by haystack
# context size, not the accuracy denominator, so no variant correction applies. Our protocol (the official
# LongMemEval judge + a Claude answerer + n=50) is NOT directly comparable to either publication — the band
# exists ONLY to sanity-check the harness, never to grade Lore.
PUBLISHED_MEM0_BAND = (0.85, 0.944)

# A measured Mem0 reference below this floor (15 points under the lower published point) means the harness is
# probably broken, not that Mem0 is bad — so the baseline is NOT locked and the harness is investigated first.
# There is no upper alarm: a shared answerer/judge can push the number either way, and above-band is fine.
SANITY_FLOOR = 0.70

# The one-sided gate margin: Lore passes at or above (reference - GATE_MARGIN). A fraction (0.10 == 10 points),
# matching the ±10 language of the L1 gate.
GATE_MARGIN = 0.10


def sanity_ok(mem0_accuracy: float) -> bool:
    """Whether a freshly measured Mem0 reference is trustworthy enough to lock — one-sided: only a suspiciously
    LOW number (a likely harness break) blocks the lock; above the floor, including above the published band,
    is accepted."""
    return mem0_accuracy >= SANITY_FLOOR


@dataclass(frozen=True, slots=True)
class Universe:
    """The measurement universe a reference is valid in. A baseline compares only within an identical universe;
    change any field and the reference must be re-measured — the same-judge-over-time discipline, made a key."""

    dataset_revision: str
    judge_model: str
    judge_prompt_hash: str
    answerer_model: str
    n: int
    protocol: str

    @staticmethod
    def of(prov: Provenance, protocol: str) -> Universe:
        return Universe(
            dataset_revision=prov.dataset_revision,
            judge_model=prov.judge_model,
            judge_prompt_hash=prov.judge_prompt_hash,
            answerer_model=prov.answerer_model,
            n=prov.n,
            protocol=protocol,
        )


@dataclass(frozen=True, slots=True)
class Baseline:
    """A locked Mem0 reference accuracy and the universe it was measured in."""

    reference_accuracy: float  # Mem0's overall (recall-only) mean accuracy, a fraction
    universe: Universe
    measured_at: str  # stamped by the caller; the library never reads the clock

    def applies_to(self, universe: Universe) -> bool:
        """Whether this baseline is valid for a run in the given universe (every field must match)."""
        return self.universe == universe

    def passes(self, lore_accuracy: float) -> bool:
        """The one-sided ±10 gate: Lore passes at or above the reference minus the margin."""
        return lore_accuracy >= self.reference_accuracy - GATE_MARGIN

    def save(self, path: Path) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(asdict(self), indent=2, sort_keys=True), "utf-8")

    @staticmethod
    def load(path: Path) -> Baseline | None:
        """Load a saved baseline, or None if the file does not exist."""
        if not path.exists():
            return None
        raw = json.loads(path.read_text("utf-8"))
        return Baseline(
            reference_accuracy=float(raw["reference_accuracy"]),
            universe=Universe(**raw["universe"]),
            measured_at=str(raw["measured_at"]),
        )


@dataclass(frozen=True, slots=True)
class GateOutcome:
    """One system's outcome from the baseline decision. kind is one of:
    mem0 → 'locked' | 'already_locked' | 'not_locked_sanity'; lore → 'gate_pass' | 'gate_fail' | 'no_baseline'."""

    system: str
    kind: str
    accuracy: float
    reference: float | None  # the baseline reference accuracy when one applied, else None


def decide_baseline(
    *,
    mem0: tuple[float, Universe] | None,
    lore: tuple[float, Universe] | None,
    existing: Baseline | None,
    now: str,
) -> tuple[Baseline | None, list[GateOutcome]]:
    """The pure baseline decision (no I/O): given each measured system's (accuracy, universe) and the existing
    locked baseline, return the baseline to PERSIST (or None to leave the stored one as-is) and the per-system
    outcomes to report. Mem0 locks a sanity-consistent reference for its universe the first time it is measured
    there; Lore is gated one-sided against the locked reference for its own universe — including a reference
    just locked by Mem0 in the same run."""
    outcomes: list[GateOutcome] = []
    to_save: Baseline | None = None
    baseline = existing

    if mem0 is not None:
        acc, universe = mem0
        if baseline is not None and baseline.applies_to(universe):
            # A reference is locked once per universe; a later Mem0 run in the same universe does not move it.
            outcomes.append(GateOutcome("mem0", "already_locked", acc, baseline.reference_accuracy))
        elif sanity_ok(acc):
            baseline = Baseline(reference_accuracy=acc, universe=universe, measured_at=now)
            to_save = baseline
            outcomes.append(GateOutcome("mem0", "locked", acc, acc))
        else:
            outcomes.append(GateOutcome("mem0", "not_locked_sanity", acc, None))

    if lore is not None:
        acc, universe = lore
        if baseline is not None and baseline.applies_to(universe):
            kind = "gate_pass" if baseline.passes(acc) else "gate_fail"
            outcomes.append(GateOutcome("lore", kind, acc, baseline.reference_accuracy))
        else:
            outcomes.append(GateOutcome("lore", "no_baseline", acc, None))

    return to_save, outcomes
