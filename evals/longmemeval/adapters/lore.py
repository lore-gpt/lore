"""The Lore adapter: drive the Lore server as a memory system. Ingest the conversation into a fresh run, wait
for async distillation via Lore's OWN read-your-writes contract (poll `covered_seq` up to the last ingested
seq — not a fixed sleep; the harness dogfoods RYW), then pack context for the question. Answering is a shared
harness step over the returned context, not the adapter's job."""

from __future__ import annotations

import math
import time
from collections.abc import Callable, Sequence
from typing import TYPE_CHECKING, Protocol

from .._types import Session
from .base import MemorySystem

if TYPE_CHECKING:
    from loregpt import PackResult, RunResult, WriteResult

# A settled-memory probe query; its results are ignored — only covered_seq is read.
_PROBE_QUERY = "distillation probe"


class LoreLike(Protocol):
    """The slice of the Lore client the adapter uses. The real loregpt.LoreClient satisfies it structurally
    (wired in the smoke script); tests implement it with a fake, so no server is needed to test the flow."""

    def create_run(self) -> RunResult: ...
    def write(self, *, run_id: str, agent_id: str, content: str) -> WriteResult: ...
    def pack(self, *, run_id: str, query: str, min_seq: int, token_budget: int) -> PackResult: ...


class DistillationTimeout(RuntimeError):
    """Raised when extraction does not catch up to the ingested history within the poll budget."""


class LoreAdapter(MemorySystem):
    def __init__(
        self,
        client: LoreLike,
        *,
        token_budget: int = 2000,
        poll_interval: float = 0.5,
        poll_timeout: float = 60.0,
        sleep: Callable[[float], None] = time.sleep,
    ) -> None:
        """`poll_interval` / `poll_timeout` bound the read-your-writes wait: distillation is polled every
        `poll_interval` seconds for up to `ceil(poll_timeout / poll_interval)` attempts (rounded up so the wait
        never undershoots the requested timeout). When the project runs Lore's economy (batched) extraction
        mode — the dogfooded full-run path — distillation lands on a batch cadence, so a caller should raise
        `poll_timeout` accordingly. `sleep` is injected so tests drive the poll deterministically; it defaults
        to `time.sleep`."""
        self._client = client
        self.token_budget = token_budget  # the retrieval-context budget — recorded in the fairness record
        self._poll_interval = poll_interval
        self._max_attempts = max(1, math.ceil(poll_timeout / poll_interval))
        self._sleep = sleep
        self._run_id: str | None = None
        self._last_seq = 0

    @property
    def name(self) -> str:
        return "lore"

    def ingest(self, sessions: Sequence[Session]) -> None:
        run = self._client.create_run()
        self._run_id = run.run_id
        last_seq = 0
        for session in sessions:
            for turn in session.turns:
                # Carry the session timestamp in the content so temporal-reasoning questions have the dates to
                # reason over (Lore assigns its own insert-order seq; the wall-clock lives in the text).
                prefix = f"[{session.date}] " if session.date else ""
                result = self._client.write(
                    run_id=self._run_id,
                    agent_id=turn.role,
                    content=f"{prefix}{turn.role}: {turn.content}",
                )
                last_seq = result.seq
        self._last_seq = last_seq
        self._wait_for_distillation(last_seq)

    def _wait_for_distillation(self, last_seq: int) -> None:
        # Read-your-writes as the sync primitive: extraction has caught up once covered_seq reaches the last
        # ingested seq. Probe with min_seq=0 (no assertion) and read covered_seq off the pack.
        if last_seq == 0 or self._run_id is None:
            return
        for attempt in range(self._max_attempts):
            pack = self._client.pack(
                run_id=self._run_id, query=_PROBE_QUERY, min_seq=0, token_budget=self.token_budget
            )
            if pack.covered_seq >= last_seq:
                return
            if attempt < self._max_attempts - 1:
                self._sleep(self._poll_interval)
        raise DistillationTimeout(
            f"extraction did not reach seq {last_seq} within "
            f"~{self._max_attempts * self._poll_interval:.1f}s ({self._max_attempts} polls)",
        )

    def retrieve(self, question: str, question_date: str) -> str:
        if self._run_id is None:
            raise RuntimeError("retrieve() called before ingest()")
        pack = self._client.pack(
            run_id=self._run_id,
            query=question,
            min_seq=self._last_seq,
            token_budget=self.token_budget,
        )
        return pack.text
