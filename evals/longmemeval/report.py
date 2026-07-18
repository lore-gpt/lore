"""Reports. A run scores a system under two variance protocols and the report keeps them SEPARATE and labelled
so they can never be conflated:

  - variance_answer   — ingest once, answer+judge N times over the FULL set. Measures answerer+judge
                        nondeterminism (what the harness controls). This is the GATED metric (target std < 2).
  - variance_pipeline — full re-ingest N times over a fixed small subset. Adds memory-construction
                        nondeterminism (a system property); reported, NOT gated. Its n is recorded because it
                        is a subset estimate, not the full set.

Each is a mean ± std over trials, plus a per-type breakdown and abstention accuracy as its own line (a
never-abstaining system scores 0 on the `_abs` items — that measures calibration, not recall, so it is never
folded into the overall number). The full provenance chain makes a score reproducible; report artifacts are
written under a gitignored directory — a bare number never enters the repo."""

from __future__ import annotations

import json
import statistics
from collections.abc import Sequence
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any

from ._types import Provenance, Verdict
from .stats import RunStats

# Human-readable protocol definitions, stamped into the report so the two variance numbers are self-describing.
VARIANCE_ANSWER = "variance_answer"
VARIANCE_PIPELINE = "variance_pipeline"
_PROTOCOL_TEXT = {
    VARIANCE_ANSWER: "ingest once, then answer+judge each trial (answerer+judge variance; the gated metric)",
    VARIANCE_PIPELINE: "full re-ingest each trial on a fixed subset (adds memory-construction variance; not gated)",
}


def _accuracy(verdicts: Sequence[Verdict]) -> float:
    if not verdicts:
        return 0.0
    return sum(1 for v in verdicts if v.correct) / len(verdicts)


@dataclass(frozen=True, slots=True)
class MeanStd:
    """An accuracy summarised across trials: mean, sample std (0 for a single trial), and the per-trial n."""

    mean: float
    std: float
    n: int  # questions per trial
    trials: int  # number of trials

    @staticmethod
    def of(per_trial: Sequence[float], n: int) -> MeanStd:
        values = list(per_trial)
        mean = statistics.mean(values) if values else 0.0
        std = statistics.stdev(values) if len(values) >= 2 else 0.0
        return MeanStd(mean=mean, std=std, n=n, trials=len(values))


def _is_abstention(verdict: Verdict) -> bool:
    return verdict.question_id.endswith("_abs")


def _recall(verdicts: Sequence[Verdict]) -> list[Verdict]:
    """The non-abstention verdicts — the recall subset. Abstention (_abs) items are calibration, reported on
    their own line, and are excluded from overall/per-type accuracy so they never drag the recall number."""
    return [v for v in verdicts if not _is_abstention(v)]


@dataclass(frozen=True, slots=True)
class VarianceResult:
    """One variance protocol's outcome: N trials of verdicts, summarised as mean ± std overall, per type, and
    for abstention items alone."""

    label: str
    trials: tuple[tuple[Verdict, ...], ...]

    @property
    def protocol(self) -> str:
        return _PROTOCOL_TEXT.get(self.label, "")

    @property
    def n_questions(self) -> int:
        """Recall questions per trial (abstention items excluded — they are reported separately)."""
        return len(_recall(self.trials[0])) if self.trials else 0

    @property
    def n_trials(self) -> int:
        return len(self.trials)

    def overall(self) -> MeanStd:
        # Recall only — abstention is its own line, never folded into the headline accuracy.
        return MeanStd.of([_accuracy(_recall(t)) for t in self.trials], self.n_questions)

    def by_type(self) -> dict[str, MeanStd]:
        recalled = [_recall(t) for t in self.trials]
        types: set[str] = {v.question_type for t in recalled for v in t}
        out: dict[str, MeanStd] = {}
        for qtype in sorted(types):
            counts = [sum(1 for v in t if v.question_type == qtype) for t in recalled]
            per_trial = [_accuracy([v for v in t if v.question_type == qtype]) for t in recalled]
            out[qtype] = MeanStd.of(per_trial, counts[0] if counts else 0)
        return out

    def abstention(self) -> MeanStd:
        counts = [sum(1 for v in t if _is_abstention(v)) for t in self.trials]
        per_trial = [_accuracy([v for v in t if _is_abstention(v)]) for t in self.trials]
        return MeanStd.of(per_trial, counts[0] if counts else 0)

    def to_dict(self) -> dict[str, Any]:
        return {
            "label": self.label,
            "protocol": self.protocol,
            "n_questions": self.n_questions,
            "n_trials": self.n_trials,
            "overall": asdict(self.overall()),
            "by_type": {k: asdict(v) for k, v in self.by_type().items()},
            "abstention": asdict(self.abstention()),
            "trial_accuracies": [_accuracy(t) for t in self.trials],
            "trials": [[asdict(v) for v in t] for t in self.trials],
        }


@dataclass(frozen=True, slots=True)
class SystemReport:
    """A full report for one system: both variance protocols (pipeline optional), the provenance chain, the
    judge cache-hit rate, and the run's cost stats."""

    system: str
    provenance: Provenance
    variance_answer: VarianceResult
    cache_hit_rate: float
    stats: RunStats
    variance_pipeline: VarianceResult | None = None
    gate_std: float = 2.0  # the std<gate target on variance_answer overall accuracy (in accuracy points → /100)

    @property
    def passes_gate(self) -> bool:
        """The variance gate: overall answer-variance std under the target (expressed in accuracy points)."""
        return self.variance_answer.overall().std * 100 <= self.gate_std

    def to_dict(self) -> dict[str, Any]:
        return {
            "system": self.system,
            "provenance": asdict(self.provenance),
            "cache_hit_rate": self.cache_hit_rate,
            "gate_std": self.gate_std,
            "passes_gate": self.passes_gate,
            "cost": asdict(self.stats),
            "variance_answer": self.variance_answer.to_dict(),
            "variance_pipeline": self.variance_pipeline.to_dict() if self.variance_pipeline else None,
        }

    def write(self, path: Path) -> None:
        """Persist the full artifact as JSON (under a gitignored reports/ directory)."""
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(self.to_dict(), indent=2, sort_keys=True), "utf-8")

    def markdown(self) -> str:
        answer = self.variance_answer.overall()
        abst = self.variance_answer.abstention()
        lines = [
            f"## LongMemEval — {self.system}",
            "",
            f"- accuracy ({VARIANCE_ANSWER}, n={answer.n}): **{answer.mean:.3f} ± {answer.std:.3f}** "
            f"over {answer.trials} trials — gate std<{self.gate_std} pts: {'PASS' if self.passes_gate else 'FAIL'}",
        ]
        if self.variance_pipeline is not None:
            pipe = self.variance_pipeline.overall()
            lines.append(
                f"- accuracy ({VARIANCE_PIPELINE}, n={pipe.n} subset): {pipe.mean:.3f} ± {pipe.std:.3f} "
                f"over {pipe.trials} trials (not gated)"
            )
        lines.extend(
            [
                f"- abstention accuracy: {abst.mean:.3f} ± {abst.std:.3f} ({abst.n} items)",
                f"- judge: `{self.provenance.judge_model}` (prompt `{self.provenance.judge_prompt_hash[:12]}`)",
                f"- answerer: `{self.provenance.answerer_model}` · extraction: "
                f"`{self.provenance.extraction_model}`"
                + (f" (mode: {self.provenance.extraction_mode})" if self.provenance.extraction_mode else ""),
            ]
        )
        if self.provenance.embedding_model:
            lines.append(f"- embedding: `{self.provenance.embedding_model}`")
        if self.provenance.system_config:
            lines.append(f"- system config: {self.provenance.system_config}")
        lines.extend(
            [
                f"- dataset: `{self.provenance.dataset}`@`{self.provenance.dataset_revision}` "
                f"({self.provenance.split}) · cache hit rate: {self.cache_hit_rate:.2f}",
                "",
                f"| question type | accuracy ({VARIANCE_ANSWER}) |",
                "| --- | --- |",
            ]
        )
        lines.extend(
            f"| {qtype} | {ms.mean:.3f} ± {ms.std:.3f} |"
            for qtype, ms in self.variance_answer.by_type().items()
        )
        lines.append("")
        lines.append("cost (coarse estimate, batch stages ~50% rate):")
        lines.extend(f"- {line}" for line in self.stats.summary_lines())
        return "\n".join(lines)


@dataclass(frozen=True, slots=True)
class Leaderboard:
    """Several systems' reports side by side — the cross-system parity view under one shared judge."""

    reports: tuple[SystemReport, ...] = field(default_factory=tuple)

    def markdown(self) -> str:
        lines = [
            "## LongMemEval leaderboard (one shared judge)",
            "",
            "| system | accuracy (answer-variance) | gate | pipeline-variance |",
            "| --- | --- | --- | --- |",
        ]
        for r in self.reports:
            a = r.variance_answer.overall()
            pipe = (
                f"{r.variance_pipeline.overall().mean:.3f} ± {r.variance_pipeline.overall().std:.3f} "
                f"(n={r.variance_pipeline.overall().n})"
                if r.variance_pipeline is not None
                else "—"
            )
            lines.append(
                f"| {r.system} | {a.mean:.3f} ± {a.std:.3f} (n={a.n}) | "
                f"{'PASS' if r.passes_gate else 'FAIL'} | {pipe} |"
            )
        return "\n".join(lines)
