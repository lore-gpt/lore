"""Run the LongMemEval harness against real memory systems, under one shared answerer + one shared judge.

Staged and cost-conscious:
  --dry-run            no API calls; loads the questions and prints the plan + a coarse cost estimate (keyless).
  --systems lore,mem0  which systems to score (default: lore).
  --trials N           trials for the GATED answer-variance (default: 3).
  --batch              route the answerer + judge through the Batch API (half rate — the full-run economy path).
  --pipeline           also run the (non-gated) pipeline-variance on a fixed small subset.

The competitor answers through the SAME answerer and grades through the SAME judge as Lore — the parity is in
the shared pipeline, not each vendor's bundled models. Report artifacts land under a gitignored directory; a
bare number is never committed.

Real runs read keys from the environment:
  LORE_API_KEY / LORE_BASE_URL   the lore system
  OPENAI_API_KEY                 the shared GPT-4o judge (also Mem0's internal extraction LLM)
  ANTHROPIC_API_KEY              the shared Claude answerer

    uv run python -m examples.smoke --dry-run
    uv run python -m examples.smoke --split s --n 50 --systems lore --trials 3
    uv run python -m examples.smoke --split s --n 500 --systems lore,mem0 --trials 3 --batch --pipeline
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
    VARIANCE_ANSWER,
    VARIANCE_PIPELINE,
    JudgeCache,
    Leaderboard,
    Provenance,
    Question,
    ResumeStore,
    RunStats,
    SystemReport,
    VarianceResult,
    dataset_pin_blocker,
    deterministic_subset,
    download_split,
    load_questions,
    lore_embedder_blocker,
    run_variance_pipeline,
    run_variance_pipeline_batched,
    run_variance_reuse_ingest,
    run_variance_reuse_ingest_batched,
)

_FIXTURE = Path(__file__).resolve().parents[1] / "fixtures" / "clean_room.json"


def _load(split: str, cache_dir: Path) -> list[Question]:
    if split == "fixture":
        return load_questions(_FIXTURE)
    return load_questions(download_split(split, cache_dir / "hf"))


def _estimate(systems: list[str], answer_qs: list[Question], pipeline_qs: list[Question], trials: int) -> None:
    turns = sum(len(s.turns) for q in answer_qs for s in q.sessions)
    per_system_answer = len(answer_qs) * trials
    per_system_pipeline = len(pipeline_qs) * trials if pipeline_qs else 0
    print(f"plan: {len(systems)} system(s) {systems}, {len(answer_qs)} questions x {trials} trials")
    print(f"  ingest: ~{turns} turns per system (answer-variance ingests once; pipeline re-ingests each trial)")
    print(f"  answer-variance: ~{per_system_answer} answerer + ~{per_system_answer} judge calls per system")
    if pipeline_qs:
        print(f"  pipeline-variance: ~{per_system_pipeline} answerer + ~{per_system_pipeline} judge calls per system")
    print("no API calls made (dry run).")


def _require(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        print(f"error: {name} is not set (needed for a real run; use --dry-run to skip)", file=sys.stderr)
        raise SystemExit(2)
    return value


def _now() -> str:
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def main() -> None:
    parser = argparse.ArgumentParser(description="LongMemEval harness against real memory systems")
    parser.add_argument("--split", default="fixture", choices=["fixture", "s", "m", "oracle"])
    parser.add_argument("--n", type=int, default=50, help="questions for the gated answer-variance")
    parser.add_argument("--pipeline-n", type=int, default=50, help="fixed subset for the non-gated pipeline-variance")
    parser.add_argument("--systems", default="lore", help="comma-separated: lore,mem0")
    parser.add_argument("--trials", type=int, default=3)
    parser.add_argument("--batch", action="store_true", help="use the Batch API for the answerer + judge")
    parser.add_argument("--pipeline", action="store_true", help="also run the pipeline-variance")
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument(
        "--quiet",
        action="store_true",
        help="print only the variance gate (PASS/FAIL) + cost, never the accuracy — for a public CI run where "
        "the score stays in the gitignored artifact, not the logs",
    )
    parser.add_argument("--judge-model", default=DEFAULT_JUDGE_MODEL)
    parser.add_argument("--answerer-model", default=os.environ.get("LORE_ANSWERER_MODEL", DEFAULT_ANSWERER_MODEL))
    parser.add_argument("--extraction-mode", default="realtime", choices=["realtime", "economy"])
    parser.add_argument("--lore-poll-timeout", type=float, default=0.0, help="0 = auto (60s realtime, 600s economy)")
    parser.add_argument("--mem0-top-k", type=int, default=20)
    parser.add_argument("--report-dir", default="reports")
    parser.add_argument("--cache-dir", default="judge_cache")
    parser.add_argument("--resume-dir", default="batch_resume")
    args = parser.parse_args()

    systems = [s.strip() for s in args.systems.split(",") if s.strip()]

    if not args.dry_run:
        # Block a real run before it spends anything (download or API) if the dataset revision is unpinned.
        pin_blocker = dataset_pin_blocker(args.split, DATASET_REVISION)
        if pin_blocker:
            print(f"error: {pin_blocker}", file=sys.stderr)
            raise SystemExit(2)

    all_questions = _load(args.split, Path(args.cache_dir))
    answer_questions = deterministic_subset(all_questions, args.n)
    pipeline_questions = deterministic_subset(all_questions, args.pipeline_n) if args.pipeline else []

    if args.dry_run:
        _estimate(systems, answer_questions, pipeline_questions, args.trials)
        return

    # Real run: import the live clients + build the shared answerer/judge (kept out of the keyless dry-run).
    from anthropic import Anthropic
    from openai import OpenAI

    from longmemeval import AnthropicBatchProvider, Judge, OpenAIBatchProvider, anthropic_answerer, openai_judge
    from longmemeval.answerer import ANSWER_MAX_TOKENS

    anthropic = Anthropic(api_key=_require("ANTHROPIC_API_KEY"))
    openai = OpenAI(api_key=_require("OPENAI_API_KEY"))
    answerer = anthropic_answerer(anthropic, model=args.answerer_model)
    judge: Judge = openai_judge(openai, model=args.judge_model)
    cache = JudgeCache(Path(args.cache_dir))

    answer_batch = judge_batch = None
    if args.batch:
        answer_batch = AnthropicBatchProvider(
            anthropic, args.answerer_model, name="answerer", max_tokens=ANSWER_MAX_TOKENS
        )
        judge_batch = OpenAIBatchProvider(openai, args.judge_model, name="judge", max_tokens=10)

    poll_timeout = args.lore_poll_timeout or (600.0 if args.extraction_mode == "economy" else 60.0)
    reports = []
    for name in systems:
        system, extraction_model, extraction_mode, system_config, embedding_model = _build_system(
            name, args, poll_timeout
        )
        stats = RunStats()

        if args.batch:
            resume = ResumeStore(Path(args.resume_dir) / f"{name}.json")
            answer_trials = run_variance_reuse_ingest_batched(
                system,
                answer_questions,
                answer_batch,
                judge,
                judge_batch,
                cache,
                args.trials,
                stats=stats,
                resume=resume,
            )
        else:
            answer_trials = run_variance_reuse_ingest(
                system, answer_questions, answerer, judge, cache, args.trials, stats=stats
            )
        variance_answer = VarianceResult(VARIANCE_ANSWER, tuple(tuple(t) for t in answer_trials))

        variance_pipeline = None
        if args.pipeline:
            if args.batch:
                pipe_resume = ResumeStore(Path(args.resume_dir) / f"{name}.pipeline.json")
                pipe_trials = run_variance_pipeline_batched(
                    system, pipeline_questions, answer_batch, judge, judge_batch, cache, args.trials,
                    stats=stats, resume=pipe_resume,
                )
            else:
                pipe_trials = run_variance_pipeline(
                    system, pipeline_questions, answerer, judge, cache, args.trials, stats=stats
                )
            variance_pipeline = VarianceResult(VARIANCE_PIPELINE, tuple(tuple(t) for t in pipe_trials))

        provenance = Provenance(
            dataset=DATASET_REPO if args.split != "fixture" else "clean-room-fixture",
            dataset_revision=DATASET_REVISION if args.split != "fixture" else "n/a",
            split=args.split,
            n=len(answer_questions),
            judge_model=judge.model,
            judge_prompt_hash=PROMPT_HASH,
            answerer_model=args.answerer_model,
            extraction_model=extraction_model,
            generated_at=_now(),
            extraction_mode=extraction_mode,
            system_config=system_config,
            embedding_model=embedding_model,
        )
        report = SystemReport(name, provenance, variance_answer, cache.hit_rate, stats, variance_pipeline)
        stamp = provenance.generated_at.replace(":", "").replace("-", "")
        out_path = Path(args.report_dir) / f"{name}.{args.split}.n{len(answer_questions)}.{stamp}.json"
        report.write(out_path)
        if args.quiet:
            # Public-CI mode: the accuracy stays in the gitignored artifact; the logs carry only the stability
            # gate and the cost, never the score.
            print(f"{name}: answer-variance gate {'PASS' if report.passes_gate else 'FAIL'} (report -> {out_path})")
            for line in stats.summary_lines():
                print(f"  {line}")
        else:
            print(report.markdown())
            print()
        reports.append(report)

    # The leaderboard shows accuracy, so it is only for a local/private run — never the public --quiet path.
    if len(reports) > 1 and not args.quiet:
        print(Leaderboard(tuple(reports)).markdown())


def _build_system(name: str, args: argparse.Namespace, poll_timeout: float) -> tuple[object, str, str, str, str]:
    """Construct a memory system + its fairness metadata (extraction_model, extraction_mode, system_config,
    embedding_model)."""
    from longmemeval import LoreAdapter, Mem0Adapter

    if name == "lore":
        from loregpt import LoreClient

        base_url = os.environ.get("LORE_BASE_URL", "http://localhost:8080")
        client = LoreClient(api_key=_require("LORE_API_KEY"), base_url=base_url)
        # Dogfooding: the operator runs the worker in this extraction mode; the adapter records it and waits on
        # the matching cadence (economy distillation lands on a batch schedule).
        lore = LoreAdapter(client, poll_timeout=poll_timeout)
        # Record the retrieval-context budget so the fairness record makes each system's context size explicit
        # (a cross-system delta could otherwise reflect a budget asymmetry rather than memory quality).
        extraction_model = os.environ.get("LORE_EXTRACTION_MODEL", "unknown")
        embedding_model = _lore_embedding_model(base_url)
        # Fail closed: a real run must know its embedder (read from /healthz) and must not measure the offline
        # fixture — a score against a vector space no deployment uses would misreport the embedding model.
        embed_blocker = lore_embedder_blocker(embedding_model)
        if embed_blocker:
            print(f"error: {embed_blocker}", file=sys.stderr)
            raise SystemExit(2)
        config = f"retrieval token_budget={lore.token_budget}"
        return lore, extraction_model, args.extraction_mode, config, embedding_model

    if name == "mem0":
        from importlib.metadata import version

        from mem0 import Memory

        adapter = Mem0Adapter(Memory(), top_k=args.mem0_top_k)
        # mem0 runs its own OpenAI-backed extraction and embedding on write; there is no separate "extraction
        # mode", and its embedder is internal (not configured or introspected here).
        config = f"mem0ai {version('mem0ai')} ({adapter.config_label}); retrieval top_k={adapter.top_k}"
        return adapter, "mem0-internal", "", config, "mem0-internal"

    raise SystemExit(f"unknown system: {name!r} (expected lore or mem0)")


def _lore_embedding_model(base_url: str) -> str:
    """Read the composed embedder identity (model@dim) from the server's /healthz — the authoritative source,
    since it reflects the server actually under test. If that read fails, fall back to composing from THIS
    process's LORE_EMBEDDING_* env only when the server is local (the env plausibly matches the server we
    launched); for a remote server, return 'unknown' rather than a confident guess that could misreport the
    vector space and poison the provenance record. A provenance read must never abort the run."""
    import json
    import urllib.parse
    import urllib.request

    try:
        with urllib.request.urlopen(f"{base_url.rstrip('/')}/healthz", timeout=5) as resp:
            body = json.loads(resp.read().decode("utf-8"))
        embedder = body.get("embedder")
        if isinstance(embedder, str) and embedder:
            return embedder
    except Exception:
        # Best-effort provenance; a health read must never abort the eval.
        pass
    # Health read failed (or an older server without the field). Trust this process's env only for a local
    # server — a remote server's embedder is unrelated to our env, so composing would write a provably-false
    # identity. Prefer an honest 'unknown' over a confident wrong answer.
    host = urllib.parse.urlparse(base_url).hostname or ""
    if host not in ("localhost", "127.0.0.1", "::1"):
        return "unknown"
    provider = os.environ.get("LORE_EMBEDDING_PROVIDER", "").strip().lower()
    if provider in ("", "fixture"):
        return "fixture-embed-v1@64"
    model = os.environ.get("LORE_EMBEDDING_MODEL", "unknown")
    dim = os.environ.get("LORE_EMBEDDING_DIM", "0")
    return f"{model}@{dim}"


if __name__ == "__main__":
    main()
