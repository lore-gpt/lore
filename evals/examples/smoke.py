"""Run the LongMemEval smoke against a real Lore stack.

Staged and cost-conscious: start with `--dry-run` (no API calls — it loads the questions and prints the plan
and a coarse cost estimate), then a small `--n 10` sanity run, then `--n 50`. The full 500-question, 3-trial
run (with the Batch API for economy) is a later slice.

Requires a running Lore stack (serve + worker with LORE_EXTRACTION_PROVIDER=anthropic) plus three keys, all
read from the environment:
  LORE_API_KEY / LORE_BASE_URL   — the provisioned project the eval writes into
  ANTHROPIC_API_KEY              — the answerer (and Lore's extractor)
  OPENAI_API_KEY                 — the official GPT-4o judge

Report artifacts land under a gitignored directory; a bare number is never committed.

    uv run python -m examples.smoke --dry-run
    uv run python -m examples.smoke --split fixture --n 3
    uv run python -m examples.smoke --split s --n 50
"""

from __future__ import annotations

import argparse
import datetime
import os
import sys
from pathlib import Path

from longmemeval import (
    DATASET_REPO,
    DATASET_REVISION,
    DEFAULT_ANSWERER_MODEL,
    DEFAULT_JUDGE_MODEL,
    PROMPT_HASH,
    JudgeCache,
    LoreAdapter,
    Provenance,
    Question,
    TrialReport,
    anthropic_answerer,
    deterministic_subset,
    download_split,
    load_questions,
    openai_judge,
    run_trial,
)

_FIXTURE = Path(__file__).resolve().parents[1] / "fixtures" / "clean_room.json"


def _load(split: str, cache_dir: Path) -> list[Question]:
    if split == "fixture":
        return load_questions(_FIXTURE)
    return load_questions(download_split(split, cache_dir / "hf"))


def _estimate(questions: list[Question]) -> None:
    turns = sum(len(s.turns) for q in questions for s in q.sessions)
    ingest_chars = sum(len(t.content) for q in questions for s in q.sessions for t in s.turns)
    print(f"plan: {len(questions)} question(s), {turns} turns to ingest (~{ingest_chars // 4} tokens)")
    print(f"api calls: ~{len(questions)} answerer + {len(questions)} judge (pack-size-dependent tokens each)")
    print("no API calls made (dry run).")


def _require(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        print(f"error: {name} is not set (needed for a real run; use --dry-run to skip)", file=sys.stderr)
        raise SystemExit(2)
    return value


def main() -> None:
    parser = argparse.ArgumentParser(description="LongMemEval smoke against a real Lore stack")
    parser.add_argument("--split", default="fixture", choices=["fixture", "s", "m", "oracle"])
    parser.add_argument("--n", type=int, default=10)
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--judge-model", default=DEFAULT_JUDGE_MODEL)
    parser.add_argument("--answerer-model", default=os.environ.get("LORE_ANSWERER_MODEL", DEFAULT_ANSWERER_MODEL))
    parser.add_argument("--report-dir", default="reports")
    parser.add_argument("--cache-dir", default="judge_cache")
    args = parser.parse_args()

    questions = deterministic_subset(_load(args.split, Path(args.cache_dir)), args.n)
    if args.dry_run:
        _estimate(questions)
        return

    from anthropic import Anthropic
    from loregpt import LoreClient
    from openai import OpenAI

    lore = LoreClient(api_key=_require("LORE_API_KEY"), base_url=os.environ.get("LORE_BASE_URL", "http://localhost:8080"))
    answerer = anthropic_answerer(Anthropic(api_key=_require("ANTHROPIC_API_KEY")), model=args.answerer_model)
    judge = openai_judge(OpenAI(api_key=_require("OPENAI_API_KEY")), model=args.judge_model)
    cache = JudgeCache(Path(args.cache_dir))

    verdicts = run_trial(LoreAdapter(lore, answerer), questions, judge, cache)
    provenance = Provenance(
        dataset=DATASET_REPO if args.split != "fixture" else "clean-room-fixture",
        dataset_revision=DATASET_REVISION if args.split != "fixture" else "n/a",
        split=args.split,
        n=len(questions),
        judge_model=judge.model,
        judge_prompt_hash=PROMPT_HASH,
        answerer_model=args.answerer_model,
        extraction_model=os.environ.get("LORE_EXTRACTION_MODEL", "unknown"),
        generated_at=datetime.datetime.now(datetime.timezone.utc).isoformat(),
    )
    report = TrialReport("lore", provenance, tuple(verdicts), cache_hit_rate=cache.hit_rate)
    stamp = provenance.generated_at.replace(":", "").replace("-", "")
    report.write(Path(args.report_dir) / f"lore.{args.split}.n{len(questions)}.{stamp}.json")
    print(report.markdown())


if __name__ == "__main__":
    main()
