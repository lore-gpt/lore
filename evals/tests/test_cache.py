from pathlib import Path

from longmemeval.cache import CacheKey, JudgeCache, JudgeDecision, hash_answer

_MODEL = "gpt-4o-2024-08-06"


def _key(*, answer: str = "the answer", model: str = _MODEL, rubric: str = "v1", qid: str = "q1") -> CacheKey:
    return CacheKey(question_id=qid, answer_hash=hash_answer(answer), judge_model=model, rubric_version=rubric)


def test_miss_then_hit_round_trips_the_decision(tmp_path: Path) -> None:
    cache = JudgeCache(tmp_path)
    key = _key()
    assert cache.get(key) is None
    decision = JudgeDecision(correct=True, reasoning="semantically matches", judge_model=_MODEL, rubric_version="v1")
    cache.put(key, decision)
    assert cache.get(key) == decision
    assert cache.hits == 1
    assert cache.misses == 1


def test_every_key_field_participates(tmp_path: Path) -> None:
    cache = JudgeCache(tmp_path)
    cache.put(_key(), JudgeDecision(True, "r", _MODEL, "v1"))
    # A change in answer, judge model, rubric version, or question id must all miss.
    assert cache.get(_key(answer="different")) is None
    assert cache.get(_key(model="gpt-4o-mini")) is None
    assert cache.get(_key(rubric="v2")) is None
    assert cache.get(_key(qid="q2")) is None


def test_hit_rate(tmp_path: Path) -> None:
    cache = JudgeCache(tmp_path)
    key = _key()
    assert cache.get(key) is None  # miss
    cache.put(key, JudgeDecision(True, "r", _MODEL, "v1"))
    assert cache.get(key) is not None  # hit
    assert cache.get(key) is not None  # hit
    assert cache.hit_rate == 2 / 3
