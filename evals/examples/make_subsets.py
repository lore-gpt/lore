"""Regenerate the committed question subsets (evals/subsets/*.json) from the pinned LongMemEval-S split.

    uv run python -m examples.make_subsets            # write the files
    uv run python -m examples.make_subsets --check    # exit 1 if the committed files differ

The files hold question ids and types only. Regenerating must reproduce them byte for byte: a subset is an
identity a measurement is recorded against, so the selection rule in longmemeval.subsets is not expected to
change, and --check is how to prove it has not.
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

from longmemeval import DATASET_REVISION, download_split, load_questions
from longmemeval.subsets import SUBSET_NAMES, SUBSETS_DIR, build_subsets, subset_document


def main() -> None:
    parser = argparse.ArgumentParser(description="regenerate the committed question subsets")
    parser.add_argument("--cache-dir", default="judge_cache")
    parser.add_argument("--check", action="store_true", help="compare against the committed files; write nothing")
    args = parser.parse_args()

    questions = load_questions(download_split("s", Path(args.cache_dir) / "hf"))
    subsets = build_subsets(questions)
    stale = []
    for name in SUBSET_NAMES:
        text = subset_document(name, subsets[name], "s", DATASET_REVISION)
        path = SUBSETS_DIR / f"{name}.json"
        if args.check:
            if not path.exists() or path.read_text("utf-8") != text:
                stale.append(name)
            continue
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, "utf-8", newline="\n")
        print(f"{name}: {len(subsets[name])} questions -> {path}")
    if stale:
        print(f"committed subsets differ from the dataset: {', '.join(stale)}", file=sys.stderr)
        raise SystemExit(1)


if __name__ == "__main__":
    main()
