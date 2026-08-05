"""Operational I/O for a measurement that runs for hours: a resumable record of finished work, and a file
that says the run is still alive.

Everything else in this package avoids the clock and the filesystem on purpose — a measurement artefact
should be a function of its inputs. These two are not measurement artefacts. They exist because a run that
takes eight hours will sometimes be interrupted, and because a run that has silently died looks exactly like
a run that is still working. Both were paid for the hard way: two multi-hour runs were lost, one of them 2h32m
in, with no error, no traceback, and nothing on disk to say how far it had got.

So this module reads the clock, writes files, and says so out loud rather than pretending to be pure.
"""

from __future__ import annotations

import json
import os
import time
from collections.abc import Callable, Mapping, Sequence
from pathlib import Path


class ContextCheckpoint:
    """Append-only record of the questions whose ingestion is finished, keyed by question id.

    What is stored is the retrieved context, not the memory store behind it. That is the whole artefact the
    rest of the run needs: the reuse-ingest protocol holds ingestion fixed and answers over these strings
    N times, so a resumed run that reads a context from here is doing exactly what the interrupted run would
    have done next. Re-ingesting to rebuild an identical context would only spend the money again.

    Append-only and flushed per record, so a process killed mid-write loses at most the question it was
    working on. A truncated final line is dropped on load rather than failing the resume — a half-written
    line means that question was not finished, which is the same thing as it not being here.
    """

    def __init__(self, path: Path) -> None:
        self.path = path

    def load(self) -> dict[str, str]:
        """Every finished question, or an empty mapping when there is nothing to resume."""
        if not self.path.exists():
            return {}
        done: dict[str, str] = {}
        for line in self.path.read_text("utf-8").splitlines():
            if not line.strip():
                continue
            try:
                record = json.loads(line)
            except json.JSONDecodeError:
                continue  # a torn last line: that question never finished
            question_id = record.get("question_id")
            context = record.get("context")
            if isinstance(question_id, str) and isinstance(context, str):
                done[question_id] = context
        return done

    def record(self, question_id: str, context: str) -> None:
        """Persist one finished question. Flushed and synced: the point is to survive a kill."""
        self.path.parent.mkdir(parents=True, exist_ok=True)
        line = json.dumps({"question_id": question_id, "context": context}, ensure_ascii=False)
        with self.path.open("a", encoding="utf-8") as fh:
            fh.write(line + "\n")
            fh.flush()
            os.fsync(fh.fileno())


class Heartbeat:
    """A file that answers "is this still running, and where is it?" without attaching to the process.

    Rewritten whole on every tick so a reader never sees a partial state, and carrying the wall-clock time of
    the last tick so a stale file is obvious: if the timestamp stops advancing, the run died at whatever is
    listed as in flight. That is the diagnosis the two lost runs could not give.

    `now` is injectable so the unit tests do not depend on real time passing.
    """

    def __init__(self, path: Path, *, total: int, now: Callable[[], float] = time.time) -> None:
        self.path = path
        self.total = total
        self._now = now
        self._started = now()

    def write(
        self,
        *,
        done: int,
        in_flight: Sequence[str],
        note: str = "",
        extra: Mapping[str, object] | None = None,
    ) -> None:
        elapsed = self._now() - self._started
        state: dict[str, object] = {
            "updated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(self._now())),
            "elapsed_seconds": round(elapsed),
            "done": done,
            "total": self.total,
            "in_flight": list(in_flight),
            "note": note,
        }
        if done:
            remaining = self.total - done
            state["seconds_per_question"] = round(elapsed / done, 1)
            state["eta_seconds"] = round(elapsed / done * remaining)
        if extra:
            state.update(extra)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        tmp = self.path.with_suffix(self.path.suffix + ".tmp")
        tmp.write_text(json.dumps(state, indent=2, sort_keys=True), "utf-8")
        tmp.replace(self.path)
