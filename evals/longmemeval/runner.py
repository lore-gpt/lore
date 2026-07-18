"""Run one evaluation trial: for each question, ingest its history into the system, take the system's answer,
and grade it with the shared judge. The same judge + cache instance is threaded through, so the same-judge
discipline holds and unchanged answers are never re-judged."""

from __future__ import annotations

from collections.abc import Sequence

from ._types import Question, Verdict
from .adapters import MemorySystem
from .cache import JudgeCache
from .judge import Judge


def run_trial(
    system: MemorySystem,
    questions: Sequence[Question],
    judge: Judge,
    cache: JudgeCache,
) -> list[Verdict]:
    """Ingest each question's history into a fresh run and judge the system's answer.

    Slice-1 isolates at the RUN level (a new run per question on one project). Distilled memories are
    project-scoped, so cross-run contamination is possible but diluted — an irrelevant earlier memory ranks low
    for a different query. A separate project per question is the escalation if that proves to matter.
    """
    verdicts: list[Verdict] = []
    for question in questions:
        system.ingest(question.sessions)
        answer = system.answer(question.question, question.question_date)
        decision = judge.score(question, answer, cache)
        verdicts.append(
            Verdict(
                question_id=question.question_id,
                question_type=question.question_type,
                generated_answer=answer,
                gold_answer=question.answer,
                correct=decision.correct,
                judge_model=decision.judge_model,
                rubric_version=decision.rubric_version,
            )
        )
    return verdicts
