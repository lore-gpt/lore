"""The public, snake_case result types and aliases. The wire is JSON; the client maps it into these frozen
dataclasses, so callers never see the generated wire types."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Literal

# A pack's scope filter: either a list of scope strings, or a dict flattened to "key:value" strings.
Scopes = list[str] | dict[str, str]

#: Where the working section came from. "live": the run's live working stripe. "unavailable": the stripe
#: was not authoritative and no durable snapshot existed, so this pack has NO working section (the state
#: facts are still durable and still arrive through the raw tail). "durable": a durable snapshot served it
#: (no producer writes those yet).
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
