"""The public, snake_case result types and aliases. The wire is JSON; the client maps it into these frozen
dataclasses, so callers never see the generated wire types."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Literal

# A pack's scope filter: either a list of scope strings, or a dict flattened to "key:value" strings.
Scopes = list[str] | dict[str, str]

#: Always "live". The working section is served from the run's durably stored working facts, read in the
#: same transaction as the rest of the pack, so it is always the run's current state and there is nothing
#: left to choose between. The literal is unchanged for wire compatibility, but "durable" and "unavailable"
#: are no longer returned. An empty working section means the run has written no state facts — not that
#: anything is degraded.
WorkingSource = Literal["live", "durable", "unavailable"]


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
    #: Retrieval legs that missed the partial-result budget and contributed nothing to this pack —
    #: today only "dense", when the embedding provider answered slower than the server's budget.
    #: Empty when every leg finished, so a non-empty value means this answer was assembled from
    #: fewer sources than the server has configured (the call still succeeded, the pack is narrower).
    degraded: tuple[str, ...] = ()
