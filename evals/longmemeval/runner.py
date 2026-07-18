"""Run evaluation trials. Every flow shares the same building blocks — the memory system's `retrieve`, the
shared answerer prompt, and the shared judge prompt — so a synchronous run and a Batch-API run send byte-
identical prompts and differ only in HOW the answerer/judge calls are dispatched (one at a time vs one batch).

Flows (each has a synchronous form and a Batch-API twin):
  - run_trial: one synchronous trial (ingest -> retrieve -> answer -> judge per question).
  - run_variance_reuse_ingest[_batched]: ingest ONCE per question, then answer+judge N times. Isolates
    answerer+judge nondeterminism — the variance the harness gates on (target std < 2). Ingestion is held
    fixed; each trial judges under its own cache variant so the trials sample independently.
  - run_variance_pipeline[_batched]: full re-ingest + re-answer N times. Also captures memory-construction
    nondeterminism (a system property, reported separately, not gated).
  - run_trial_batched: one trial in three phases (ingest+retrieve all -> batch-answer -> batch-judge), the
    economy path for a full run.
"""

from __future__ import annotations

from collections.abc import Callable, Sequence

from ._types import Question, Verdict
from .adapters import MemorySystem
from .answerer import ANSWER_SYSTEM, Answerer, build_answer_prompt
from .batch import BatchProvider, BatchRequest, ResumeStore, run_batch
from .cache import JudgeCache, JudgeDecision
from .judge import Judge
from .stats import RunStats, estimate_tokens


def _verdict(question: Question, answer: str, decision: JudgeDecision) -> Verdict:
    return Verdict(
        question_id=question.question_id,
        question_type=question.question_type,
        generated_answer=answer,
        gold_answer=question.answer,
        correct=decision.correct,
        judge_model=decision.judge_model,
        rubric_version=decision.rubric_version,
    )


def _answer_sync(answerer: Answerer, context: str, question: Question, stats: RunStats | None) -> str:
    if stats is not None:
        stats.answerer_sync_calls += 1
        prompt = build_answer_prompt(context, question.question, question.question_date)
        stats.answerer_input_tokens += estimate_tokens(ANSWER_SYSTEM + prompt)
    return answerer(context, question.question, question.question_date)


def run_trial(
    system: MemorySystem,
    questions: Sequence[Question],
    answerer: Answerer,
    judge: Judge,
    cache: JudgeCache,
    *,
    stats: RunStats | None = None,
) -> list[Verdict]:
    """One synchronous trial: ingest each question's history into a fresh run, retrieve, answer, judge."""
    verdicts: list[Verdict] = []
    for question in questions:
        system.ingest(question.sessions)
        context = system.retrieve(question.question, question.question_date)
        answer = _answer_sync(answerer, context, question, stats)
        decision = judge.score_many([(question, answer)], cache, stats=stats)[question.question_id]
        verdicts.append(_verdict(question, answer, decision))
    return verdicts


def run_variance_reuse_ingest(
    system: MemorySystem,
    questions: Sequence[Question],
    answerer: Answerer,
    judge: Judge,
    cache: JudgeCache,
    n_trials: int,
    *,
    stats: RunStats | None = None,
) -> list[list[Verdict]]:
    """Ingest each question ONCE, then run `n_trials` answer+judge passes over the fixed retrieved contexts.
    The per-trial accuracy spread is the answerer+judge variance the harness gates on (ingestion held fixed).
    Each trial judges under its own cache variant, so a variance measurement actually samples N times (a shared
    cache key would serve trial 1's decision to every later trial and collapse the spread to zero)."""
    contexts: dict[str, str] = {}
    for question in questions:
        system.ingest(question.sessions)
        contexts[question.question_id] = system.retrieve(question.question, question.question_date)

    trials: list[list[Verdict]] = []
    for trial in range(n_trials):
        verdicts: list[Verdict] = []
        for question in questions:
            answer = _answer_sync(answerer, contexts[question.question_id], question, stats)
            decision = judge.score_many(
                [(question, answer)], cache, stats=stats, variant=f"t{trial}"
            )[question.question_id]
            verdicts.append(_verdict(question, answer, decision))
        trials.append(verdicts)
    return trials


def run_variance_pipeline(
    system: MemorySystem,
    questions: Sequence[Question],
    answerer: Answerer,
    judge: Judge,
    cache: JudgeCache,
    n_trials: int,
    *,
    stats: RunStats | None = None,
) -> list[list[Verdict]]:
    """Full re-ingest + re-answer `n_trials` times over the SAME question set (run on a small fixed subset).
    Each trial ingests into a fresh isolated scope, so the spread also reflects memory-construction
    nondeterminism — reported as a separate, non-gated system property."""
    trials: list[list[Verdict]] = []
    for trial in range(n_trials):
        verdicts: list[Verdict] = []
        for question in questions:
            system.ingest(question.sessions)
            context = system.retrieve(question.question, question.question_date)
            answer = _answer_sync(answerer, context, question, stats)
            decision = judge.score_many(
                [(question, answer)], cache, stats=stats, variant=f"t{trial}"
            )[question.question_id]
            verdicts.append(_verdict(question, answer, decision))
        trials.append(verdicts)
    return trials


def _ingest_and_retrieve(system: MemorySystem, questions: Sequence[Question]) -> dict[str, str]:
    contexts: dict[str, str] = {}
    for question in questions:
        system.ingest(question.sessions)
        contexts[question.question_id] = system.retrieve(question.question, question.question_date)
    return contexts


def _batched_answer_and_judge(
    questions: Sequence[Question],
    contexts: dict[str, str],
    answer_batch: BatchProvider,
    judge: Judge,
    judge_batch: BatchProvider,
    cache: JudgeCache,
    *,
    stats: RunStats | None,
    resume: ResumeStore | None,
    sleep: Callable[[float], None] | None,
    variant: str = "",
    resume_ns: str = "",
) -> list[Verdict]:
    """Phase 2+3 over already-retrieved contexts: answer every question in ONE batch, then judge them all in
    ONE batch (cache-aware). Uses the same shared prompt builders as the synchronous path. `variant` scopes the
    judge cache to this trial (so a variance run re-judges each trial). `resume_ns` namespaces the resume keys
    to this trial so distinct trials' batches never alias. The answer batch's resume id is kept until the judge
    phase also collects, so a crash during the judge poll re-collects the finished answers (free) instead of
    re-submitting them (paid)."""
    answer_key = f"{answer_batch.name}{resume_ns}"
    answer_requests: list[BatchRequest] = []
    token_est = 0
    for question in questions:
        prompt = build_answer_prompt(contexts[question.question_id], question.question, question.question_date)
        answer_requests.append(BatchRequest(custom_id=question.question_id, system=ANSWER_SYSTEM, prompt=prompt))
        token_est += estimate_tokens(ANSWER_SYSTEM + prompt)
    answer_outcome = run_batch(
        answer_batch, answer_requests, resume=resume, resume_key=answer_key, clear_on_success=False, sleep=sleep
    )
    answers = answer_outcome.results
    if stats is not None:
        fallbacks = len(answer_outcome.fallback_ids)
        stats.answerer_batch_calls += len(answer_requests) - fallbacks
        stats.answerer_sync_calls += fallbacks
        stats.answerer_input_tokens += token_est

    pairs = [(question, answers[question.question_id]) for question in questions]
    decisions = judge.score_many(
        pairs,
        cache,
        batch=judge_batch,
        resume=resume,
        resume_key=f"{judge_batch.name}{resume_ns}",
        sleep=sleep,
        stats=stats,
        variant=variant,
    )
    # Both phases collected — release the deferred answer-batch resume id.
    if resume is not None:
        resume.clear(answer_key)
    return [_verdict(q, answers[q.question_id], decisions[q.question_id]) for q in questions]


def run_trial_batched(
    system: MemorySystem,
    questions: Sequence[Question],
    answer_batch: BatchProvider,
    judge: Judge,
    judge_batch: BatchProvider,
    cache: JudgeCache,
    *,
    stats: RunStats | None = None,
    resume: ResumeStore | None = None,
    sleep: Callable[[float], None] | None = None,
) -> list[Verdict]:
    """One trial in three phases: (1) ingest + retrieve every question; (2) answer them all in ONE Batch-API
    job; (3) judge them all in ONE Batch-API job (cache-aware). Prompts are built with the same shared builders
    the synchronous path uses, so a batched result matches its synchronous twin bit-for-bit."""
    contexts = _ingest_and_retrieve(system, questions)
    return _batched_answer_and_judge(
        questions, contexts, answer_batch, judge, judge_batch, cache, stats=stats, resume=resume, sleep=sleep
    )


def run_variance_reuse_ingest_batched(
    system: MemorySystem,
    questions: Sequence[Question],
    answer_batch: BatchProvider,
    judge: Judge,
    judge_batch: BatchProvider,
    cache: JudgeCache,
    n_trials: int,
    *,
    stats: RunStats | None = None,
    resume: ResumeStore | None = None,
    sleep: Callable[[float], None] | None = None,
) -> list[list[Verdict]]:
    """The Batch-API twin of run_variance_reuse_ingest: ingest ONCE, then run `n_trials` batched answer+judge
    passes over the fixed retrieved contexts. This is the economy path for the gated answer-variance on a full
    set — the dominant answerer+judge volume runs at the Batch-API's half rate. Each trial scopes its judge
    cache + resume keys by trial, so trials sample independently and a crash never grafts one trial's batch
    onto another."""
    contexts = _ingest_and_retrieve(system, questions)
    return [
        _batched_answer_and_judge(
            questions,
            contexts,
            answer_batch,
            judge,
            judge_batch,
            cache,
            stats=stats,
            resume=resume,
            sleep=sleep,
            variant=f"t{trial}",
            resume_ns=f".t{trial}",
        )
        for trial in range(n_trials)
    ]


def run_variance_pipeline_batched(
    system: MemorySystem,
    questions: Sequence[Question],
    answer_batch: BatchProvider,
    judge: Judge,
    judge_batch: BatchProvider,
    cache: JudgeCache,
    n_trials: int,
    *,
    stats: RunStats | None = None,
    resume: ResumeStore | None = None,
    sleep: Callable[[float], None] | None = None,
) -> list[list[Verdict]]:
    """The Batch-API twin of run_variance_pipeline: full re-ingest each trial, then batched answer+judge. Keeps
    the pipeline-variance stage on the same dispatch mode as the gated stage under `--batch`, so the run's cost
    accounting is not a mix of batch and sync calls."""
    trials: list[list[Verdict]] = []
    for trial in range(n_trials):
        contexts = _ingest_and_retrieve(system, questions)
        trials.append(
            _batched_answer_and_judge(
                questions,
                contexts,
                answer_batch,
                judge,
                judge_batch,
                cache,
                stats=stats,
                resume=resume,
                sleep=sleep,
                variant=f"pt{trial}",
                resume_ns=f".pt{trial}",
            )
        )
    return trials
