from collections.abc import Callable, Sequence
from pathlib import Path

from longmemeval._types import Question, Session, Turn, Verdict
from longmemeval.adapters import MemorySystem
from longmemeval.answerer import ANSWER_SYSTEM, build_answer_prompt
from longmemeval.batch import BatchRequest, BatchStatus
from longmemeval.cache import JudgeCache
from longmemeval.judge import Judge
from longmemeval.runner import (
    run_trial,
    run_trial_batched,
    run_variance_pipeline,
    run_variance_pipeline_batched,
    run_variance_reuse_ingest,
    run_variance_reuse_ingest_batched,
)
from longmemeval.stats import RunStats

# Per-question gold answers, and the retrieved context each question gets. q2's context does NOT contain its
# gold, so q2 judges "no" while q1/q3_abs judge "yes" — the verdict set MIXES correct/incorrect, which makes
# the judge label a load-bearing, per-question-distinguishable field (a mis-join or a batch-only label
# divergence flips a verdict and is caught).
_GOLDS = {"q1": "APPLE", "q2": "BANANA", "q3_abs": "CHERRY"}
_CONTEXTS = {
    "question-q1": "the fruit is APPLE",
    "question-q2": "no fruit mentioned here",  # BANANA absent -> q2 judges "no"
    "question-q3_abs": "the fruit is CHERRY",
}


def _answer_model(system: str, prompt: str) -> str:
    # A deterministic function of the prompt bytes: sync and batch dispatch of the same request are identical.
    return f"ANS[{len(system)}]<{prompt}>"


def _answerer(context: str, question: str, question_date: str) -> str:
    return _answer_model(ANSWER_SYSTEM, build_answer_prompt(context, question, question_date))


def _judge_model(prompt: str) -> str:
    # Mimic the real judge deterministically: correct iff the gold answer appears in the model response. The
    # gold and response are parsed out of the official grading-prompt format, so each question's label depends
    # on THAT question's prompt.
    response = prompt.rsplit("Model Response: ", 1)[-1]
    gold = ""
    for marker in ("Correct Answer: ", "Explanation: "):
        if marker in prompt:
            gold = prompt.split(marker, 1)[1].split("\n", 1)[0]
            break
    return "yes" if gold and gold in response else "no"


def _judge() -> Judge:
    return Judge(complete=_judge_model, model="gpt-4o", rubric_version="v")


class FakeMemorySystem(MemorySystem):
    """A MemorySystem fake: counts ingests and returns the canned per-question context from _CONTEXTS."""

    def __init__(self) -> None:
        self.ingests = 0

    @property
    def name(self) -> str:
        return "fake"

    def ingest(self, sessions: Sequence[Session]) -> None:
        self.ingests += 1

    def retrieve(self, question: str, question_date: str) -> str:
        return _CONTEXTS.get(question, "no context")


class FakeBatch:
    """A BatchProvider fake applying a pure function; `drop` custom_ids are omitted from collect (forcing the
    synchronous complete_one fallback)."""

    def __init__(self, name: str, fn: Callable[[BatchRequest], str], *, drop: tuple[str, ...] = ()) -> None:
        self.name = name
        self._fn = fn
        self._drop = set(drop)
        self._batches: dict[str, list[BatchRequest]] = {}
        self._n = 0

    def submit(self, requests: Sequence[BatchRequest]) -> str:
        self._n += 1
        bid = f"{self.name}-{self._n}"
        self._batches[bid] = list(requests)
        return bid

    def poll(self, batch_id: str) -> BatchStatus:
        return BatchStatus.READY

    def collect(self, batch_id: str) -> dict[str, str]:
        return {r.custom_id: self._fn(r) for r in self._batches[batch_id] if r.custom_id not in self._drop}

    def complete_one(self, request: BatchRequest) -> str:
        return self._fn(request)


def _answer_provider(**kw: object) -> FakeBatch:
    return FakeBatch("answerer", lambda r: _answer_model(r.system, r.prompt), **kw)  # type: ignore[arg-type]


def _judge_provider(**kw: object) -> FakeBatch:
    return FakeBatch("judge", lambda r: _judge_model(r.prompt), **kw)  # type: ignore[arg-type]


def _q(qid: str, qtype: str) -> Question:
    return Question(
        question_id=qid,
        question_type=qtype,
        question=f"question-{qid}",
        answer=_GOLDS[qid],
        question_date="2031/01/01",
        sessions=(Session("s", "2031/01/01", (Turn("user", "hi"),)),),
    )


def _questions() -> list[Question]:
    return [_q("q1", "multi-session"), _q("q2", "temporal-reasoning"), _q("q3_abs", "multi-session")]


def _key(verdicts: list[Verdict]) -> list[tuple[str, str, bool]]:
    return [(v.question_id, v.generated_answer, v.correct) for v in verdicts]


def _correct(verdicts: list[Verdict]) -> list[bool]:
    return [v.correct for v in verdicts]


def test_run_trial_sync(tmp_path: Path) -> None:
    verdicts = run_trial(FakeMemorySystem(), _questions(), _answerer, _judge(), JudgeCache(tmp_path))
    assert [v.question_id for v in verdicts] == ["q1", "q2", "q3_abs"]
    # q1/q3_abs contexts carry their gold -> "yes"; q2's does not -> "no".
    assert _correct(verdicts) == [True, False, True]


def test_reuse_ingest_ingests_once_per_question(tmp_path: Path) -> None:
    system = FakeMemorySystem()
    trials = run_variance_reuse_ingest(system, _questions(), _answerer, _judge(), JudgeCache(tmp_path), n_trials=3)
    assert len(trials) == 3
    assert all(len(t) == 3 for t in trials)
    assert system.ingests == 3  # ONE ingest per question, reused across the 3 trials (not 9)


def test_pipeline_reingests_every_trial(tmp_path: Path) -> None:
    system = FakeMemorySystem()
    trials = run_variance_pipeline(system, _questions(), _answerer, _judge(), JudgeCache(tmp_path), n_trials=3)
    assert len(trials) == 3
    assert system.ingests == 9  # full re-ingest each trial: 3 questions x 3 trials


def test_batched_trial_three_phases(tmp_path: Path) -> None:
    verdicts = run_trial_batched(
        FakeMemorySystem(),
        _questions(),
        _answer_provider(),
        _judge(),
        _judge_provider(),
        JudgeCache(tmp_path),
        sleep=lambda s: None,
    )
    assert [v.question_id for v in verdicts] == ["q1", "q2", "q3_abs"]
    assert _correct(verdicts) == [True, False, True]


def test_judge_batch_joins_each_verdict_to_the_right_question(tmp_path: Path) -> None:
    # The custom_id join is load-bearing (neither API preserves order). With per-question distinct labels, a
    # mis-join would flip q2's verdict; assert every verdict is keyed to its own question.
    verdicts = run_trial_batched(
        FakeMemorySystem(),
        _questions(),
        _answer_provider(),
        _judge(),
        _judge_provider(),
        JudgeCache(tmp_path),
        sleep=lambda s: None,
    )
    by_id = {v.question_id: v.correct for v in verdicts}
    assert by_id == {"q1": True, "q2": False, "q3_abs": True}


def test_reuse_ingest_batched_ingests_once_and_matches_sync(tmp_path: Path) -> None:
    questions = _questions()
    system = FakeMemorySystem()
    batched = run_variance_reuse_ingest_batched(
        system,
        questions,
        _answer_provider(),
        _judge(),
        _judge_provider(),
        JudgeCache(tmp_path / "b"),
        n_trials=2,
        sleep=lambda s: None,
    )
    assert system.ingests == 3  # ingest ONCE per question, reused across the 2 batched trials
    assert len(batched) == 2
    sync = run_variance_reuse_ingest(
        FakeMemorySystem(), questions, _answerer, _judge(), JudgeCache(tmp_path / "s"), n_trials=2
    )
    assert [_key(t) for t in batched] == [_key(t) for t in sync]


def test_pipeline_batched_matches_sync_and_reingests(tmp_path: Path) -> None:
    questions = _questions()
    system = FakeMemorySystem()
    batched = run_variance_pipeline_batched(
        system,
        questions,
        _answer_provider(),
        _judge(),
        _judge_provider(),
        JudgeCache(tmp_path / "b"),
        n_trials=2,
        sleep=lambda s: None,
    )
    assert system.ingests == 6  # full re-ingest each trial: 3 x 2
    sync = run_variance_pipeline(
        FakeMemorySystem(), questions, _answerer, _judge(), JudgeCache(tmp_path / "s"), n_trials=2
    )
    assert [_key(t) for t in batched] == [_key(t) for t in sync]


def test_sync_and_batch_produce_identical_verdicts(tmp_path: Path) -> None:
    questions = _questions()
    sync = run_trial(FakeMemorySystem(), questions, _answerer, _judge(), JudgeCache(tmp_path / "sync"))
    batched = run_trial_batched(
        FakeMemorySystem(),
        questions,
        _answer_provider(),
        _judge(),
        _judge_provider(),
        JudgeCache(tmp_path / "batch"),
        sleep=lambda s: None,
    )
    # The verdict set mixes correct/incorrect, so `correct` is load-bearing: a judge-path label divergence
    # between the two dispatch modes would break this equality, not just an answer-path one.
    assert _correct(sync) == [True, False, True]
    assert _key(sync) == _key(batched)


def test_stats_count_sync_vs_batch_calls_mutually_exclusive(tmp_path: Path) -> None:
    sync_stats = RunStats()
    run_trial(FakeMemorySystem(), _questions(), _answerer, _judge(), JudgeCache(tmp_path / "s"), stats=sync_stats)
    assert sync_stats.answerer_sync_calls == 3
    assert sync_stats.answerer_batch_calls == 0
    assert sync_stats.judge_sync_calls == 3
    assert sync_stats.judge_batch_calls == 0  # the split is mutually exclusive

    batch_stats = RunStats()
    run_trial_batched(
        FakeMemorySystem(),
        _questions(),
        _answer_provider(),
        _judge(),
        _judge_provider(),
        JudgeCache(tmp_path / "b"),
        stats=batch_stats,
        sleep=lambda s: None,
    )
    assert batch_stats.answerer_batch_calls == 3
    assert batch_stats.judge_batch_calls == 3
    assert batch_stats.answerer_sync_calls == 0
    assert batch_stats.judge_sync_calls == 0


def test_batch_drop_counts_as_sync_not_batch(tmp_path: Path) -> None:
    # An item the batch drops is filled by the full-rate synchronous complete_one — so it must be counted as a
    # sync call, or the reported economy split overstates the batch savings.
    stats = RunStats()
    run_trial_batched(
        FakeMemorySystem(),
        _questions(),
        _answer_provider(drop=("q2",)),
        _judge(),
        _judge_provider(drop=("q1",)),
        JudgeCache(tmp_path),
        stats=stats,
        sleep=lambda s: None,
    )
    assert stats.answerer_batch_calls == 2 and stats.answerer_sync_calls == 1  # q2 fell back to sync
    assert stats.judge_batch_calls == 2 and stats.judge_sync_calls == 1  # q1 fell back to sync


def test_variance_rejudges_each_trial_then_a_rerun_hits_cache(tmp_path: Path) -> None:
    cache = JudgeCache(tmp_path)
    stats = RunStats()
    # Fixed ingestion + deterministic answerer -> identical answers every trial. A trial-aware cache variant is
    # what lets the variance actually SAMPLE: each of the 3 trials re-judges (else trial 1's decision would be
    # served to trials 2-3 and the std would collapse to 0 — a vacuous gate).
    one = [_q("q1", "multi-session")]
    run_variance_reuse_ingest(FakeMemorySystem(), one, _answerer, _judge(), cache, 3, stats=stats)
    assert stats.judge_sync_calls == 3
    assert stats.cache_hits == 0

    # A RE-RUN (same trials, same cache) is served entirely from cache — the re-run economy the cache exists for.
    rerun = RunStats()
    run_variance_reuse_ingest(
        FakeMemorySystem(), [_q("q1", "multi-session")], _answerer, _judge(), cache, 3, stats=rerun
    )
    assert rerun.judge_sync_calls == 0
    assert rerun.cache_hits == 3
