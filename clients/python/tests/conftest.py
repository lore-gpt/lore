"""Test helpers: a recording httpx.MockTransport, the only test seam (no real network for the mapping tests).
It doubles as the SDK's real ``transport`` option, so tests exercise the public injection point."""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any

import httpx


@dataclass
class Recorder:
    calls: list[dict[str, Any]] = field(default_factory=list)

    @property
    def last(self) -> dict[str, Any]:
        return self.calls[-1]


def mock_transport(
    status: int,
    body: Any = None,
    *,
    raw: str | None = None,
    rec: Recorder | None = None,
) -> httpx.MockTransport:
    """A MockTransport that records each request (method/url/path/headers/json body) and returns a canned
    response: ``raw`` sends a literal (possibly non-JSON) body, ``body`` sends JSON, otherwise an empty body."""

    def handler(request: httpx.Request) -> httpx.Response:
        if rec is not None:
            rec.calls.append(
                {
                    "method": request.method,
                    "path": request.url.path,
                    "headers": dict(request.headers),
                    "json": json.loads(request.content) if request.content else None,
                }
            )
        if raw is not None:
            return httpx.Response(status, text=raw)
        if body is None:
            return httpx.Response(status)
        return httpx.Response(status, json=body)

    return httpx.MockTransport(handler)


def raising_transport(exc: Exception) -> httpx.MockTransport:
    """A transport whose handler raises — for the connection-failure path (httpx.RequestError)."""

    def handler(request: httpx.Request) -> httpx.Response:
        raise exc

    return httpx.MockTransport(handler)


def routing_transport(
    routes: dict[str, tuple[int, Any]], *, rec: Recorder | None = None
) -> httpx.MockTransport:
    """A transport that returns a different (status, JSON body) per request path — for exercising a full
    create_run -> write -> pack sequence over one client."""

    def handler(request: httpx.Request) -> httpx.Response:
        if rec is not None:
            rec.calls.append(
                {
                    "method": request.method,
                    "path": request.url.path,
                    "headers": dict(request.headers),
                    "json": json.loads(request.content) if request.content else None,
                }
            )
        status, body = routes[request.url.path]
        return httpx.Response(status, json=body)

    return httpx.MockTransport(handler)
