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
uv run python -m examples.smoke --split s --n 50 --systems lore \
  --lore-provision-cmd 'docker compose -f /path/to/docker-compose.yml run --rm --no-deps lore-server provision --project {name}' \
  --lore-poll-timeout 300
```

Two things to know before running it:

- **Use a throwaway database.** Every ingestion leaves a project behind and nothing reclaims them; that is
  deliberate (a cleanup path would be machinery for a measurement that runs on a disposable stack). Do not
  point a real deployment at this.
- **Raise `--lore-poll-timeout`.** The default 60s is not enough at LongMemEval scale — a 514-event question
  was measured taking 94-177s to distil, and one timeout fails the whole run.

## License

Apache-2.0. The LongMemEval dataset is the property of its authors; see the upstream repository for its terms.
