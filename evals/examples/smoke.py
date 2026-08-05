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
import itertools
import os
import sys
from pathlib import Path
from typing import NamedTuple

from longmemeval import (
    DATASET_REPO,
    DATASET_REVISION,
    DEFAULT_ANSWERER_MODEL,
    DEFAULT_JUDGE_MODEL,
    GATE_MARGIN,
    PROMPT_HASH,
    SANITY_FLOOR,
    VARIANCE_ANSWER,
    VARIANCE_PIPELINE,
    Baseline,
    JudgeCache,
    Leaderboard,
    Provenance,
    Question,
    ResumeStore,
    RunStats,
    SystemReport,
    Universe,
    VarianceResult,
    dataset_pin_blocker,
    decide_baseline,
    deterministic_subset,
    download_split,
    load_questions,
    lore_embedder_blocker,
    lore_extraction_blocker,
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
    parser.add_argument(
        "--lore-provision-cmd",
        default=os.environ.get("LORE_PROVISION_CMD", ""),
        help="command that creates ONE empty Lore project and prints its API key on stdout, with {name} "
        "substituted per ingestion. Required for a real Lore run: recall is project-scoped, so without it "
        "every question would see the others' histories. Split with shlex (use forward slashes in paths). "
        'e.g. "docker compose -f infra/compose.yml run --rm --no-deps lore-server provision --project {name}"',
    )
    parser.add_argument("--mem0-top-k", type=int, default=20)
    parser.add_argument("--report-dir", default="reports")
    parser.add_argument("--cache-dir", default="judge_cache")
    parser.add_argument("--resume-dir", default="batch_resume")
    parser.add_argument(
        "--baseline-dir", default="baseline", help="gitignored dir holding the locked Mem0 baseline reference"
    )
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
        sut = _build_system(name, args, poll_timeout)
        system = sut.system
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
            extraction_model=sut.extraction_model,
            generated_at=_now(),
            extraction_mode=sut.extraction_mode,
            system_config=sut.config,
            embedding_model=sut.embedding_model,
            isolation=sut.isolation,
            provision_command=sut.provision_command,
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

    # Baseline gate: lock Mem0's reference for this universe (once), then gate Lore one-sided against it.
    _apply_baseline_gate(reports, Path(args.baseline_dir) / "mem0.json", quiet=args.quiet, now=_now())

    # The leaderboard shows accuracy, so it is only for a local/private run — never the public --quiet path.
    if len(reports) > 1 and not args.quiet:
        print(Leaderboard(tuple(reports)).markdown())


def _apply_baseline_gate(reports: list[SystemReport], baseline_path: Path, *, quiet: bool, now: str) -> None:
    """Persist Mem0's locked reference (once per universe, sanity-checked) and gate Lore one-sided against it,
    printing each system's outcome. Accuracy numbers are withheld under --quiet (the public CI path): only the
    PASS/FAIL and lock status show — the reference stays in the gitignored baseline file, never the logs."""

    def measured(system: str) -> tuple[float, Universe] | None:
        report = next((r for r in reports if r.system == system), None)
        if report is None:
            return None
        return report.variance_answer.overall().mean, Universe.of(report.provenance, VARIANCE_ANSWER)

    to_save, outcomes = decide_baseline(
        mem0=measured("mem0"), lore=measured("lore"), existing=Baseline.load(baseline_path), now=now
    )
    if to_save is not None:
        to_save.save(baseline_path)

    for o in outcomes:
        acc = "" if quiet else f" (accuracy {o.accuracy:.3f})"
        if o.kind == "locked":
            print(f"mem0: baseline reference locked for this universe -> {baseline_path}{acc}")
        elif o.kind == "already_locked":
            print("mem0: reference already locked for this universe (left unchanged)")
        elif o.kind == "not_locked_sanity":
            print(
                f"mem0: reference below the sanity floor ({SANITY_FLOOR:.2f}) — NOT locked; investigate the "
                f"harness before trusting a run{acc}"
            )
        elif o.kind in ("gate_pass", "gate_fail"):
            verdict = "PASS" if o.kind == "gate_pass" else "FAIL"
            ref = o.reference if o.reference is not None else 0.0
            detail = "" if quiet else f" (lore {o.accuracy:.3f} vs mem0 ref {ref:.3f} - {GATE_MARGIN:.2f})"
            print(f"lore: +/-10 baseline gate {verdict}{detail}")
        elif o.kind == "no_baseline":
            print(
                "lore: no Mem0 baseline locked for this universe — measure mem0 first "
                "(same dataset revision, judge, answerer, n)"
            )


class SystemUnderTest(NamedTuple):
    """A constructed memory system plus the fairness metadata its report must carry. A named tuple rather
    than a bare tuple because these are provenance fields: a caller that silently swapped two of them would
    misreport what was measured, and the report is the only record a later reader gets."""

    system: object
    extraction_model: str
    extraction_mode: str
    config: str
    embedding_model: str
    # How this system kept one question's memory out of another's. Both systems isolate per question — a
    # fresh project for Lore, a fresh user_id for Mem0 — which is what makes them comparable at all.
    isolation: str
    # Only Lore needs a command to create an isolated scope; Mem0's is a string it makes up, so this is empty
    # there. Recorded for reproducibility; it carries no secret.
    provision_command: str = ""


def _build_system(name: str, args: argparse.Namespace, poll_timeout: float) -> SystemUnderTest:
    """Construct a memory system + its fairness metadata."""
    from longmemeval import LoreAdapter, Mem0Adapter

    if name == "lore":
        from loregpt import LoreClient

        from longmemeval import lore_isolation_blocker, parse_command, provision_project

        base_url = os.environ.get("LORE_BASE_URL", "http://localhost:8080")

        # Fail closed before spending anything: without per-question projects the run would measure recall
        # over a pool of other questions' histories, which is a different (and much easier to misread)
        # question than the one LongMemEval asks.
        isolation_blocker = lore_isolation_blocker(args.lore_provision_cmd)
        if isolation_blocker:
            print(f"error: {isolation_blocker}", file=sys.stderr)
            raise SystemExit(2)
        argv = parse_command(args.lore_provision_cmd)

        # One fresh project per ingestion. The counter only has to make names unique within a run; the
        # measurement database is single-use, so nothing reclaims the projects afterwards.
        counter = itertools.count(1)

        def new_lore_client() -> LoreClient:
            key = provision_project(argv, f"lme-{next(counter)}")
            return LoreClient(api_key=key, base_url=base_url)

        # Dogfooding: the operator runs the worker in this extraction mode; the adapter records it and waits on
        # the matching cadence (economy distillation lands on a batch schedule).
        lore = LoreAdapter(new_lore_client, poll_timeout=poll_timeout)
        # Record the retrieval-context budget so the fairness record makes each system's context size explicit
        # (a cross-system delta could otherwise reflect a budget asymmetry rather than memory quality).
        health = _lore_health(base_url)
        extraction_model = _lore_extraction_identity(health)
        embedding_model = _lore_embedding_model(base_url, health)
        # Fail closed on both halves of the pipeline the score depends on: the model that WROTE the memories
        # and the vector space they are recalled through. Neither may be unknown, and neither may be the
        # offline fixture — canned output and a vector space no deployment uses do not measure the product.
        for blocker in (lore_extraction_blocker(extraction_model), lore_embedder_blocker(embedding_model)):
            if blocker:
                print(f"error: {blocker}", file=sys.stderr)
                raise SystemExit(2)
        config = f"retrieval token_budget={lore.token_budget}"
        return SystemUnderTest(
            lore, extraction_model, args.extraction_mode, config, embedding_model,
            isolation="per-question", provision_command=args.lore_provision_cmd,
        )

    if name == "mem0":
        from importlib.metadata import version

        from mem0 import Memory

        from longmemeval import mem0_retrieval_blocker

        # Fail closed before spending anything: Mem0's keyword and entity legs are optional dependencies
        # that go quiet rather than loud, and a competitor scored on one leg of three would understate it
        # in our favour. The probe runs once, here, rather than per question.
        retrieval_blocker = mem0_retrieval_blocker(_mem0_degraded_legs())
        if retrieval_blocker:
            print(f"error: {retrieval_blocker}", file=sys.stderr)
            raise SystemExit(2)

        # A bare Memory() cannot write on mem0ai 2.0.12: its default model is gpt-5-mini, and its default
        # params include temperature=0.1 / top_p=0.1 / max_tokens, which that model rejects with a 400.
        #
        # mem0 already handles this — LLMBase._get_supported_params drops those params for reasoning models —
        # but its detector's set lists "gpt-5", "gpt-5o", "gpt-5o-mini" and NOT "gpt-5-mini", so mem0's own
        # default slips past mem0's own guard. Rather than pick a different engine for the competitor, which
        # is the one thing a parity comparison must not do, this sets mem0's documented `is_reasoning_model`
        # escape hatch. The model stays mem0's, and the request becomes exactly the shape mem0 intends for a
        # GPT-5 model: messages + response_format, with the unsupported params dropped by mem0's own code.
        #
        # Everything else stays mem0's choice, including its embedder — text-embedding-3-small@1536, which
        # happens to be the same one Lore is configured with here, so neither side gets a vector-space edge.
        mem0_llm_model = "gpt-5-mini"
        adapter = Mem0Adapter(
            Memory.from_config(
                {
                    "llm": {
                        "provider": "openai",
                        "config": {"model": mem0_llm_model, "is_reasoning_model": True},
                    }
                }
            ),
            top_k=args.mem0_top_k,
            config_label=(
                f"oss-default + is_reasoning_model=True "
                f"(mem0's own flag; its detector misses {mem0_llm_model})"
            ),
        )
        # mem0 runs its own OpenAI-backed extraction and embedding on write; there is no separate "extraction
        # mode", and its embedder is internal (not configured or introspected here).
        config = f"mem0ai {version('mem0ai')} ({adapter.config_label}); retrieval top_k={adapter.top_k}"
        # Mem0 isolates for free: the adapter searches under a fresh per-ingest user_id, so no provisioning
        # command is needed. Same guarantee as Lore's fresh project, which is what makes the two comparable.
        return SystemUnderTest(adapter, mem0_llm_model, "", config, "mem0-internal", isolation="per-question")

    raise SystemExit(f"unknown system: {name!r} (expected lore or mem0)")


def _lore_health(base_url: str) -> dict[str, object]:
    """Read the server's /healthz once. Both provenance fields below come out of it, and one request is
    enough for both — two reads could also disagree if the server were restarted between them. An empty
    mapping means the read failed; a provenance read must never abort the run, so nothing raises here."""
    import json
    import urllib.request

    try:
        with urllib.request.urlopen(f"{base_url.rstrip('/')}/healthz", timeout=5) as resp:
            body = json.loads(resp.read().decode("utf-8"))
    except Exception:
        return {}
    return body if isinstance(body, dict) else {}


def _lore_embedding_model(base_url: str, health: dict[str, object]) -> str:
    """The composed embedder identity (model@dim), from /healthz — the authoritative source, since it
    reflects the server actually under test. If that read failed, fall back to composing from THIS process's
    LORE_EMBEDDING_* env only when the server is local (the env plausibly matches the server we launched);
    for a remote server, return 'unknown' rather than a confident guess that could misreport the vector space
    and poison the provenance record."""
    import urllib.parse

    embedder = health.get("embedder")
    if isinstance(embedder, str) and embedder:
        return embedder
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


def _lore_extraction_identity(health: dict[str, object]) -> str:
    """The extractor distilling this deployment's memories (provider/model, or 'fixture'), from /healthz.

    Unlike the embedder there is deliberately NO environment fallback. Extraction is configured on the
    worker, a different process in a different container, so this process's LORE_EXTRACTION_* says nothing
    about it — and reading it here is exactly how the report came to carry a confident 'unknown' while a real
    model was doing the work. The server is the only witness; if it does not say, we do not know."""
    extraction = health.get("extraction")
    return extraction if isinstance(extraction, str) and extraction else "unknown"


def _mem0_degraded_legs() -> list[str]:
    """Which of Mem0's retrieval legs are not actually running, as human-readable reasons.

    Mem0's search is a hybrid — semantic, BM25 keyword, and an entity boost — and the two non-semantic legs
    are optional dependencies that degrade to a no-op with a log line and no error. A bare install therefore
    scores a competitor running on one leg of three, against Lore's full hybrid. That is an asymmetry in our
    favour, which is the one direction a parity comparison must never lean, and it is invisible in the
    result: the number simply comes out lower.

    Probes are end-to-end rather than a package-presence check, because presence is not the same as working:
    spaCy installs cleanly and then fails to import (its CLI module reaches for click, which current Typer no
    longer supplies), and Mem0 reports that as "spaCy is not installed"."""
    degraded: list[str] = []
    try:
        from fastembed import SparseTextEmbedding  # noqa: F401
    except Exception as exc:
        degraded.append(f"BM25 keyword search is off ({type(exc).__name__}: {exc})")
    try:
        from mem0.utils.entity_extraction import extract_entities
    except Exception as exc:
        # Mem0's internals moved. We cannot tell whether the leg is live, so we say so and block rather
        # than assume: this probe needs updating for the installed version.
        degraded.append(f"the entity leg could not be probed — this harness's probe needs updating ({exc})")
    else:
        if not extract_entities("The auth service was rolled forward to 2.4.0 on Friday."):
            degraded.append("entity extraction returns nothing (spaCy missing, unimportable, or no model)")
    return degraded


if __name__ == "__main__":
    main()
