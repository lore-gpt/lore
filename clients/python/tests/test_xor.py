from __future__ import annotations

import pytest

from loregpt import LoreClient
from tests.conftest import mock_transport


def _client() -> LoreClient:
    return LoreClient("k", transport=mock_transport(202, {"event_id": "e", "seq": 1}))


def test_write_with_both_content_and_payload_raises() -> None:
    with pytest.raises(ValueError, match=r"content.*payload"):
        _client().write(run_id="r", agent_id="a", content="x", payload={"k": 1})  # type: ignore[call-overload]


def test_write_with_neither_raises() -> None:
    with pytest.raises(ValueError, match=r"content.*payload"):
        _client().write(run_id="r", agent_id="a")  # type: ignore[call-overload]
