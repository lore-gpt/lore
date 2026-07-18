from pathlib import Path

import pytest

from longmemeval._types import Question, Session, Turn
from longmemeval.cache import JudgeCache
from longmemeval.judge import PROMPT_HASH, Judge, build_grading_prompt

_JUDGE_MODEL = "gpt-4o-2024-08-06"


def _question(qid: str = "q1", qtype: str = "single-session-user", answer: str = "Biscuit") -> Question:
    return Question(
        question_id=qid,
        question_type=qtype,
        question="What is the dog's name?",
        answer=answer,
        question_date="2031/01/01",
        sessions=(Session("s", "2031/01/01", (Turn("user", "My dog is Biscuit.", has_answer=True),)),),
    )


def test_parses_yes_no_and_caches(tmp_path: Path) -> None:
    calls: list[str] = []

    def complete(prompt: str) -> str:
        calls.append(prompt)
        return "Yes."

    judge = Judge(complete=complete, model=_JUDGE_MODEL)
    cache = JudgeCache(tmp_path)
    question = _question()

    first = judge.score(question, "The dog is named Biscuit.", cache)
    assert first.correct is True
    assert first.judge_model == _JUDGE_MODEL
    # A second identical score is served from cache — the judge is not called again.
    second = judge.score(question, "The dog is named Biscuit.", cache)
    assert second == first
    assert len(calls) == 1
    assert cache.hit_rate == 0.5


def test_no_is_wrong(tmp_path: Path) -> None:
    judge = Judge(complete=lambda _: "No", model=_JUDGE_MODEL)
    verdict = judge.score(_question(), "I am not sure.", JudgeCache(tmp_path))
    assert verdict.correct is False


def test_official_prompt_selection_by_type() -> None:
    default = build_grading_prompt("multi-session", "q?", "gold", "resp", abstention=False)
    temporal = build_grading_prompt("temporal-reasoning", "q?", "gold", "resp", abstention=False)
    preference = build_grading_prompt("single-session-preference", "q?", "gold", "resp", abstention=False)
    abstention = build_grading_prompt("single-session-user", "q?", "gold", "resp", abstention=True)

    assert "off-by-one" in temporal and "off-by-one" not in default
    assert "Rubric:" in preference
    assert "unanswerable" in abstention
    # The question, gold, and response are interpolated in order.
    assert "Question: q?" in default and "Correct Answer: gold" in default and "Model Response: resp" in default


def test_unknown_task_raises_like_upstream() -> None:
    # The official get_anscheck_prompt fails fast on an unknown task; so does ours (never grade under the
    # wrong rubric). Abstention is exempt — it grades any task with the unanswerable template.
    with pytest.raises(ValueError, match="unknown LongMemEval task type"):
        build_grading_prompt("some-new-type", "q?", "gold", "resp", abstention=False)
    # Abstention still works for any task string.
    assert "unanswerable" in build_grading_prompt("some-new-type", "q?", "gold", "resp", abstention=True)


def test_prompt_hash_is_stable() -> None:
    # A fixed 64-hex sha256 of the embedded official prompts — bumps only if the prompts change.
    assert len(PROMPT_HASH) == 64
    assert all(c in "0123456789abcdef" for c in PROMPT_HASH)
