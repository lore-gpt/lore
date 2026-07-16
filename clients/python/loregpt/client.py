"""The Lore clients. LoreClient (sync) and AsyncLoreClient (async) share the same facade — create_run, write,
write_state, pack — differing only by ``await``. Request building, response parsing, and error mapping live in
_wire (pure functions), so each client is a thin shell over httpx.Client / httpx.AsyncClient. Requests are
not retried (a write is not idempotent); failures raise a typed LoreError."""

from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, overload

import httpx

if TYPE_CHECKING:
    from types import TracebackType

from ._types import PackResult, RunResult, Scopes, WriteResult
from ._wire import (
    decode_response,
    event_request,
    pack_request,
    parse_pack,
    parse_run,
    parse_write,
    run_request,
    state_request,
)
from .errors import LoreConnectionError

DEFAULT_BASE_URL = "http://localhost:8080"
_DEFAULT_TIMEOUT = httpx.Timeout(connect=10.0, read=30.0, write=30.0, pool=5.0)
_DEFAULT_LIMITS = httpx.Limits(max_connections=100, max_keepalive_connections=20)


def _headers(api_key: str, extra: Mapping[str, str] | None) -> dict[str, str]:
    # Extra headers first, so the SDK's auth and content-type always win (never accidentally overridden).
    return {**(extra or {}), "authorization": f"Bearer {api_key}", "content-type": "application/json"}


class LoreClient:
    """Synchronous client for the Lore coordination-memory API."""

    def __init__(
        self,
        api_key: str,
        *,
        base_url: str = DEFAULT_BASE_URL,
        timeout: httpx.Timeout | float | None = None,
        headers: Mapping[str, str] | None = None,
        transport: httpx.BaseTransport | None = None,
    ) -> None:
        if not api_key:
            raise ValueError("LoreClient: api_key is required")
        self._client = httpx.Client(
            base_url=base_url.rstrip("/"),
            headers=_headers(api_key, headers),
            timeout=_DEFAULT_TIMEOUT if timeout is None else timeout,
            limits=_DEFAULT_LIMITS,
            transport=transport,
        )

    def create_run(self) -> RunResult:
        """Create a run in the API key's project."""
        path, body = run_request()
        return parse_run(self._send(path, body))

    @overload
    def write(self, *, run_id: str, agent_id: str, content: str) -> WriteResult: ...
    @overload
    def write(self, *, run_id: str, agent_id: str, payload: dict[str, Any]) -> WriteResult: ...
    def write(
        self,
        *,
        run_id: str,
        agent_id: str,
        content: str | None = None,
        payload: dict[str, Any] | None = None,
    ) -> WriteResult:
        """Append an event to a run. Pass exactly one of ``content`` (a str, wrapped as ``{"content": ...}``)
        or ``payload`` (an opaque dict sent verbatim); passing both or neither raises ValueError."""
        path, body = event_request(run_id, agent_id, content, payload)
        return parse_write(self._send(path, body))

    def write_state(self, *, run_id: str, agent_id: str, entity: str, predicate: str, value: object) -> WriteResult:
        """Write one working-memory fact (the ``kind:"state"`` convention), seen immediately by a same-run reader."""
        path, body = state_request(run_id, agent_id, entity, predicate, value)
        return parse_write(self._send(path, body))

    def pack(
        self,
        *,
        run_id: str,
        query: str,
        min_seq: int = 0,
        scopes: Scopes | None = None,
        limit: int | None = None,
        token_budget: int | None = None,
    ) -> PackResult:
        """Retrieve a context pack for a run. ``min_seq`` asserts read-your-writes."""
        path, body = pack_request(run_id, query, min_seq, scopes, limit, token_budget)
        return parse_pack(self._send(path, body))

    def _send(self, path: str, body: Mapping[str, object]) -> Any:
        try:
            resp = self._client.post(path, json=body)
        except httpx.RequestError as exc:
            raise LoreConnectionError("request did not reach a response") from exc
        return decode_response(resp.status_code, resp.text)

    def close(self) -> None:
        self._client.close()

    def __enter__(self) -> LoreClient:
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc_val: BaseException | None,
        exc_tb: TracebackType | None,
    ) -> None:
        self.close()


class AsyncLoreClient:
    """Asynchronous client for the Lore coordination-memory API — the same facade as LoreClient, with await."""

    def __init__(
        self,
        api_key: str,
        *,
        base_url: str = DEFAULT_BASE_URL,
        timeout: httpx.Timeout | float | None = None,
        headers: Mapping[str, str] | None = None,
        transport: httpx.AsyncBaseTransport | None = None,
    ) -> None:
        if not api_key:
            raise ValueError("AsyncLoreClient: api_key is required")
        self._client = httpx.AsyncClient(
            base_url=base_url.rstrip("/"),
            headers=_headers(api_key, headers),
            timeout=_DEFAULT_TIMEOUT if timeout is None else timeout,
            limits=_DEFAULT_LIMITS,
            transport=transport,
        )

    async def create_run(self) -> RunResult:
        path, body = run_request()
        return parse_run(await self._send(path, body))

    @overload
    async def write(self, *, run_id: str, agent_id: str, content: str) -> WriteResult: ...
    @overload
    async def write(self, *, run_id: str, agent_id: str, payload: dict[str, Any]) -> WriteResult: ...
    async def write(
        self,
        *,
        run_id: str,
        agent_id: str,
        content: str | None = None,
        payload: dict[str, Any] | None = None,
    ) -> WriteResult:
        path, body = event_request(run_id, agent_id, content, payload)
        return parse_write(await self._send(path, body))

    async def write_state(
        self, *, run_id: str, agent_id: str, entity: str, predicate: str, value: object
    ) -> WriteResult:
        path, body = state_request(run_id, agent_id, entity, predicate, value)
        return parse_write(await self._send(path, body))

    async def pack(
        self,
        *,
        run_id: str,
        query: str,
        min_seq: int = 0,
        scopes: Scopes | None = None,
        limit: int | None = None,
        token_budget: int | None = None,
    ) -> PackResult:
        path, body = pack_request(run_id, query, min_seq, scopes, limit, token_budget)
        return parse_pack(await self._send(path, body))

    async def _send(self, path: str, body: Mapping[str, object]) -> Any:
        try:
            resp = await self._client.post(path, json=body)
        except httpx.RequestError as exc:
            raise LoreConnectionError("request did not reach a response") from exc
        return decode_response(resp.status_code, resp.text)

    async def aclose(self) -> None:
        await self._client.aclose()

    async def __aenter__(self) -> AsyncLoreClient:
        return self

    async def __aexit__(
        self,
        exc_type: type[BaseException] | None,
        exc_val: BaseException | None,
        exc_tb: TracebackType | None,
    ) -> None:
        await self.aclose()
