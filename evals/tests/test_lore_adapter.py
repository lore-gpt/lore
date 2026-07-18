import pytest
from loregpt import PackResult, RunResult, WriteResult

from longmemeval._types import Session, Turn
from longmemeval.adapters.lore import DistillationTimeout, LoreAdapter


class FakeLore:
    """A LoreLike fake: records writes/packs and ramps covered_seq up to the last write after `covered_after`
    distillation probes, so the adapter's read-your-writes poll can be exercised without a server."""

    def __init__(self, covered_after: int = 1) -> None:
        self.writes: list[tuple[str, str, str]] = []
        self.packs: list[tuple[str, int, int]] = []
        self._probes = 0
        self._covered_after = covered_after
        self._last_seq = 0

    def create_run(self) -> RunResult:
        return RunResult(run_id="run-1", created_at="t")

    def write(self, *, run_id: str, agent_id: str, content: str) -> WriteResult:
        self.writes.append((run_id, agent_id, content))
        self._last_seq = len(self.writes)
        return WriteResult(event_id=f"ev-{self._last_seq}", seq=self._last_seq)

    def pack(self, *, run_id: str, query: str, min_seq: int, token_budget: int) -> PackResult:
        self.packs.append((query, min_seq, token_budget))
        if query == "distillation probe":
            self._probes += 1
            covered = self._last_seq if self._probes >= self._covered_after else 0
        else:
            covered = self._last_seq
        return PackResult(
            text=f"PACK[{query}]",
            sources=[],
            covered_seq=covered,
            freshness_lag_ms=0,
            saved_tokens=0,
            working_source="live",
            truncated=False,
        )


def _sessions() -> list[Session]:
    return [
        Session("s1", "2031/01/04", (Turn("user", "hi"), Turn("assistant", "hello"))),
        Session("s2", "", (Turn("user", "bye"),)),
    ]


def test_ingest_writes_turns_with_role_and_date_prefix() -> None:
    fake = FakeLore(covered_after=1)
    adapter = LoreAdapter(fake, sleep=lambda s: None)
    adapter.ingest(_sessions())
    assert [agent for _, agent, _ in fake.writes] == ["user", "assistant", "user"]
    assert fake.writes[0][2] == "[2031/01/04] user: hi"
    assert fake.writes[2][2] == "user: bye"  # an empty date carries no prefix


def test_ryw_poll_waits_until_covered() -> None:
    fake = FakeLore(covered_after=3)  # covered_seq catches up on the 3rd probe
    sleeps: list[float] = []
    adapter = LoreAdapter(fake, sleep=sleeps.append, poll_interval=0.1, poll_timeout=10.0)
    adapter.ingest(_sessions())
    probes = [p for p in fake.packs if p[0] == "distillation probe"]
    assert len(probes) == 3
    # Sleeps happen only BETWEEN probes, each for the configured poll_interval.
    assert sleeps == [0.1, 0.1]


def test_retrieve_packs_with_read_your_writes() -> None:
    fake = FakeLore(covered_after=1)
    adapter = LoreAdapter(fake, sleep=lambda s: None)
    adapter.ingest(_sessions())
    context = adapter.retrieve("what?", "2031/02/01")
    answer_pack = next(p for p in fake.packs if p[0] == "what?")
    assert answer_pack[1] == 3  # min_seq == the last ingested seq (read-your-writes)
    assert context == "PACK[what?]"  # retrieve returns the pack context, not an answer


def test_distillation_timeout_when_never_caught_up() -> None:
    fake = FakeLore(covered_after=999)
    adapter = LoreAdapter(fake, sleep=lambda s: None, poll_interval=0.1, poll_timeout=0.3)
    with pytest.raises(DistillationTimeout):
        adapter.ingest(_sessions())


def test_retrieve_before_ingest_raises() -> None:
    adapter = LoreAdapter(FakeLore())
    with pytest.raises(RuntimeError):
        adapter.retrieve("q", "d")
