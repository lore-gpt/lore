"""Pure request-building, response-parsing, and error-mapping shared by the sync and async clients, so the
only per-client code is the two-line HTTP send. camelCase does not apply here — the wire is snake_case and so
is the public surface, so most fields are 1:1; this module still owns the payload wrapping, scope flattening,
and status/JSON handling."""

from __future__ import annotations

import json
from collections.abc import Mapping
from typing import TYPE_CHECKING, Any

from ._types import PackResult, PackSource, RunResult, Scopes, WriteResult
from .errors import LoreParseError, from_response

# The generated wire module is imported ONLY here, under TYPE_CHECKING — it is never a runtime import, so its
# typing_extensions dependency never becomes a runtime requirement (which would break the zero-extra-dep story
# on Python 3.10). Every annotation below is a string (PEP 563 via `from __future__ import annotations`), so
# these names are used by mypy but never evaluated at runtime.
if TYPE_CHECKING:
    from ._generated.wire import (
        CreateEventRequest,
        CreateEventResponse,
        CreateRunResponse,
        PackRequest,
        PackResponse,
    )
    from ._generated.wire import Error as WireError

RUNS_PATH = "/v1/runs"
EVENTS_PATH = "/v1/events"
PACK_PATH = "/v1/pack"


def run_request() -> tuple[str, Mapping[str, object]]:
    return RUNS_PATH, {}


def event_request(
    run_id: str, agent_id: str, content: str | None, payload: dict[str, Any] | None
) -> tuple[str, Mapping[str, object]]:
    if (content is None) == (payload is None):
        raise ValueError("write() takes exactly one of `content` or `payload` (received both or neither)")
    if content is not None:
        p: dict[str, Any] = {"content": content}
    else:
        assert payload is not None  # guaranteed by the exactly-one check above
        p = payload
    body: CreateEventRequest = {"run_id": run_id, "agent_id": agent_id, "payload": p}
    return EVENTS_PATH, body


def state_request(
    run_id: str, agent_id: str, entity: str, predicate: str, value: object
) -> tuple[str, Mapping[str, object]]:
    body: CreateEventRequest = {
        "run_id": run_id,
        "agent_id": agent_id,
        "payload": {"kind": "state", "entity": entity, "predicate": predicate, "value": value},
    }
    return EVENTS_PATH, body


def pack_request(
    run_id: str,
    query: str,
    min_seq: int,
    scopes: Scopes | None,
    limit: int | None,
    token_budget: int | None,
) -> tuple[str, Mapping[str, object]]:
    body: PackRequest = {"run_id": run_id, "query": query, "min_seq": min_seq}
    if scopes is not None:
        body["scopes"] = normalize_scopes(scopes)
    if limit is not None:
        body["limit"] = limit
    if token_budget is not None:
        body["token_budget"] = token_budget
    return PACK_PATH, body


def normalize_scopes(scopes: Scopes) -> list[str]:
    if isinstance(scopes, dict):
        return [f"{k}:{v}" for k, v in scopes.items()]
    return list(scopes)


def parse_run(data: Any) -> RunResult:
    b: CreateRunResponse = data
    return RunResult(run_id=b["run_id"], created_at=b["created_at"])


def parse_write(data: Any) -> WriteResult:
    b: CreateEventResponse = data
    return WriteResult(event_id=b["event_id"], seq=b["seq"])


def parse_pack(data: Any) -> PackResult:
    b: PackResponse = data
    return PackResult(
        text=b["text"],
        sources=[
            PackSource(id=s["id"], kind=s["kind"], score=s["score"], section=s["section"]) for s in b["sources"]
        ],
        covered_seq=b["covered_seq"],
        freshness_lag_ms=b["freshness_lag_ms"],
        saved_tokens=b["saved_tokens"],
        working_source=b["working_source"],
        truncated=b["truncated"],
    )


def decode_response(status: int, text: str) -> Any:
    """Parse a response body and either return it (2xx) or raise a typed error. A non-JSON body on an error
    status is still a typed API error (synthesized message), NOT a parse error — LoreParseError is only for a
    2xx that failed to parse."""
    parsed: Any = None
    if text:
        try:
            parsed = json.loads(text)
        except json.JSONDecodeError as exc:
            if status >= 400:
                raise from_response(status, {"message": f"HTTP {status}"}) from exc
            raise LoreParseError(f"response body was not JSON (status {status})") from exc
    if status >= 400:
        raise from_response(status, _to_wire_error(parsed, status))
    return parsed


def _to_wire_error(parsed: object, status: int) -> WireError:
    if isinstance(parsed, dict):
        msg = parsed.get("message")
        out: WireError = {"message": msg if isinstance(msg, str) else f"HTTP {status}"}
        code = parsed.get("code")
        if isinstance(code, str):
            out["code"] = code
        return out
    return {"message": f"HTTP {status}"}
