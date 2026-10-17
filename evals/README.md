# LongMemEval harness

A reproducible harness that scores memory systems on [LongMemEval](https://github.com/xiaowu0162/LongMemEval)
through **one pinned, cached judge** — the *same-judge discipline*: every system is graded by the identical
judge model and the identical official prompts, and answers the question through the identical shared answerer,
so the numbers are comparable by construction rather than by each vendor's own grader and bundled models.

Adapters ship for **Lore** and **Mem0 OSS**. An adapter for **Graphiti** (the OSS successor to Zep — the
library, not the hosted product) plugs into the same interface in a later slice; adding a system is a
socket-plug, not a harness change.

## Design

- **Adapter** (`longmemeval.adapters.MemorySystem`) — the one interface every system implements. It is
  memory-only: `ingest` a timestamped multi-session history into a fresh isolated scope, then `retrieve` the
  relevant context for a question. It does **not** answer — answering and judging are shared harness steps
  applied identically to every system, which is what makes the comparison a parity comparison.
  - *Lore* drives the server through the published Python SDK and waits for async distillation via the
    read-your-writes contract (`covered_seq`) before retrieving the pack.
  - *Mem0* drives the in-process `mem0.Memory` (embedded vector store, no external service) — `add` per session,
    `search` for retrieval.
- **Answerer** — a shared model (Anthropic Claude) turns each system's retrieved context into an answer. The
  same answerer + prompt is used for every system.
- **Judge** — the official LongMemEval judge: a pinned OpenAI GPT-4o snapshot with the official per-type
  prompts, verbatim. A harness-only dependency; not part of the Lore product.
- **Batch API** — full runs route the answerer (Anthropic Message Batches) and the judge (OpenAI Batch) through
  a submit → poll → collect flow at the Batch API's half rate. Submitted batch ids are persisted so a run that
  dies mid-flight resumes instead of paying again; a dropped individual request falls back to a synchronous
  call. The batch and synchronous paths share the same prompt builders, so a batched result matches its
  synchronous twin.
- **Cache** (`longmemeval.cache.JudgeCache`) — judge decisions keyed on
  `(question_id, answer_hash, judge_model, rubric_version)`, so an unchanged answer under an unchanged judge and
  rubric is never re-judged.
- **Variance** — a score is a mean ± std over trials, under two labelled protocols that are never conflated:
  - `variance_answer` — ingest once, answer + judge N times on the full set. Isolates answerer + judge
    nondeterminism. This is the **gated** metric (target std < 2 accuracy points).
  - `variance_pipeline` — full re-ingest N times on a fixed small subset. Also captures memory-construction
    nondeterminism; reported as a system property, **not** gated (its subset `n` is recorded).
  Overall, per-type, and abstention accuracy are reported separately (abstention measures calibration, not
  recall, so it is never folded into the overall number).
- **Reproducibility chain** — every report embeds the dataset repo + pinned revision, the judge model + prompt
  hash, the answerer + extraction models, the extraction mode and the system's version/configuration (the
  fairness record — a score is only defensible next to how the system was driven), `n`, and the timestamp. Two
  runs are comparable only when the whole chain matches.

## Dataset

The dataset is fetched from HuggingFace (`xiaowu0162/longmemeval-cleaned`) at a pinned revision at runtime and
cached locally — it is **never vendored into this repo**. A tiny, original **clean-room** fixture
(`fixtures/clean_room.json`) — in the LongMemEval schema but with entirely original content — backs the keyless
unit tests. Dataset credit: [LongMemEval (Wu et al., ICLR 2025)](https://github.com/xiaowu0162/LongMemEval).

## Running

```bash
uv sync
uv run pytest        # keyless unit tests (no API calls, no network)
uv run mypy          # strict type-check
uvx ruff@0.14.4 check .

# keyless plan + coarse cost estimate (no keys, no network)
uv run python -m examples.smoke --dry-run --split fixture --n 3 --systems lore,mem0 --batch --pipeline
```

The real evaluation (against a running Lore stack, a real extractor, the GPT-4o judge, and the Claude answerer,
plus Mem0's OpenAI-backed extraction) is **secret-gated** and **staged**. Install the competitor extra with
`uv sync --extra competitors`. It needs an OpenAI key (judge + Mem0), an Anthropic key (answerer), and a Lore
deployment (`LORE_BASE_URL`); costs are estimated before the run via `--dry-run`. Result artifacts (scores,
model outputs) are gitignored — a standalone number is not published until a measurement-gated decision.

### Which questions: named subsets

The `s` split is measured by **named subset**, never by count: `--subset` is required there and `--n` is refused.
The subsets divide the split's 500 questions into disjoint parts with distinct jobs:

| Subset | Questions | Job |
| --- | --- | --- |
| `gate-v1` | 50 | the questions every earlier n=50 measurement used — kept so those runs stay reproducible |
| `dev` | 70 | development: measure, change, measure again, as often as needed |
| `gate-v2` | 50 | read once, for a verdict, after development is finished |
| `holdout` | 330 | untouched |

Selection is mechanical: within each question type, questions are taken in `question_id` order (the ids are hashes,
so the order is effectively random but reproducible) with per-type counts fixed in `longmemeval/subsets.py`. The
verdict sets share the split's own type proportions; `dev` raises the two smallest types so a change to them is
visible. The lists live in `subsets/*.json` (ids and types only) and `python -m examples.make_subsets --check`
proves they are still what the rule produces.

Subsets named `gate-*` are **sealed**: a real run refuses them unless `--unseal-gate` is passed, and the subset's
name lands in the report's provenance as the record of the read. Development tooling compares answers against
gold question by question, and a verdict set looked at that way stops measuring the system.

The subset is part of the measurement universe (with the split, dataset revision, judge, answerer, n and protocol),
so a Mem0 reference locked on `dev` can never grade a run on `gate-v2`. Each universe keeps its own reference file
under `baseline/`. The answerer is pinned to a dated model snapshot for the same reason: an alias can resolve to a
different model between two measurements taken weeks apart.

### Isolating each question

LongMemEval asks whether a system can answer from **one** user's history. Lore scopes distilled recall to a
project and only the raw tail to a run, so questions sharing a project recall each other's histories — on a
three-question probe, 25-65% of a pack's cited sources came from a different question, and because the context
is a fixed size those displace the evidence the question needs. Mem0 isolates for free (a fresh `user_id` per
ingestion), so a shared-project Lore run would not be measuring the same thing.

So a real Lore run provisions an empty project per ingestion, and refuses to start without one. Pass the
command that creates one for your deployment; `{name}` is substituted per ingestion, and the command must
print the API key on stdout — which `lore provision` does when run **without** `--out`:

```bash
uv run python -m examples.smoke --split s --subset dev --systems lore \
  --lore-provision-cmd 'docker compose -f /path/to/docker-compose.yml run --rm --no-deps lore-server provision --project {name}' \
  --lore-run-dir /tmp/eval/lore-dev-1
```

Each turn reaches Lore as a structured event: the dated text both systems read (`[2023/05/21 (Sun) 14:49] user:
...`) plus `role`, `session_id` and `occurred_at` — machine-readable copies of what that text already says,
never information the compared system lacks.

Two things to know before running it:

- **Use a throwaway database.** Every ingestion leaves a project behind and nothing reclaims them; that is
  deliberate (a cleanup path would be machinery for a measurement that runs on a disposable stack). Do not
  point a real deployment at this.
- **Pass `--lore-run-dir`.** It is what makes a long run survivable; see below. Without it an interruption
  costs the whole run, and so does a single question whose distillation never catches up.

`--lore-poll-timeout` bounds how long one question may wait for distillation before it counts as stalled.
The default is 600s, which is a measured number rather than a round one: the same 514-event question was
observed taking 94s, 177s and 260s across three runs, so a budget near the low end of that spread turns
ordinary variance into a failure. The former 60s default was a fixture-scale number.

### Lore's run is resumable too

Lore ingests **serially** and that does not change: its work happens server-side in one deployment, so
putting questions in flight together would change queue depth and extraction scheduling — measurement
conditions, not just speed. What `--lore-run-dir` adds is everything around the work:

- **Each finished question's context is written as it lands.** Re-running against the same directory costs
  the questions that had not finished, not the ones that had.
- **A stalled question does not end the run.** It is set aside, the rest carry on, and it gets one more
  attempt — a fresh ingestion — at the end. If it still stalls the run fails and names it. A partial set
  is never scored: an accuracy over fewer questions than it reports still looks comparable while its
  denominator quietly changed.
- **A resume under a different configuration is refused.** The extraction model and mode, the embedder, the
  split, the subset and the question count are written beside the checkpoint and compared on every resume.
  What no check here can see is the server's extraction **prompt** — the harness is never handed it — so a
  build whose prompt changed needs a new directory. That part is discipline.
- **`heartbeat.json` says whether it is still alive**, which question is in flight, and what has stalled.

### What a real run refuses to do

Six checks run before anything is spent. Each exists because the failure it catches is silent — the run
completes, the report reads normally, and only the number is wrong:

| Refusal | Why it is not obvious |
| --- | --- |
| The dataset revision is the unpinned placeholder | The score would not be reproducible, and a reference measured under one revision does not apply to another |
| No per-question provisioning command for Lore | Questions share a project and recall each other's histories |
| The embedder is unknown or the offline fixture | A vector space no deployment uses is not the one the score claims to be about |
| The extractor is unknown or the offline fixture | The fixture returns canned output, so the run measures the fixture rather than extraction. This identity is read from the server's `/healthz` and never from the harness process's own environment — extraction runs in the worker, so this process's `LORE_EXTRACTION_*` says nothing about it |
| A sealed `gate-*` subset without `--unseal-gate` | Development reads a verdict set early, and the verdict then measures the tuning |
| `--batch` together with `--lore-run-dir` | The batch path never consults the checkpoint, so the run looks resumable and is not |

### A verdict from more than one ingestion

A Lore run ingests every question through an LLM extractor, and the memories differ from one ingestion to the next:
on the same ten questions, two ingestions of identical code disagreed on four of them. The answer-variance protocol
cannot see that — it holds ingestion fixed — so read a verdict from at least two independent ingestions, each with
its own `--lore-run-dir`, run one after the other, and combine them:

```bash
uv run python -m examples.aggregate reports/lore.s.dev.<A>.json reports/lore.s.dev.<B>.json
```

The gate is applied to the mean and the range is printed beside it. Reports from different systems or universes
are refused rather than averaged.

### Diagnosing a development run

```bash
uv run python -m examples.devloop reports/lore.s.dev.<A>.json --run-dir /tmp/eval/lore-dev-1
```

It reads only what the run wrote and separates wrong answers by where the evidence was: **not in context** (it was
never distilled, or never retrieved) or **in context** (it was in front of the answerer and the answer was still
wrong) — two failures with opposite fixes. It also reports how much of the returned context is anchored in time.
The gold-answer check is a heuristic: it probes only short answers and matches numbers on word boundaries. It
refuses sealed subsets.

### Mem0 takes hours, and the run is built to survive that

Measured on LongMemEval-S: **about 38 minutes per question**, roughly fifty sessions each needing two LLM
calls on a reasoning model. Serially that is ~6 hours at n=10 and over **thirty hours** at n=50. Cost is not
the constraint — single-digit dollars — wall clock is.

`--mem0-workers N` ingests N questions at once, in separate **processes**:

```bash
uv run python -m examples.smoke --split s --subset dev --systems mem0 --trials 3 --mem0-workers 4
```

Three things about that, in the order they matter:

- **It changes speed, not the competitor.** Sessions inside one question stay ordered, because Mem0 decides
  whether a new fact adds to or updates what is already there. Across questions there is no such link — each
  is ingested and searched under its own scope. Same model, same flags, same order within a question; only
  the number of questions in flight changes.
- **Processes, not threads, deliberately.** `qdrant-client`'s local mode carries no lock of its own, so
  sharing one store between threads would rest on a race not happening rather than on it being impossible. A
  two-worker thread probe passed, which proves only that. Per-worker stores inside one process are not an
  option either: Mem0 opens a second, fixed store under its home whatever you configure, and a second
  `Memory` in the same process collides with it. `MEM0_DIR` moves that home, so separate processes share
  nothing.
- **Start conservatively and watch their error rate.** Our speed must not cost their quality. The run prints
  any Mem0-side ingestion failures; if they appear, lower the worker count before the number is baked into a
  reference.

Turning down Mem0's reasoning effort would be faster still and is **deliberately not done**: that changes the
competitor's own configuration, which is the one thing a parity comparison must not do. Parallelism is ours
to choose; their settings are not.

**Each run gets its own store root.** Mem0's local store outlives the process while the harness's scope
counter restarts at 1 in each one, so two runs sharing a root write different questions into the same scope
and then search them together — measured in a live store, one scope holding 558 memories from one day's run
and 635 from the next. `--mem0-run-dir` defaults to a fresh timestamped directory for exactly that reason;
pass it explicitly only to resume.

**Interruptions cost the unfinished questions, not the run.** Each finished question's context is appended to
`contexts.jsonl` under the run directory as soon as it lands, and re-running with the same `--mem0-run-dir`
skips whatever is already there. Alongside it, `heartbeat.json` is rewritten every thirty seconds with what
is in flight, how long it has been going and an ETA — so a run that has died is distinguishable from one that
is merely slow, which is the question two lost runs could not answer.

For a run measured in hours, start it detached rather than inside a terminal session you might close.

Mem0 has a refusal of its own. Its search is a hybrid — semantic, BM25 keyword, and an entity boost — and the
two non-semantic legs are optional dependencies that degrade to a no-op with a log line and no error. A bare
install therefore scores it on one leg of three against Lore's full hybrid: an asymmetry in our favour that is
invisible in the result, and one the sanity floor cannot catch, because it yields a *plausibly* low number
rather than an absurd one. `uv sync --extra competitors` installs what those legs need; the run probes them
and refuses to start if either is dead.

## License

Apache-2.0. The LongMemEval dataset is the property of its authors; see the upstream repository for its terms.
