"""Ingest Mem0's side of a run across several worker processes.

Why this exists: measured, Mem0 spends about 38 minutes per LongMemEval-S question — roughly 50 sessions,
two LLM calls each, on a reasoning model. At n=50 that is over thirty hours, which is not a run anyone
finishes. Cost is not the problem (single-digit dollars); wall clock is.

Why it is safe to parallelise this and nothing else. Sessions inside one question must stay ordered, because
Mem0 decides whether a new fact adds to or updates the memories already there. Across questions there is no
such link: each is ingested under its own scope and searched under that scope alone. So the unit of
parallelism is the question, and inside a question nothing changes.

Why processes rather than threads. `qdrant-client`'s local mode carries no lock — no threading import in
either qdrant_local.py or local_collection.py — so sharing one store between threads relies on a race not
happening rather than on it being impossible. A two-worker probe passed, which proves only that. Separate
processes remove the question: MEM0_DIR moves Mem0's home, including the migrations store it opens whatever
the configured vector-store path is, so two workers share nothing. (That same hard-coded store is why
per-worker collections inside one process are not an option: two Memory instances in one process collide on
it and the second one raises.)

Why this is Mem0-only. Lore's ingestion is server-side work through one deployment, so running questions
concurrently would change queue depth and extraction scheduling — measurement conditions, not just speed.
Its serial cost is a few hours, which is affordable. Mem0's is not.

None of this touches Mem0's configuration: same model, same flags, same one-add-per-session order within a
question. Only the number of questions in flight changes.
"""

from __future__ import annotations

import multiprocessing as mp
import queue
import sys
import traceback
from collections.abc import Sequence
from dataclasses import dataclass
from multiprocessing.context import SpawnContext
from pathlib import Path

from ._types import Question
from .progress import ContextCheckpoint, Heartbeat

# How long the driver waits for a result before writing a heartbeat and looking again. Short enough that a
# death is visible within a minute, long enough that the file is not rewritten pointlessly.
HEARTBEAT_INTERVAL_SECONDS = 30.0

# A question takes ~38 minutes. If nothing lands in three times that AND no worker is alive, the pool has
# died rather than being slow, and waiting longer only wastes the operator's evening.
STALL_TIMEOUT_SECONDS = 3 * 38 * 60


@dataclass(frozen=True)
class Mem0PoolConfig:
    """Everything a worker needs to rebuild the same Mem0 the serial path would have built.

    `root` is per-run and is what keeps runs from bleeding into each other. Mem0's local store persists
    across processes while the harness's scope counter restarts at 1 in each one, so two runs sharing a root
    write different questions into the same scope and search them together — measured in a live store, where
    one scope held 558 memories from one day's run and 635 from the next. A fresh root per run makes that
    impossible instead of merely unlikely.
    """

    root: Path
    llm_model: str
    is_reasoning_model: bool
    top_k: int
    split: str
    n: int
    cache_dir: Path


@dataclass(frozen=True)
class _Job:
    index: int  # position in the deterministic subset; the worker re-derives the question from it
    question_id: str


def _worker(
    cfg: Mem0PoolConfig,
    worker_index: int,
    jobs: mp.Queue[_Job | None],
    out: mp.Queue[tuple[str, bool, str]],
) -> None:
    """One worker process: its own Mem0 home, its own store, one question at a time.

    Runs under the spawn start method, so this re-imports everything. That is deliberate — MEM0_DIR has to be
    set before Mem0 is imported for it to take effect, and a forked child would already have the parent's
    module loaded.
    """
    import os

    home = cfg.root / f"home-{worker_index}"
    home.mkdir(parents=True, exist_ok=True)
    os.environ["MEM0_DIR"] = str(home)

    try:
        from mem0 import Memory

        from .adapters.mem0 import Mem0Adapter
        from .loader import deterministic_subset, download_split, load_questions
    except Exception:  # pragma: no cover - import failure is reported, not raised into the pool
        out.put(("", False, f"worker {worker_index} could not start:\n{traceback.format_exc(limit=4)}"))
        return

    try:
        questions = deterministic_subset(load_questions(download_split(cfg.split, cfg.cache_dir / "hf")), cfg.n)
        memory = Memory.from_config(
            {
                "llm": {
                    "provider": "openai",
                    "config": {"model": cfg.llm_model, "is_reasoning_model": cfg.is_reasoning_model},
                },
                "vector_store": {"provider": "qdrant", "config": {"path": str(cfg.root / f"store-{worker_index}")}},
            }
        )
        adapter = Mem0Adapter(memory, top_k=cfg.top_k)
    except Exception:
        out.put(("", False, f"worker {worker_index} could not build Mem0:\n{traceback.format_exc(limit=4)}"))
        return

    while True:
        job = jobs.get()
        if job is None:
            return
        question = questions[job.index]
        try:
            adapter.ingest(question.sessions)
            context = adapter.retrieve(question.question, question.question_date)
        except Exception:
            out.put((job.question_id, False, traceback.format_exc(limit=6)))
            continue
        out.put((job.question_id, True, context))


def build_contexts(
    questions: Sequence[Question],
    cfg: Mem0PoolConfig,
    *,
    workers: int,
    checkpoint: ContextCheckpoint,
    heartbeat: Heartbeat,
) -> dict[str, str]:
    """Ingest every question and return its retrieved context, resuming whatever is already checkpointed.

    Raises RuntimeError if the pool stops making progress with work outstanding — a stall is reported rather
    than waited out, because the checkpoint means restarting costs only the unfinished questions.
    """
    done = {qid: ctx for qid, ctx in checkpoint.load().items() if any(q.question_id == qid for q in questions)}
    todo = [(i, q) for i, q in enumerate(questions) if q.question_id not in done]
    heartbeat.write(done=len(done), in_flight=[], note=f"resuming with {len(todo)} of {len(questions)} left")
    if not todo:
        return done

    ctx_mp: SpawnContext = mp.get_context("spawn")
    jobs: mp.Queue[_Job | None] = ctx_mp.Queue()
    out: mp.Queue[tuple[str, bool, str]] = ctx_mp.Queue()
    for index, question in todo:
        jobs.put(_Job(index=index, question_id=question.question_id))
    n_workers = max(1, min(workers, len(todo)))
    for _ in range(n_workers):
        jobs.put(None)

    procs = [ctx_mp.Process(target=_worker, args=(cfg, i, jobs, out), daemon=True) for i in range(n_workers)]
    for p in procs:
        p.start()

    in_flight = {q.question_id for _, q in todo}
    failures: list[str] = []
    idle_seconds = 0.0
    try:
        while in_flight:
            try:
                question_id, ok, payload = out.get(timeout=HEARTBEAT_INTERVAL_SECONDS)
            except queue.Empty:
                idle_seconds += HEARTBEAT_INTERVAL_SECONDS
                alive = sum(1 for p in procs if p.is_alive())
                heartbeat.write(
                    done=len(done),
                    in_flight=sorted(in_flight),
                    note=f"{alive}/{n_workers} workers alive",
                    extra={"idle_seconds": round(idle_seconds)},
                )
                if alive == 0:
                    raise RuntimeError(
                        f"every Mem0 worker exited with {len(in_flight)} question(s) unfinished: "
                        f"{sorted(in_flight)}. Exit codes {[p.exitcode for p in procs]}. The checkpoint at "
                        f"{checkpoint.path} holds the finished ones; re-running resumes from there."
                    ) from None
                if idle_seconds >= STALL_TIMEOUT_SECONDS:
                    raise RuntimeError(
                        f"no Mem0 worker produced a result in {idle_seconds / 60:.0f} minutes with "
                        f"{len(in_flight)} question(s) outstanding: {sorted(in_flight)}. Treating this as a "
                        f"stall; the checkpoint at {checkpoint.path} holds the finished ones."
                    ) from None
                continue

            idle_seconds = 0.0
            if not question_id:  # a worker failed to start at all
                failures.append(payload)
                continue
            in_flight.discard(question_id)
            if not ok:
                failures.append(f"{question_id}:\n{payload}")
                continue
            done[question_id] = payload
            checkpoint.record(question_id, payload)
            heartbeat.write(done=len(done), in_flight=sorted(in_flight), note="ingesting")
    finally:
        for p in procs:
            if p.is_alive():
                p.terminate()
        for p in procs:
            p.join(timeout=30)

    if failures:
        detail = "\n\n".join(failures)
        print(f"warning: {len(failures)} Mem0 question(s) failed to ingest:\n{detail}", file=sys.stderr)
    missing = [q.question_id for q in questions if q.question_id not in done]
    if missing:
        raise RuntimeError(
            f"{len(missing)} question(s) never produced a context: {missing}. Scoring a partial set would "
            f"report an accuracy over a different denominator than the one the universe key claims."
        )
    heartbeat.write(done=len(done), in_flight=[], note="ingestion complete")
    return done
