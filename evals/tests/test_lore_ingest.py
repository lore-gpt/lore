import json
from collections.abc import Sequence
from pathlib import Path

import pytest

from longmemeval._types import Question, Session, Turn
from longmemeval.adapters.base import MemorySystem
from longmemeval.adapters.lore import DistillationTimeout
from longmemeval.lore_ingest import build_contexts
from longmemeval.progress import ContextCheckpoint, Heartbeat


class FakeSystem(MemorySystem):
    """A memory system that records the order it was asked to ingest. A question id in `stalls` raises
    DistillationTimeout until it has been attempted that many times; `raise_on` fails one outright."""

    def __init__(self, *, stalls: dict[str, int] | None = None, raise_on: str | None = None) -> None:
        self.ingested: list[str] = []
        self.attempts: dict[str, int] = {}
        self._stalls = stalls or {}
        self._raise_on = raise_on
        self._current = ""

    @property
    def name(self) -> str:
        return "lore"

    def ingest(self, sessions: Sequence[Session]) -> None:
        # The fake keys off the session id, which the questions below give the same value as their own id.
        question_id = sessions[0].session_id
        self._current = question_id
        self.attempts[question_id] = self.attempts.get(question_id, 0) + 1
        self.ingested.append(question_id)
        if question_id == self._raise_on:
            raise RuntimeError("server refused the write")
        if self.attempts[question_id] <= self._stalls.get(question_id, 0):
            raise DistillationTimeout(f"{question_id} never caught up")

    def retrieve(self, question: str, question_date: str) -> str:
        return f"CONTEXT[{self._current}]"


def _question(question_id: str) -> Question:
    return Question(
        question_id=question_id,
        question_type="single-session-user",
        question=f"q-{question_id}",
        answer="a",
        question_date="2031/01/04",
        sessions=(Session(question_id, "2031/01/04", (Turn("user", "hi"),)),),
    )


def _io(tmp_path: Path, total: int) -> tuple[ContextCheckpoint, Heartbeat]:
    return (
        ContextCheckpoint(tmp_path / "contexts.jsonl"),
        Heartbeat(tmp_path / "heartbeat.json", total=total, now=lambda: 0.0),
    )


def test_ingests_every_question_and_checkpoints_each_as_it_lands(tmp_path: Path) -> None:
    questions = [_question("q1"), _question("q2")]
    checkpoint, heartbeat = _io(tmp_path, len(questions))

    contexts = build_contexts(questions, FakeSystem(), checkpoint=checkpoint, heartbeat=heartbeat)

    assert contexts == {"q1": "CONTEXT[q1]", "q2": "CONTEXT[q2]"}
    # On disk, not just in memory: the point of the checkpoint is to survive the process.
    assert checkpoint.load() == contexts


def test_resume_skips_what_is_already_finished(tmp_path: Path) -> None:
    questions = [_question("q1"), _question("q2")]
    checkpoint, heartbeat = _io(tmp_path, len(questions))
    checkpoint.record("q1", "CONTEXT-FROM-EARLIER-RUN")
    system = FakeSystem()

    contexts = build_contexts(questions, system, checkpoint=checkpoint, heartbeat=heartbeat)

    assert system.ingested == ["q2"], "a resumed question must not be ingested (and paid for) again"
    assert contexts["q1"] == "CONTEXT-FROM-EARLIER-RUN"
    assert contexts["q2"] == "CONTEXT[q2]"


def test_a_checkpoint_from_another_run_does_not_leak_questions_in(tmp_path: Path) -> None:
    checkpoint, heartbeat = _io(tmp_path, 1)
    checkpoint.record("q9", "CONTEXT-FOR-A-QUESTION-NOT-IN-THIS-RUN")

    contexts = build_contexts([_question("q1")], FakeSystem(), checkpoint=checkpoint, heartbeat=heartbeat)

    assert contexts == {"q1": "CONTEXT[q1]"}


def test_a_stalled_question_is_set_aside_and_retried_after_the_others(tmp_path: Path) -> None:
    questions = [_question("q1"), _question("q2"), _question("q3")]
    checkpoint, heartbeat = _io(tmp_path, len(questions))
    system = FakeSystem(stalls={"q2": 1})  # q2 stalls on its first attempt only

    contexts = build_contexts(questions, system, checkpoint=checkpoint, heartbeat=heartbeat)

    assert len(contexts) == 3, "a stall must not cost the run the questions it could still finish"
    # Order is the claim: q2 stalled, q3 carried on anyway, and q2 came back at the END rather than blocking.
    assert system.ingested == ["q1", "q2", "q3", "q2"]


def test_a_question_that_never_catches_up_fails_the_run_loudly(tmp_path: Path) -> None:
    questions = [_question("q1"), _question("q2")]
    checkpoint, heartbeat = _io(tmp_path, len(questions))
    system = FakeSystem(stalls={"q2": 99})

    with pytest.raises(RuntimeError) as excinfo:
        build_contexts(questions, system, checkpoint=checkpoint, heartbeat=heartbeat)

    message = str(excinfo.value)
    assert "q2" in message, "the operator has to be told WHICH question is missing"
    assert str(checkpoint.path) in message, "and where the work that did finish is"
    # A partial set is never returned: scoring 1 of 2 questions as if it were the run is the failure this
    # guards against. What survives is the checkpoint, so the resume costs only q2.
    assert checkpoint.load() == {"q1": "CONTEXT[q1]"}


def test_stall_tolerance_does_not_swallow_a_real_failure(tmp_path: Path) -> None:
    questions = [_question("q1"), _question("q2")]
    checkpoint, heartbeat = _io(tmp_path, len(questions))
    system = FakeSystem(raise_on="q2")

    # A refused write says the deployment is wrong, not that one question is slow — retrying it n times and
    # then reporting a stall would describe the wrong problem.
    with pytest.raises(RuntimeError, match="server refused the write"):
        build_contexts(questions, system, checkpoint=checkpoint, heartbeat=heartbeat)

    assert system.attempts["q2"] == 1, "a non-stall failure is not retried"
    assert checkpoint.load() == {"q1": "CONTEXT[q1]"}


def test_heartbeat_names_the_question_in_flight_and_the_stalled_ones(tmp_path: Path) -> None:
    questions = [_question("q1"), _question("q2")]
    checkpoint, heartbeat = _io(tmp_path, len(questions))
    system = FakeSystem(stalls={"q1": 99})

    with pytest.raises(RuntimeError):
        build_contexts(questions, system, checkpoint=checkpoint, heartbeat=heartbeat)

    state = json.loads(heartbeat.path.read_text("utf-8"))
    assert state["stalled"] == ["q1"], "a reader of the heartbeat file has to see which question is stuck"
    assert state["done"] == 1
    assert state["in_flight"] == []


def test_a_resume_under_a_different_configuration_is_refused(tmp_path: Path) -> None:
    questions = [_question("q1")]
    checkpoint, heartbeat = _io(tmp_path, len(questions))
    first = {"extraction_model": "haiku", "split": "s"}
    build_contexts(questions, FakeSystem(), checkpoint=checkpoint, heartbeat=heartbeat, fingerprint=first)

    checkpoint2, heartbeat2 = _io(tmp_path, len(questions))
    with pytest.raises(RuntimeError) as excinfo:
        build_contexts(
            questions,
            FakeSystem(),
            checkpoint=checkpoint2,
            heartbeat=heartbeat2,
            fingerprint={"extraction_model": "sonnet", "split": "s"},
        )

    message = str(excinfo.value)
    assert "extraction_model" in message, "the refusal has to name what actually differs"
    assert "'haiku' -> 'sonnet'" in message


def test_a_resume_under_the_same_configuration_is_allowed(tmp_path: Path) -> None:
    questions = [_question("q1"), _question("q2")]
    fields = {"extraction_model": "haiku", "split": "s"}
    checkpoint, heartbeat = _io(tmp_path, len(questions))
    checkpoint.record("q1", "CONTEXT-FROM-EARLIER-RUN")
    system = FakeSystem()

    contexts = build_contexts(
        questions, system, checkpoint=checkpoint, heartbeat=heartbeat, fingerprint=fields
    )

    assert system.ingested == ["q2"]
    assert len(contexts) == 2
    assert json.loads((tmp_path / "run.json").read_text("utf-8")) == fields
