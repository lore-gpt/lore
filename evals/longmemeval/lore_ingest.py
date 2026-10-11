"""Ingest Lore's side of a run so an interruption costs the unfinished questions rather than the whole
measurement.

Why this is not the Mem0 pool. Lore ingests serially on purpose: the work happens server-side in one
deployment, so putting questions in flight together would change queue depth and extraction scheduling —
measurement conditions, not just speed. What this shares with the pool is everything around the work: a
record of what is already finished, and a file that says the run is still alive.

Why a stalled question no longer ends the run. Ingestion waits for distillation through Lore's own
read-your-writes contract, and that wait can run out. Sometimes it is slow — the same question took 94s,
177s and 260s across three runs — and sometimes it is permanent, because an extraction pass that exhausts
its attempts leaves the run's checkpoint where it is and nothing re-enqueues it until a new event arrives.
Until now that timeout propagated straight out of the ingestion loop and took the whole run with it,
including every question already paid for. Now the question is set aside, the remaining ones carry on, and
the stalled ones get one more attempt at the end: a fresh ingestion, which is what the protocol already does
for every question.

Why a resume carries a fingerprint. Reusing a directory is the cheap way to finish an interrupted run and
the cheap way to ruin one: contexts written by one build answered next to contexts written by another is a
measurement of neither. The fields a run can actually observe — the extraction model and mode, the embedder,
the split and the size — are written beside the checkpoint on first use and compared on every resume, and a
mismatch refuses rather than blends. What no fingerprint here can see is the server's extraction PROMPT: it
lives in the deployment, not in anything the harness is handed. A build whose prompt changed needs a new
directory, and that stays a matter of discipline.

What it deliberately does not do is score a run that is missing a question. A measurement over a smaller set
than it reports is worse than no measurement, because the accuracy still looks comparable while its
denominator has quietly changed. A question that is still stalled after its retries raises, naming what is
missing and where the finished work is — so resuming spends time only on that.
"""

from __future__ import annotations

import json
from collections.abc import Mapping, Sequence
from pathlib import Path

from ._types import Question
from .adapters import MemorySystem
from .adapters.lore import DistillationTimeout
from .progress import ContextCheckpoint, Heartbeat

FINGERPRINT_FILENAME = "run.json"


def _check_fingerprint(path: Path, fields: Mapping[str, str]) -> None:
    """Write this run's identity beside the checkpoint, or refuse a resume that does not match it."""
    if not path.exists():
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(dict(fields), indent=2, sort_keys=True), "utf-8")
        return
    recorded = json.loads(path.read_text("utf-8"))
    differs = sorted(k for k in set(recorded) | set(fields) if recorded.get(k) != fields.get(k))
    if differs:
        detail = "; ".join(f"{k}: {recorded.get(k)!r} -> {fields.get(k)!r}" for k in differs)
        raise RuntimeError(
            f"{path} was written by a different run ({detail}). Resuming would answer contexts built by one "
            f"configuration next to contexts built by another, which measures neither — point --lore-run-dir "
            f"at a new directory for this one."
        )


def build_contexts(
    questions: Sequence[Question],
    system: MemorySystem,
    *,
    checkpoint: ContextCheckpoint,
    heartbeat: Heartbeat,
    fingerprint: Mapping[str, str] | None = None,
    stall_retries: int = 1,
) -> dict[str, str]:
    """Ingest every question and return its retrieved context, resuming whatever is already checkpointed.

    Only a stalled distillation is survivable here. Anything else — a dead server, a rejected write — is
    raised on the spot: it says the deployment is wrong rather than one question being slow, and the
    checkpoint has already saved the finished work either way.

    `fingerprint` is what this run is; a resume whose fingerprint differs is refused rather than blended.

    Raises RuntimeError when a question is still stalled after `stall_retries` further attempts.
    """
    if fingerprint is not None:
        _check_fingerprint(checkpoint.path.parent / FINGERPRINT_FILENAME, fingerprint)
    known = {q.question_id for q in questions}
    done = {qid: ctx for qid, ctx in checkpoint.load().items() if qid in known}
    todo = [q for q in questions if q.question_id not in done]
    heartbeat.write(done=len(done), in_flight=[], note=f"resuming with {len(todo)} of {len(questions)} left")

    for attempt in range(stall_retries + 1):
        if not todo:
            break
        note = "ingesting" if attempt == 0 else f"retrying stalled questions (attempt {attempt + 1})"
        stalled: list[Question] = []
        for question in todo:
            heartbeat.write(
                done=len(done),
                in_flight=[question.question_id],
                note=note,
                extra={"stalled": [q.question_id for q in stalled]},
            )
            try:
                system.ingest(question.sessions)
                context = system.retrieve(question.question, question.question_date)
            except DistillationTimeout:
                # Set aside, not raised: the questions after this one are independent of it, and the run has
                # already bought their histories. A retry happens once the rest are done.
                stalled.append(question)
                continue
            checkpoint.record(question.question_id, context)
            done[question.question_id] = context
        todo = stalled

    if todo:
        heartbeat.write(
            done=len(done),
            in_flight=[],
            note="stalled",
            extra={"stalled": [q.question_id for q in todo]},
        )
        ids = ", ".join(q.question_id for q in todo)
        raise RuntimeError(
            f"distillation never caught up for {len(todo)} of {len(questions)} questions after "
            f"{stall_retries + 1} attempts: {ids}. The {len(done)} finished contexts are in "
            f"{checkpoint.path}, and re-running against the same directory retries only what is missing. "
            f"A slow pass wants a longer poll timeout; a pass that ran out of attempts leaves the run's "
            f"checkpoint parked, and no length of wait moves it."
        )

    heartbeat.write(done=len(done), in_flight=[], note="ingestion finished")
    return done
