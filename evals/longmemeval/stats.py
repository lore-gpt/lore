"""Run statistics for cost visibility. This counts the model calls a run actually made — split by stage
(answerer vs judge) and by mode (synchronous vs Batch API) — plus a COARSE token estimate (chars / 4, the
same v0 proxy the SDK uses; not a tokenizer, not exact). No currency figures: a bare call count and
a token estimate are provider-neutral, and the caller multiplies by whatever per-token rate their provider
charges. Batch stages are noted separately because the Batch API bills at half the synchronous per-token rate,
so a mode split is the meaningful economy signal."""

from __future__ import annotations

from dataclasses import dataclass

# Coarse token proxy: ~4 characters per token. A deliberate v0 estimate, not a real tokenizer.
_CHARS_PER_TOKEN = 4


def estimate_tokens(text: str) -> int:
    """A coarse token estimate for a piece of text (chars / 4). For visibility only."""
    return len(text) // _CHARS_PER_TOKEN


@dataclass(slots=True)
class RunStats:
    """Mutable accumulator threaded through a run. Counts calls and input-token estimates by stage and mode.
    `cache_hits` are judge decisions served from cache (no model call), so they are the direct measure of what
    the judge cache saved."""

    answerer_sync_calls: int = 0
    answerer_batch_calls: int = 0
    judge_sync_calls: int = 0
    judge_batch_calls: int = 0
    cache_hits: int = 0
    answerer_input_tokens: int = 0  # coarse estimate, summed over answerer prompts
    judge_input_tokens: int = 0  # coarse estimate, summed over judge prompts

    @property
    def answerer_calls(self) -> int:
        return self.answerer_sync_calls + self.answerer_batch_calls

    @property
    def judge_calls(self) -> int:
        return self.judge_sync_calls + self.judge_batch_calls

    def merge(self, other: RunStats) -> None:
        """Fold another RunStats into this one (accumulate across trials / systems)."""
        self.answerer_sync_calls += other.answerer_sync_calls
        self.answerer_batch_calls += other.answerer_batch_calls
        self.judge_sync_calls += other.judge_sync_calls
        self.judge_batch_calls += other.judge_batch_calls
        self.cache_hits += other.cache_hits
        self.answerer_input_tokens += other.answerer_input_tokens
        self.judge_input_tokens += other.judge_input_tokens

    def summary_lines(self) -> list[str]:
        """Human-readable cost-visibility lines. Tokens are coarse estimates; batch stages bill at ~50% of the
        synchronous per-token rate (multiply by your provider's rate for a currency figure)."""
        return [
            f"answerer calls: {self.answerer_calls} "
            f"({self.answerer_sync_calls} sync, {self.answerer_batch_calls} batch @ ~50% rate) "
            f"~{self.answerer_input_tokens} input tokens (est)",
            f"judge calls: {self.judge_calls} "
            f"({self.judge_sync_calls} sync, {self.judge_batch_calls} batch @ ~50% rate) "
            f"~{self.judge_input_tokens} input tokens (est); cache served {self.cache_hits}",
        ]
