# LongMemEval harness

A reproducible harness that scores memory systems on [LongMemEval](https://github.com/xiaowu0162/LongMemEval)
through **one pinned, cached judge** — the *same-judge discipline*: every system is graded by the identical
judge model and the identical official prompts, so the numbers are comparable by construction rather than by
each vendor's own grader.

> This is the first slice: the harness machinery + a **Lore** adapter. Adapters for other memory systems
> (Mem0, Zep/Graphiti) plug into the same interface in a later slice — the parity comparison is a socket-plug,
> not a harness change.

## Design

- **Adapter** (`longmemeval.adapters.MemorySystem`) — the one interface every system implements: `ingest` a
  timestamped multi-session history, then `answer` a question from memory. The Lore adapter drives the server
  through the published Python SDK, waits for async distillation via the read-your-writes contract
  (`covered_seq`), packs context, and runs a separate answerer model.
- **Judge** — the official LongMemEval judge: a pinned OpenAI GPT-4o snapshot with the official per-type
  prompts, verbatim, so scores are comparable to the published methodology. The judge is a harness-only
  dependency; it is not part of the Lore product.
- **Cache** (`longmemeval.cache.JudgeCache`) — judge decisions keyed on
  `(question_id, answer_hash, judge_model, rubric_version)`, so an unchanged answer under an unchanged judge
  and rubric is never re-judged.
- **Reproducibility chain** — every run report embeds the dataset repo + pinned revision, the judge model +
  prompt hash, the answerer and extraction models, `n`, and the timestamp. Two runs are comparable only when
  the whole chain matches.

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
```

The real evaluation (against a running Lore stack, a real extractor, the GPT-4o judge, and the answerer) is
**secret-gated** and **staged** (a small sanity run before the smoke). It needs an Anthropic key (answerer +
Lore's extractor) and an OpenAI key (judge); costs are documented before the full run. Result artifacts
(scores, model outputs) are gitignored — a standalone number is not published until a parity slice lands.

## License

Apache-2.0. The LongMemEval dataset is the property of its authors; see the upstream repository for its terms.
