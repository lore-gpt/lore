from __future__ import annotations

import asyncio
import http.server
import socketserver
import threading
import time
from typing import Any

import httpx
import pytest

from loregpt import (
    AsyncLoreClient,
    InvalidRunIdError,
    LoreApiError,
    LoreClient,
    LoreConnectionError,
    LoreParseError,
    MinSeqOutOfRangeError,
    ModelMismatchError,
    NotFoundError,
    PackResult,
    PackSource,
    RunResult,
    UnauthorizedError,
    UnknownLoreError,
    WriteResult,
)
from tests.conftest import Recorder, mock_transport, raising_transport, routing_transport

_PACK_RESPONSE: dict[str, Any] = {
    "text": "PACK",
    "sources": [{"id": "m1", "kind": "semantic", "score": 0.9, "section": "distilled"}],
    "covered_seq": 5,
    "freshness_lag_ms": 12,
    "saved_tokens": 100,
    "working_source": "live",
    "truncated": False,
}


def _client(transport: httpx.MockTransport) -> LoreClient:
    return LoreClient("lore_sk_test", transport=transport)


def test_create_run_maps_and_hits_runs() -> None:
    rec = Recorder()
    res = _client(mock_transport(201, {"run_id": "run-1", "created_at": "2026-07-16T00:00:00Z"}, rec=rec)).create_run()
    assert res == RunResult(run_id="run-1", created_at="2026-07-16T00:00:00Z")
    assert rec.last["method"] == "POST"
    assert rec.last["path"] == "/v1/runs"
    assert rec.last["json"] == {}
    assert rec.last["headers"]["authorization"] == "Bearer lore_sk_test"
    assert rec.last["headers"]["content-type"] == "application/json"


def test_write_content_wraps_payload() -> None:
    rec = Recorder()
    res = _client(mock_transport(202, {"event_id": "ev-1", "seq": 1}, rec=rec)).write(
        run_id="r", agent_id="researcher", content="hello"
    )
    assert res == WriteResult(event_id="ev-1", seq=1)
    assert rec.last["path"] == "/v1/events"
    assert rec.last["json"] == {"run_id": "r", "agent_id": "researcher", "payload": {"content": "hello"}}


def test_write_payload_verbatim() -> None:
    rec = Recorder()
    _client(mock_transport(202, {"event_id": "ev", "seq": 2}, rec=rec)).write(
        run_id="r", agent_id="a", payload={"note": "x", "n": 3}
    )
    assert rec.last["json"] == {"run_id": "r", "agent_id": "a", "payload": {"note": "x", "n": 3}}


def test_write_state_builds_state_payload() -> None:
    rec = Recorder()
    res = _client(mock_transport(202, {"event_id": "ev", "seq": 3}, rec=rec)).write_state(
        run_id="r", agent_id="a", entity="auth-service", predicate="status", value="up"
    )
    assert res == WriteResult(event_id="ev", seq=3)
    assert rec.last["json"] == {
        "run_id": "r",
        "agent_id": "a",
        "payload": {"kind": "state", "entity": "auth-service", "predicate": "status", "value": "up"},
    }


def test_pack_maps_request_and_response_and_always_sends_min_seq() -> None:
    rec = Recorder()
    res = _client(mock_transport(200, _PACK_RESPONSE, rec=rec)).pack(run_id="r", query="auth", token_budget=2000)
    assert res == PackResult(
        text="PACK",
        sources=[PackSource(id="m1", kind="semantic", score=0.9, section="distilled")],
        covered_seq=5,
        freshness_lag_ms=12,
        saved_tokens=100,
        working_source="live",
        truncated=False,
    )
    # min_seq is always sent (0 when omitted); scopes/limit omitted are absent from the wire body.
    assert rec.last["json"] == {"run_id": "r", "query": "auth", "min_seq": 0, "token_budget": 2000}


def test_pack_scopes_list_verbatim_and_dict_flattened() -> None:
    rec = Recorder()
    client = _client(mock_transport(200, _PACK_RESPONSE, rec=rec))
    client.pack(run_id="r", query="q", scopes=["team:a", "b"], min_seq=7)
    assert rec.last["json"] == {"run_id": "r", "query": "q", "min_seq": 7, "scopes": ["team:a", "b"]}
    client.pack(run_id="r", query="q", scopes={"team": "platform", "tier": "gold"})
    assert rec.last["json"] == {
        "run_id": "r",
        "query": "q",
        "min_seq": 0,
        "scopes": ["team:platform", "tier:gold"],
    }


@pytest.mark.parametrize(
    ("status", "code", "cls"),
    [
        (400, "invalid_run_id", InvalidRunIdError),
        (400, "min_seq_out_of_range", MinSeqOutOfRangeError),
        (401, "unauthorized", UnauthorizedError),
        (404, "not_found", NotFoundError),
        (409, "model_mismatch", ModelMismatchError),
    ],
)
def test_error_codes_map_to_typed_exceptions(status: int, code: str, cls: type[LoreApiError]) -> None:
    client = _client(mock_transport(status, {"code": code, "message": f"{code} happened"}))
    with pytest.raises(cls) as excinfo:
        client.create_run()
    exc = excinfo.value
    assert isinstance(exc, LoreApiError)
    assert exc.code == code
    assert exc.http_status == status


def test_missing_or_unknown_code_becomes_unknown() -> None:
    with pytest.raises(UnknownLoreError) as excinfo:
        _client(mock_transport(500, {"message": "boom"})).create_run()
    assert excinfo.value.http_status == 500
    assert excinfo.value.raw_code is None

    with pytest.raises(UnknownLoreError) as excinfo:
        _client(mock_transport(418, {"code": "future_code", "message": "later"})).create_run()
    assert excinfo.value.raw_code == "future_code"


def test_transport_failure_and_non_json_body() -> None:
    with pytest.raises(LoreConnectionError):
        _client(raising_transport(httpx.ConnectError("refused"))).create_run()
    with pytest.raises(LoreParseError):
        _client(mock_transport(200, raw="<html>not json</html>")).create_run()


def test_post_is_not_retried_on_500() -> None:
    rec = Recorder()
    with pytest.raises(LoreApiError):
        _client(mock_transport(500, {"code": "internal", "message": "boom"}, rec=rec)).write(
            run_id="r", agent_id="a", content="x"
        )
    assert len(rec.calls) == 1


def test_trailing_slash_base_url_is_normalized() -> None:
    rec = Recorder()
    transport = mock_transport(201, {"run_id": "r", "created_at": "t"}, rec=rec)
    LoreClient("k", base_url="http://example.test:9000/", transport=transport).create_run()
    assert rec.last["path"] == "/v1/runs"


def test_empty_api_key_raises() -> None:
    with pytest.raises(ValueError, match="api_key"):
        LoreClient("")
    with pytest.raises(ValueError, match="api_key"):
        AsyncLoreClient("")


def test_generated_wire_is_not_imported_at_runtime() -> None:
    import sys

    # The generated wire module is TYPE_CHECKING-only, so using the client must not import it — otherwise its
    # typing_extensions dependency would silently become a runtime requirement, breaking on Python 3.10.
    sys.modules.pop("loregpt._generated.wire", None)
    _client(mock_transport(201, {"run_id": "r", "created_at": "t"})).create_run()
    assert "loregpt._generated.wire" not in sys.modules


def test_async_client_parity() -> None:
    rec = Recorder()
    routes: dict[str, tuple[int, Any]] = {
        "/v1/runs": (201, {"run_id": "r", "created_at": "t"}),
        "/v1/events": (202, {"event_id": "e", "seq": 1}),
        "/v1/pack": (200, _PACK_RESPONSE),
    }

    async def body() -> tuple[RunResult, WriteResult, PackResult]:
        async with AsyncLoreClient("lore_sk_test", transport=routing_transport(routes, rec=rec)) as client:
            run = await client.create_run()
            write = await client.write(run_id="r", agent_id="a", content="hi")
            pack = await client.pack(run_id="r", query="q", min_seq=1)
            return run, write, pack

    run, write, pack = asyncio.run(body())
    # The async client parses each response and hits the right paths with the right bodies (parity with sync).
    assert run == RunResult(run_id="r", created_at="t")
    assert write == WriteResult(event_id="e", seq=1)
    assert pack.covered_seq == 5
    assert [c["path"] for c in rec.calls] == ["/v1/runs", "/v1/events", "/v1/pack"]
    assert rec.calls[1]["json"] == {"run_id": "r", "agent_id": "a", "payload": {"content": "hi"}}
    assert rec.calls[2]["json"] == {"run_id": "r", "query": "q", "min_seq": 1}


def test_async_error_mapping() -> None:
    async def body() -> None:
        transport = mock_transport(404, {"code": "not_found", "message": "nope"})
        async with AsyncLoreClient("k", transport=transport) as client:
            await client.create_run()

    with pytest.raises(NotFoundError):
        asyncio.run(body())


def test_custom_timeout_aborts_a_slow_request() -> None:
    class _Slow(http.server.BaseHTTPRequestHandler):
        def do_POST(self) -> None:
            time.sleep(2.0)
            self.send_response(201)
            self.end_headers()
            self.wfile.write(b"{}")

        def log_message(self, *args: Any) -> None:
            pass

    server = socketserver.TCPServer(("127.0.0.1", 0), _Slow)
    port = server.server_address[1]
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    client = LoreClient("k", base_url=f"http://127.0.0.1:{port}", timeout=httpx.Timeout(0.3))
    try:
        with pytest.raises(LoreConnectionError):
            client.create_run()
    finally:
        client.close()
        server.shutdown()
        server.server_close()
