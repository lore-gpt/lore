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
    uv run python -m examples.smoke --split s --subset dev --systems lore --trials 3
    uv run python -m examples.smoke --split s --subset dev --systems lore,mem0 --trials 3 --batch --pipeline
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
    MemorySystem,
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
from longmemeval.baseline import baseline_path
from longmemeval.guards import run_dir_blocker, subset_seal_blocker
from longmemeval.subsets import SUBSET_NAMES, Subset, load_subset, select

# What `--lore-poll-timeout 0` resolves to. Distillation of real LongMemEval content was measured at 94s, 177s
# and 260s for the SAME question across three runs, and the economy path lands on a batch cadence rather than
# a per-pass one; the former 60s default was a fixture-scale number that a real pass beats routinely, and
# beating it aborted the run.
DEFAULT_POLL_TIMEOUT_SECONDS = 600.0

_FIXTURE = Path(__file__).resolve().parents[1] / "fixtures" / "clean_room.json"


def _load(split: str, cache_dir: Path) -> list[Question]:
    if split == "fixture":
        return load_questions(_FIXTURE)
    return load_questions(download_split(split, cache_dir / "hf"))


def _subset_for(args: argparse.Namespace) -> Subset | None:
    """The named subset a run measures, or None for a run that chooses its questions by count.

    Split s is chosen only by subset. Drawing it by count hands back the first n questions in a fixed order, and
    past the first 50 that order runs straight into the development and verdict sets — so a count could read a
    sealed subset without anyone asking to."""
    if args.split == "s":
        if not args.subset:
            raise SystemExit(f"error: split s needs --subset (one of {', '.join(SUBSET_NAMES)})")
        if args.n is not None:
            raise SystemExit("error: --n does not apply to split s — its questions come from --subset")
        subset = load_subset(args.subset)
        if subset.split != "s" or subset.dataset_revision != DATASET_REVISION:
            raise SystemExit(
                f"error: subset {subset.name!r} was built for {subset.split}@{subset.dataset_revision}, "
                f"not s@{DATASET_REVISION}"
            )
        return subset
    if args.subset:
        raise SystemExit("error: --subset applies to split s only")
    return None


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
    parser.add_argument(
        "--subset",
        choices=SUBSET_NAMES,
        default="",
        help="named question subset for split s (required there): dev for development, gate-* for a verdict",
    )
    parser.add_argument(
        "--unseal-gate",
        action="store_true",
        help="read a sealed gate-* subset; it is read once, for a verdict, and the read lands in the report",
    )
    parser.add_argument(
        "--n", type=int, default=None, help="questions by count, for the fixture/m/oracle splits (default 50)"
    )
    parser.add_argument(
        "--pipeline-n",
        type=int,
        default=50,
        help="questions for the non-gated pipeline-variance on a count-chosen split (split s reuses its --subset)",
    )
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
    parser.add_argument(
        "--lore-poll-timeout",
        type=float,
        default=0.0,
        help=f"how long one question may wait for distillation before it counts as stalled "
        f"(0 = auto, {DEFAULT_POLL_TIMEOUT_SECONDS:.0f}s)",
    )
    parser.add_argument(
        "--lore-run-dir",
        default="",
        help="ingest Lore's questions through a resumable checkpoint in this directory. Each finished "
        "question's context is written as it lands, so an interrupted run resumes at the question it was "
        "on, and a question whose distillation stalls is set aside and retried at the end instead of "
        "ending the run. Off by default: without it the runner ingests in-process and writes nothing.",
    )
    parser.add_argument(
        "--lore-provision-cmd",
        default=os.environ.get("LORE_PROVISION_CMD", ""),
        help="command that creates ONE empty Lore project and prints its API key on stdout, with {name} "
        "substituted per ingestion. Required for a real Lore run: recall is project-scoped, so without it "
        "every question would see the others' histories. Split with shlex (use forward slashes in paths). "
        'e.g. "docker compose -f infra/compose.yml run --rm --no-deps lore-server provision --project {name}"',
    )
    parser.add_argument("--mem0-top-k", type=int, default=20)
    parser.add_argument(
        "--mem0-workers",
        type=int,
        default=1,
        help="ingest Mem0's questions in this many worker PROCESSES (default 1 = serial). Mem0 spends about "
        "38 minutes per LongMemEval-S question, so n=50 is over thirty hours serially. Questions are "
        "independent scopes, so this only changes how many are in flight — never the order inside one. "
        "Start conservatively: our speed must not cost their quality, and the run reports Mem0-side "
        "failures so a raised error rate is visible before it is baked into a reference.",
    )
    parser.add_argument(
        "--mem0-run-dir",
        default="",
        help="scratch root for this run's Mem0 stores; defaults to a fresh timestamped directory under "
        "--resume-dir. Each run MUST get its own: Mem0's local store outlives the process while the scope "
        "counter restarts at 1, so a shared root makes run 2's question N searchable alongside run 1's.",
    )
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

    subset = _subset_for(args)
    if subset is not None and not args.dry_run:
        seal_blocker = subset_seal_blocker(subset.name, args.unseal_gate)
        if seal_blocker:
            print(f"error: {seal_blocker}", file=sys.stderr)
            raise SystemExit(2)
    if not args.dry_run:
        dir_blocker = run_dir_blocker(args.batch, args.lore_run_dir)
        if dir_blocker:
            print(f"error: {dir_blocker}", file=sys.stderr)
            raise SystemExit(2)
    args.subset_key = subset.key if subset is not None else ""

    all_questions = _load(args.split, Path(args.cache_dir))
    if subset is not None:
        answer_questions = select(all_questions, subset)
        # The pipeline variance re-ingests the same questions; drawing it by count would reach outside the subset.
        pipeline_questions = answer_questions if args.pipeline else []
    else:
        answer_questions = deterministic_subset(all_questions, args.n if args.n is not None else 50)
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

    poll_timeout = args.lore_poll_timeout or DEFAULT_POLL_TIMEOUT_SECONDS
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
                system, answer_questions, answerer, judge, cache, args.trials, stats=stats,
                contexts=_prebuilt_contexts(name, args, answer_questions, sut),
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
            subset=args.subset_key,
        )
        report = SystemReport(name, provenance, variance_answer, cache.hit_rate, stats, variance_pipeline)
        stamp = provenance.generated_at.replace(":", "").replace("-", "")
        label = subset.name if subset is not None else f"n{len(answer_questions)}"
        out_path = Path(args.report_dir) / f"{name}.{args.split}.{label}.{stamp}.json"
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
    _apply_baseline_gate(reports, Path(args.baseline_dir), quiet=args.quiet, now=_now())

    # The leaderboard shows accuracy, so it is only for a local/private run — never the public --quiet path.
    if len(reports) > 1 and not args.quiet:
        print(Leaderboard(tuple(reports)).markdown())


def _apply_baseline_gate(reports: list[SystemReport], baseline_dir: Path, *, quiet: bool, now: str) -> None:
    """Persist Mem0's locked reference (once per universe, sanity-checked) and gate Lore one-sided against it,
    printing each system's outcome. Accuracy numbers are withheld under --quiet (the public CI path): only the
    PASS/FAIL and lock status show — the reference stays in the gitignored baseline file, never the logs."""

    def measured(system: str) -> tuple[float, Universe] | None:
        report = next((r for r in reports if r.system == system), None)
        if report is None:
            return None
        return report.variance_answer.overall().mean, Universe.of(report.provenance, VARIANCE_ANSWER)

    mem0, lore = measured("mem0"), measured("lore")
    # One reference file per universe: the reference that applies is the one stored under the measured universe.
    keyed = mem0 if mem0 is not None else lore
    if keyed is None:
        return
    path = baseline_path(baseline_dir, keyed[1])
    to_save, outcomes = decide_baseline(mem0=mem0, lore=lore, existing=Baseline.load(path), now=now)
    if to_save is not None:
        to_save.save(path)

    for o in outcomes:
        acc = "" if quiet else f" (accuracy {o.accuracy:.3f})"
        if o.kind == "locked":
            print(f"mem0: baseline reference locked for this universe -> {path}{acc}")
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
                "(same dataset revision, split, question subset, judge and answerer)"
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
    """The extractor distilling this deployment's memories (provider/model/prompt-<digest>, or 'fixture'), from
    /healthz.

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


def _mem0_run_root(args: argparse.Namespace) -> Path:
    """This run's own scratch root for Mem0's stores. Fresh by default, because sharing one across runs is
    how question N of run 2 ends up searchable alongside question N of run 1."""
    if args.mem0_run_dir:
        return Path(args.mem0_run_dir)
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%S")
    return Path(args.resume_dir) / f"mem0-run-{stamp}"


def _prebuilt_contexts(
    name: str, args: argparse.Namespace, questions: list[Question], sut: SystemUnderTest
) -> dict[str, str] | None:
    """Ingest ahead of the runner when the run asked for it, or None to let the runner ingest in-process.

    Both paths hand `run_variance_reuse_ingest` the same thing it would have built itself: that protocol holds
    ingestion fixed by definition, so where a context was produced does not change what it is.
    """
    if name == "mem0":
        return _mem0_contexts(name, args, questions, sut)
    if name == "lore":
        return _lore_contexts(args, questions, sut)
    return None


def _lore_contexts(
    args: argparse.Namespace, questions: list[Question], sut: SystemUnderTest
) -> dict[str, str] | None:
    """Ingest Lore's questions serially through a checkpoint, or return None when the run did not ask for one.

    Serial is deliberate and unchanged: Lore's ingestion is server-side work through one deployment, so
    putting questions in flight together would change queue depth and extraction scheduling. What the
    checkpoint buys is the other half of what the Mem0 pool buys — an interrupted or stalled run costs the
    questions it had not finished, not the ones it had.
    """
    if not args.lore_run_dir:
        return None

    from longmemeval import ContextCheckpoint, Heartbeat, build_lore_contexts

    root = Path(args.lore_run_dir)
    root.mkdir(parents=True, exist_ok=True)
    checkpoint = ContextCheckpoint(root / "contexts.jsonl")
    heartbeat = Heartbeat(root / "heartbeat.json", total=len(questions))
    print(f"lore: ingesting {len(questions)} questions serially, checkpointed", file=sys.stderr)
    print(f"      progress: {heartbeat.path}", file=sys.stderr)
    print(
        f"      resume:   {checkpoint.path} "
        f"(re-running with --lore-run-dir {root} skips what is already finished)",
        file=sys.stderr,
        flush=True,
    )
    system = sut.system
    assert isinstance(system, MemorySystem)
    # What a resume is allowed to be: the same questions through the same configuration. The server's
    # extraction prompt is not in here because the harness is never handed it — that one stays discipline.
    fingerprint = {
        "extraction_model": sut.extraction_model,
        "extraction_mode": sut.extraction_mode,
        "embedding_model": sut.embedding_model,
        "split": args.split,
        "questions": str(len(questions)),
        "subset": args.subset_key,
    }
    return build_lore_contexts(
        questions, system, checkpoint=checkpoint, heartbeat=heartbeat, fingerprint=fingerprint
    )


def _mem0_contexts(
    name: str, args: argparse.Namespace, questions: list[Question], sut: SystemUnderTest
) -> dict[str, str] | None:
    """Ingest Mem0's questions through the worker pool, or return None to let the runner ingest serially.

    Only Mem0 takes this path. Lore's ingestion is server-side work through one deployment, so running its
    questions concurrently would change queue depth and extraction scheduling — measurement conditions, not
    just speed — and its serial cost is affordable anyway.
    """
    if name != "mem0" or args.mem0_workers <= 1:
        return None

    from longmemeval import ContextCheckpoint, Heartbeat, Mem0PoolConfig, build_mem0_contexts

    root = _mem0_run_root(args)
    root.mkdir(parents=True, exist_ok=True)
    checkpoint = ContextCheckpoint(root / "contexts.jsonl")
    heartbeat = Heartbeat(root / "heartbeat.json", total=len(questions))
    print(
        f"mem0: ingesting {len(questions)} questions across {args.mem0_workers} worker processes",
        file=sys.stderr,
    )
    print(f"      progress: {heartbeat.path}", file=sys.stderr)
    print(
        f"      resume:   {checkpoint.path} "
        f"(re-running with --mem0-run-dir {root} skips what is already finished)",
        file=sys.stderr,
        flush=True,
    )
    cfg = Mem0PoolConfig(
        root=root,
        llm_model=sut.extraction_model,
        is_reasoning_model=True,
        top_k=args.mem0_top_k,
        split=args.split,
        cache_dir=Path(args.cache_dir),
    )
    return build_mem0_contexts(
        questions, cfg, workers=args.mem0_workers, checkpoint=checkpoint, heartbeat=heartbeat
    )


if __name__ == "__main__":
    main()
