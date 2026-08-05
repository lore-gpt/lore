"""The resume promise, made testable.

Both of these exist because two multi-hour runs died with nothing on disk to say how far they had got. A
checkpoint that loses work on a kill, or a heartbeat that cannot distinguish "slow" from "dead", would be
worse than none at all — it would look like a safety net while not being one. So each guarantee is pinned.
"""

from __future__ import annotations

import json
from pathlib import Path

from longmemeval import ContextCheckpoint, Heartbeat


def test_checkpoint_round_trips_and_survives_a_torn_line(tmp_path: Path) -> None:
    path = tmp_path / "contexts.jsonl"
    checkpoint = ContextCheckpoint(path)
    assert checkpoint.load() == {}, "an absent checkpoint means nothing to resume, not an error"

    checkpoint.record("q1", "context one")
    checkpoint.record("q2", "context\nwith a newline")
    assert checkpoint.load() == {"q1": "context one", "q2": "context\nwith a newline"}

    # A process killed mid-write leaves a partial final line. That question was not finished, so dropping it
    # is exactly right — and the finished ones before it must still come back.
    with path.open("a", encoding="utf-8") as fh:
        fh.write('{"question_id": "q3", "context": "half a lin')
    assert checkpoint.load() == {"q1": "context one", "q2": "context\nwith a newline"}


def test_checkpoint_records_are_independently_readable(tmp_path: Path) -> None:
    # One JSON object per line, so a reader (or a human) can see progress while the run is still going.
    checkpoint = ContextCheckpoint(tmp_path / "contexts.jsonl")
    checkpoint.record("q1", "one")
    checkpoint.record("q2", "two")
    lines = (tmp_path / "contexts.jsonl").read_text("utf-8").splitlines()
    assert [json.loads(line)["question_id"] for line in lines] == ["q1", "q2"]


def test_heartbeat_reports_progress_and_projects_a_finish(tmp_path: Path) -> None:
    clock = iter([0.0, 600.0, 600.0])  # start, then one tick ten minutes in
    beat = Heartbeat(tmp_path / "heartbeat.json", total=4, now=lambda: next(clock))
    beat.write(done=1, in_flight=["q2", "q3"], note="ingesting")

    state = json.loads((tmp_path / "heartbeat.json").read_text("utf-8"))
    assert state["done"] == 1
    assert state["total"] == 4
    assert state["in_flight"] == ["q2", "q3"]
    assert state["elapsed_seconds"] == 600
    # One question in ten minutes leaves three to go: the operator gets an ETA rather than a guess.
    assert state["seconds_per_question"] == 600.0
    assert state["eta_seconds"] == 1800
    # The timestamp is the whole point: a file whose updated_at stops advancing names the moment of death,
    # and in_flight names where it happened.
    assert state["updated_at"].endswith("Z")


def test_heartbeat_before_any_progress_still_says_it_is_alive(tmp_path: Path) -> None:
    # A run whose first question takes 38 minutes must not look dead for 38 minutes.
    clock = iter([0.0, 60.0, 60.0])
    beat = Heartbeat(tmp_path / "heartbeat.json", total=2, now=lambda: next(clock))
    beat.write(done=0, in_flight=["q1", "q2"], note="0/2 workers reported yet")

    state = json.loads((tmp_path / "heartbeat.json").read_text("utf-8"))
    assert state["done"] == 0
    assert state["elapsed_seconds"] == 60
    assert "eta_seconds" not in state, "an ETA from zero completions would be a fabricated number"
    assert state["note"]


def test_heartbeat_is_rewritten_whole(tmp_path: Path) -> None:
    # Written via a temp file and renamed, so a reader polling the file never catches it half-written.
    path = tmp_path / "heartbeat.json"
    clock = iter([0.0, 10.0, 10.0, 20.0, 20.0])
    beat = Heartbeat(path, total=2, now=lambda: next(clock))
    beat.write(done=0, in_flight=["q1"])
    beat.write(done=2, in_flight=[], note="done")

    state = json.loads(path.read_text("utf-8"))
    assert state["done"] == 2
    assert state["in_flight"] == []
    assert not list(tmp_path.glob("*.tmp")), "the temp file must not be left behind"
