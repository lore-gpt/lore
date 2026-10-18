"""Diagnose a finished development run: per-type accuracy, and for every wrong answer whether the evidence reached
the answerer at all.

    uv run python -m examples.smoke --split s --subset dev --systems lore --trials 1 --lore-run-dir RUN_DIR ...
    uv run python -m examples.devloop REPORT.json --run-dir RUN_DIR

The second command reads only what the first wrote (the report and RUN_DIR/contexts.jsonl), so it costs nothing and
can be re-run freely. It refuses a sealed subset: inspecting gold answers question by question is exactly what a
verdict set must be kept from.
"""

from __future__ import annotations

import argparse
import json
from pathlib import Path

from longmemeval import ContextCheckpoint
from longmemeval.diagnostics import diagnose, summarize
from longmemeval.subsets import SEALED_PREFIX


def main() -> None:
    parser = argparse.ArgumentParser(description="diagnose a finished development run")
    parser.add_argument("report", help="report JSON written by examples.smoke")
    parser.add_argument("--run-dir", required=True, help="the --lore-run-dir the run used (holds contexts.jsonl)")
    parser.add_argument("--json", default="", help="also write the per-question diagnosis to this file")
    args = parser.parse_args()

    report = json.loads(Path(args.report).read_text("utf-8"))
    subset = str(report["provenance"].get("subset", ""))
    if subset.startswith(SEALED_PREFIX):
        raise SystemExit(f"error: {subset} is a sealed subset — per-question diagnosis is for development subsets")

    contexts = ContextCheckpoint(Path(args.run_dir) / "contexts.jsonl").load()
    diagnoses = diagnose(report, contexts)
    summary = summarize(diagnoses)

    print(f"## {report['system']} — {subset or 'subset not recorded'}")
    print(f"- recall: {summary.correct}/{summary.questions}")
    for kind, count in sorted(summary.failures.items()):
        print(f"- wrong, evidence {kind}: {count}")
    print(
        f"- context lines anchored in time: absolute {summary.absolute_share:.1%} · "
        f"relative ('today', 'recently') {summary.relative_share:.1%} of {summary.pack_lines}"
    )
    print()
    print("| question type | correct | total |")
    print("| --- | --- | --- |")
    for qtype, (correct, total) in summary.by_type.items():
        print(f"| {qtype} | {correct} | {total} |")
    print()
    print("| question | type | evidence |")
    print("| --- | --- | --- |")
    for d in diagnoses:
        if d.failure is not None:
            print(f"| `{d.question_id}` | {d.question_type} | {d.failure} |")

    if args.json:
        rows = [
            {
                "question_id": d.question_id,
                "question_type": d.question_type,
                "correct": d.correct,
                "evidence": d.evidence,
                "pack_lines": d.pack_lines,
                "relative_lines": d.relative_lines,
                "absolute_lines": d.absolute_lines,
            }
            for d in diagnoses
        ]
        Path(args.json).write_text(json.dumps(rows, indent=2) + "\n", "utf-8")


if __name__ == "__main__":
    main()
