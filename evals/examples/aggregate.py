"""Combine several runs of one system over one universe and apply the baseline gate to their mean.

    uv run python -m examples.aggregate reports/lore.s.dev.A.json reports/lore.s.dev.B.json

Each report must come from an independent ingestion (its own --lore-run-dir). The gate is applied to the mean, and
the range between runs is printed beside it: a mean that clears the bar while one run sits far below it is a
weaker result than the same mean from two runs that agree, and the reader should be able to see which it is.
"""

from __future__ import annotations

import argparse
import json
from pathlib import Path

from longmemeval import GATE_MARGIN, Baseline
from longmemeval.aggregate import aggregate
from longmemeval.baseline import baseline_path


def main() -> None:
    parser = argparse.ArgumentParser(description="combine runs of one system and gate their mean")
    parser.add_argument("reports", nargs="+", help="report JSON files written by examples.smoke")
    parser.add_argument("--baseline-dir", default="baseline")
    args = parser.parse_args()

    reports = [json.loads(Path(p).read_text("utf-8")) for p in args.reports]
    agg = aggregate(reports)

    print(f"## {agg.system} — {len(agg.accuracies)} run(s), subset {agg.universe.subset or '(by count)'}")
    for path, acc in zip(args.reports, agg.accuracies, strict=True):
        print(f"- {acc:.3f}  {path}")
    print(f"- mean {agg.mean:.3f} · range {agg.low:.3f} to {agg.high:.3f} (spread {agg.spread * 100:.1f} pts)")
    print()
    print("| question type | mean | per run |")
    print("| --- | --- | --- |")
    for qtype, mean in agg.type_means().items():
        runs = " / ".join(f"{a:.3f}" for a in agg.by_type[qtype])
        print(f"| {qtype} | {mean:.3f} | {runs} |")
    print()

    reference = Baseline.load(baseline_path(Path(args.baseline_dir), agg.universe))
    if agg.system != "lore":
        return
    if reference is None:
        print("lore: no Mem0 reference locked for this universe — measure mem0 on the same subset first")
        return
    verdict = "PASS" if reference.passes(agg.mean) else "FAIL"
    print(
        f"lore: +/-10 baseline gate {verdict} on the mean (lore {agg.mean:.3f} vs mem0 ref "
        f"{reference.reference_accuracy:.3f} - {GATE_MARGIN:.2f}; runs {agg.low:.3f} to {agg.high:.3f})"
    )


if __name__ == "__main__":
    main()
