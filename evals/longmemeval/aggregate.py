"""Combine several runs of one system over one measurement universe into a single number.

Why a verdict wants more than one run. A Lore run ingests every question through an LLM extractor, and the
memories it writes differ from one ingestion to the next: on the same ten questions, two ingestions of
byte-identical code disagreed on four of them. The answer-variance protocol cannot see that — it holds ingestion fixed
and repeats only the answerer and the judge — so a number read from a single ingestion carries an uncertainty that
the number itself does not show. Averaging independent ingestions (each with its own ``--lore-run-dir``) narrows
it; reporting the range keeps what is left of it visible.

Reports are combined only when they agree on the system and on the universe. Averaging across universes would
blend measurements of different things into a number that describes none of them.
"""

from __future__ import annotations

import statistics
from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from typing import Any

from ._types import Provenance
from .baseline import Universe
from .report import VARIANCE_ANSWER


@dataclass(frozen=True, slots=True)
class Aggregate:
    """The combined recall accuracy of several runs. Accuracies are fractions, one per run, in input order."""

    system: str
    universe: Universe
    accuracies: tuple[float, ...]
    by_type: Mapping[str, tuple[float, ...]]  # per question type, one accuracy per run

    @property
    def mean(self) -> float:
        return statistics.fmean(self.accuracies)

    @property
    def low(self) -> float:
        return min(self.accuracies)

    @property
    def high(self) -> float:
        return max(self.accuracies)

    @property
    def spread(self) -> float:
        """The distance between the best and worst run — how far apart independent ingestions landed."""
        return self.high - self.low

    def type_means(self) -> dict[str, float]:
        return {qtype: statistics.fmean(accs) for qtype, accs in sorted(self.by_type.items())}


def report_universe(report: Mapping[str, Any]) -> Universe:
    """The universe a written report was measured in."""
    return Universe.of(Provenance(**report["provenance"]), VARIANCE_ANSWER)


def aggregate(reports: Sequence[Mapping[str, Any]]) -> Aggregate:
    """Combine written reports (the JSON a run writes). Raises ValueError when there is nothing to combine, or
    when the reports disagree on the system or the universe."""
    if not reports:
        raise ValueError("no reports to aggregate")
    systems = {str(r["system"]) for r in reports}
    if len(systems) != 1:
        raise ValueError(f"reports come from different systems: {sorted(systems)}")
    universes = {report_universe(r) for r in reports}
    if len(universes) != 1:
        raise ValueError(
            "reports were measured in different universes (dataset, split, subset, judge, answerer, n or "
            "protocol differ) — their accuracies are not comparable"
        )
    accuracies = tuple(float(r["variance_answer"]["overall"]["mean"]) for r in reports)
    types = sorted(set.intersection(*(set(r["variance_answer"]["by_type"]) for r in reports)))
    by_type = {t: tuple(float(r["variance_answer"]["by_type"][t]["mean"]) for r in reports) for t in types}
    return Aggregate(system=systems.pop(), universe=universes.pop(), accuracies=accuracies, by_type=by_type)
