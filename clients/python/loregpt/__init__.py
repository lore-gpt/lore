"""Lore — coordination memory for multi-agent AI systems.

Create runs, write events, and fetch read-your-writes context packs. See :class:`LoreClient` (sync) and
:class:`AsyncLoreClient` (async).
"""

from ._types import PackResult, PackSource, RunResult, Scopes, WorkingSource, WriteResult
from .client import AsyncLoreClient, LoreClient
from .errors import (
    InvalidBodyError,
    InvalidRunIdError,
    LoreApiError,
    LoreConnectionError,
    LoreError,
    LoreParseError,
    MinSeqOutOfRangeError,
    ModelMismatchError,
    NotFoundError,
    UnauthorizedError,
    UnknownLoreError,
)

__version__ = "0.1.0"

__all__ = [
    "AsyncLoreClient",
    "InvalidBodyError",
    "InvalidRunIdError",
    "LoreApiError",
    "LoreClient",
    "LoreConnectionError",
    "LoreError",
    "LoreParseError",
    "MinSeqOutOfRangeError",
    "ModelMismatchError",
    "NotFoundError",
    "PackResult",
    "PackSource",
    "RunResult",
    "Scopes",
    "UnauthorizedError",
    "UnknownLoreError",
    "WorkingSource",
    "WriteResult",
    "__version__",
]
