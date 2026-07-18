from typing import Any

import pytest

from longmemeval._types import Session, Turn
from longmemeval.adapters.mem0 import Mem0Adapter


class FakeMem0:
    """A Mem0Like fake mirroring the OSS mem0.Memory 2.x surface: add() takes user_id only (NOT the Platform-
    only `timestamp`, which the real OSS class rejects), search() takes filters+top_k. No key, no vector store."""

    def __init__(self, results: list[dict[str, Any]] | None = None) -> None:
        self.adds: list[tuple[list[dict[str, str]], str]] = []
        self.searches: list[tuple[str, dict[str, Any], int]] = []
        self._results = results or []

    def add(self, messages: list[dict[str, str]], *, user_id: str) -> dict[str, Any]:
        self.adds.append((messages, user_id))
        return {"results": [{"memory": "m", "event": "ADD"}]}

    def search(self, query: str, *, filters: dict[str, Any], top_k: int = 20) -> dict[str, Any]:
        self.searches.append((query, filters, top_k))
        return {"results": self._results}


def _sessions() -> list[Session]:
    return [
        Session("s1", "2023/05/20 (Sat) 02:21", (Turn("user", "I moved to Berlin"), Turn("assistant", "Nice!"))),
        Session("s2", "", (Turn("user", "  "), Turn("user", "I love jazz"))),  # blank turn is skipped
    ]


def test_ingest_adds_one_call_per_session_without_a_timestamp_kwarg() -> None:
    fake = FakeMem0()
    Mem0Adapter(fake).ingest(_sessions())
    assert len(fake.adds) == 2  # one add per non-empty session
    messages0, user_id0 = fake.adds[0]
    # add() carries user_id (the 2.x shape) and NO timestamp — that Platform-only kwarg crashes the OSS class;
    # the session date rides the message text instead.
    assert user_id0.startswith("lme-")
    assert messages0[0]["content"].startswith("[2023/05/20 (Sat) 02:21] ")
    # The blank-content turn in session 2 is dropped; only the real one is added.
    messages1, _ = fake.adds[1]
    assert [m["content"] for m in messages1] == ["I love jazz"]


def test_each_ingest_uses_a_fresh_user_id_for_isolation() -> None:
    fake = FakeMem0()
    adapter = Mem0Adapter(fake)
    adapter.ingest(_sessions())
    first_uid = fake.adds[-1][1]
    adapter.ingest(_sessions())
    second_uid = fake.adds[-1][1]
    assert first_uid != second_uid  # re-ingest (pipeline variance) starts a clean memory scope


def test_retrieve_uses_2x_filters_and_top_k_and_orders_by_created_at() -> None:
    results = [
        {"memory": "later fact", "created_at": "2023-06-01T00:00:00Z"},
        {"memory": "earlier fact", "created_at": "2023-05-01T00:00:00Z"},
        {"memory": "", "created_at": "2023-05-15T00:00:00Z"},  # empty memory is dropped
    ]
    fake = FakeMem0(results)
    adapter = Mem0Adapter(fake, top_k=7)
    adapter.ingest(_sessions())
    context = adapter.retrieve("where do I live?", "2023/07/01")
    query, filters, top_k = fake.searches[-1]
    assert query == "where do I live?"
    assert filters == {"user_id": fake.adds[-1][1]}  # 2.x: user_id lives in filters, not a positional
    assert top_k == 7
    # Oldest-first, empties dropped.
    assert context == "earlier fact\nlater fact"


def test_top_k_is_exposed_for_the_fairness_record() -> None:
    assert Mem0Adapter(FakeMem0(), top_k=11).top_k == 11


def test_retrieve_before_ingest_raises() -> None:
    with pytest.raises(RuntimeError):
        Mem0Adapter(FakeMem0()).retrieve("q", "d")
