from longmemeval.stats import RunStats, estimate_tokens


def test_estimate_tokens_is_chars_over_four() -> None:
    assert estimate_tokens("") == 0
    assert estimate_tokens("a" * 40) == 10


def test_call_totals_split_by_stage_and_mode() -> None:
    stats = RunStats(
        answerer_sync_calls=3,
        answerer_batch_calls=7,
        judge_sync_calls=2,
        judge_batch_calls=8,
        cache_hits=5,
    )
    assert stats.answerer_calls == 10
    assert stats.judge_calls == 10


def test_merge_accumulates_every_field() -> None:
    a = RunStats(answerer_sync_calls=1, judge_batch_calls=2, cache_hits=3, answerer_input_tokens=100)
    b = RunStats(answerer_sync_calls=4, judge_batch_calls=5, cache_hits=6, judge_input_tokens=50)
    a.merge(b)
    assert a.answerer_sync_calls == 5
    assert a.judge_batch_calls == 7
    assert a.cache_hits == 9
    assert a.answerer_input_tokens == 100
    assert a.judge_input_tokens == 50


def test_summary_lines_report_mode_split_and_cache() -> None:
    stats = RunStats(answerer_sync_calls=1, answerer_batch_calls=2, judge_batch_calls=3, cache_hits=4)
    lines = "\n".join(stats.summary_lines())
    assert "answerer calls: 3" in lines  # 1 sync + 2 batch
    assert "judge calls: 3" in lines
    assert "cache served 4" in lines
    assert "50% rate" in lines  # the batch-discount signal is visible, no currency figure
