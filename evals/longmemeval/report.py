"""Reports. A TrialReport carries the verdicts plus the full provenance chain, so a score is reproducible and
two runs are comparable only when the chain matches. SystemSummary aggregates trials into a mean +/- std (the
variance report; a single trial has std 0). Report artifacts are written under a gitignored directory — a bare
number never enters the repo."""

from __future__ import annotations

import json
import statistics
from collections.abc import Sequence
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Any

from ._types import Provenance, Verdict


def _accuracy(verdicts: Sequence[Verdict]) -> float:
    if not verdicts:
        return 0.0
    return sum(1 for v in verdicts if v.correct) / len(verdicts)


@dataclass(frozen=True, slots=True)
class TrialReport:
    """One trial's graded results with its reproducibility provenance."""

    system: str
    provenance: Provenance
    verdicts: tuple[Verdict, ...]
    cache_hit_rate: float

    @property
    def n(self) -> int:
        return len(self.verdicts)

    @property
    def accuracy(self) -> float:
        return _accuracy(self.verdicts)

    def accuracy_by_type(self) -> dict[str, float]:
        groups: dict[str, list[Verdict]] = {}
        for verdict in self.verdicts:
            groups.setdefault(verdict.question_type, []).append(verdict)
        return {qtype: _accuracy(group) for qtype, group in sorted(groups.items())}

    def to_dict(self) -> dict[str, Any]:
        return {
            "system": self.system,
            "provenance": asdict(self.provenance),
            "n": self.n,
            "accuracy": self.accuracy,
            "cache_hit_rate": self.cache_hit_rate,
            "accuracy_by_type": self.accuracy_by_type(),
            "verdicts": [asdict(v) for v in self.verdicts],
        }

    def write(self, path: Path) -> None:
        """Persist the full artifact as JSON (under a gitignored reports/ directory)."""
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(self.to_dict(), indent=2, sort_keys=True), "utf-8")

    def markdown(self) -> str:
        lines = [
            f"## LongMemEval — {self.system}",
            "",
            f"- accuracy: **{self.accuracy:.3f}** (n={self.n})",
            f"- judge: `{self.provenance.judge_model}` (prompt `{self.provenance.judge_prompt_hash[:12]}`)",
            f"- answerer: `{self.provenance.answerer_model}` · extraction: `{self.provenance.extraction_model}`",
            f"- dataset: `{self.provenance.dataset}`@`{self.provenance.dataset_revision}` "
            f"({self.provenance.split}) · cache hit rate: {self.cache_hit_rate:.2f}",
            "",
            "| question type | accuracy |",
            "| --- | --- |",
        ]
        lines.extend(f"| {qtype} | {acc:.3f} |" for qtype, acc in self.accuracy_by_type().items())
        return "\n".join(lines)


@dataclass(frozen=True, slots=True)
class SystemSummary:
    """A system's accuracy across trials: mean +/- std. The variance report — never a single-run "we passed"."""

    system: str
    trial_accuracies: tuple[float, ...]

    @property
    def mean(self) -> float:
        return statistics.mean(self.trial_accuracies) if self.trial_accuracies else 0.0

    @property
    def std(self) -> float:
        return statistics.stdev(self.trial_accuracies) if len(self.trial_accuracies) >= 2 else 0.0


def aggregate(system: str, trials: Sequence[TrialReport]) -> SystemSummary:
    return SystemSummary(system=system, trial_accuracies=tuple(trial.accuracy for trial in trials))
