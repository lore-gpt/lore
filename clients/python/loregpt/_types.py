"""The public, snake_case result types and aliases. The wire is JSON; the client maps it into these frozen
dataclasses, so callers never see the generated wire types."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Literal

# A pack's scope filter: either a list of scope strings, or a dict flattened to "key:value" strings.
Scopes = list[str] | dict[str, str]

WorkingSource = Literal["live", "durable", "skipped"]


@dataclass(frozen=True, slots=True)
class RunResult:
    run_id: str
    created_at: str


@dataclass(frozen=True, slots=True)
class WriteResult:
    event_id: str
    seq: int


@dataclass(frozen=True, slots=True)
class PackSource:
    id: str
    kind: str
    score: float
    section: str


@dataclass(frozen=True, slots=True)
class PackResult:
    text: str
    sources: list[PackSource]
    covered_seq: int
    freshness_lag_ms: int
    saved_tokens: int
    working_source: WorkingSource
    truncated: bool
