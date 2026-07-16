"""The exceptions the SDK raises. Every failure is a LoreError; server failures are keyed by the API's
machine ``code`` (discriminate on the concrete class or ``.code``, never string-match a message)."""

from __future__ import annotations

from collections.abc import Callable
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from ._generated.wire import Error as WireError


class LoreError(Exception):
    """Base for everything the SDK raises."""

    code: str


class LoreApiError(LoreError):
    """Base for an error the server returned, carrying the HTTP status."""

    def __init__(self, message: str, http_status: int) -> None:
        super().__init__(message)
        self.http_status = http_status


class InvalidBodyError(LoreApiError):
    code = "invalid_body"


class InvalidRunIdError(LoreApiError):
    code = "invalid_run_id"


class MinSeqOutOfRangeError(LoreApiError):
    code = "min_seq_out_of_range"


class NotFoundError(LoreApiError):
    code = "not_found"


class UnauthorizedError(LoreApiError):
    code = "unauthorized"


class ModelMismatchError(LoreApiError):
    code = "model_mismatch"


class UnknownLoreError(LoreApiError):
    """A server error whose ``code`` the SDK does not model — a new code, or an absent one (the API's error
    schema makes ``code`` optional). Keyed by HTTP status; the raw code, if any, is on ``raw_code``."""

    code = "unknown"

    def __init__(self, message: str, http_status: int, raw_code: str | None = None) -> None:
        super().__init__(message, http_status)
        self.raw_code = raw_code


class LoreConnectionError(LoreError):
    """The request never reached a response (network failure, timeout, aborted)."""

    code = "connection"


class LoreParseError(LoreError):
    """The server responded, but the body was not the JSON the SDK expected."""

    code = "parse"


_KNOWN: dict[str, Callable[[str, int], LoreApiError]] = {
    "invalid_body": InvalidBodyError,
    "invalid_run_id": InvalidRunIdError,
    "min_seq_out_of_range": MinSeqOutOfRangeError,
    "not_found": NotFoundError,
    "unauthorized": UnauthorizedError,
    "model_mismatch": ModelMismatchError,
}


def from_response(status: int, body: WireError) -> LoreApiError:
    """Map a server error response (HTTP status + ``{message, code?}``) to the right typed error. A missing or
    unmodelled code becomes an :class:`UnknownLoreError` keyed by status — the SDK never guesses a code."""
    message = body.get("message") or f"HTTP {status}"
    raw = body.get("code")
    ctor = _KNOWN.get(raw) if raw is not None else None
    if ctor is not None:
        return ctor(message, status)
    return UnknownLoreError(message, status, raw)
