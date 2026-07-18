"""The LongMemEval judge. The grading prompts are the official LongMemEval prompts, VERBATIM (from
src/evaluation/evaluate_qa.py, get_anscheck_prompt) — comparability to the published methodology needs both the
model AND the prompt to match. The default model is the official GPT-4o snapshot. The judge is a harness-only
dependency; it is not part of the Lore product."""

from __future__ import annotations

import hashlib
from collections.abc import Callable, Sequence
from typing import TYPE_CHECKING

from ._types import Question
from .batch import BatchProvider, BatchRequest, ResumeStore, run_batch
from .cache import CacheKey, JudgeCache, JudgeDecision, hash_answer
from .stats import RunStats, estimate_tokens

if TYPE_CHECKING:
    from openai import OpenAI

# The official LongMemEval judge model snapshot.
DEFAULT_JUDGE_MODEL = "gpt-4o-2024-08-06"
# A semantic label for THIS embedding of the official prompts; bump it if the embedded prompts change, so the
# cache re-judges. The exact prompt bytes are pinned separately by PROMPT_HASH (recorded in the report).
RUBRIC_VERSION = "longmemeval-official-v1"

# --- Official LongMemEval grading prompts, verbatim (get_anscheck_prompt) ---------------------------------
_DEFAULT = (
    "I will give you a question, a correct answer, and a response from a model. Please answer yes if the "
    "response contains the correct answer. Otherwise, answer no. If the response is equivalent to the correct "
    "answer or contains all the intermediate steps to get the correct answer, you should also answer yes. If "
    "the response only contains a subset of the information required by the answer, answer no. \n\nQuestion: "
    "{}\n\nCorrect Answer: {}\n\nModel Response: {}\n\nIs the model response correct? Answer yes or no only."
)
_TEMPORAL = (
    "I will give you a question, a correct answer, and a response from a model. Please answer yes if the "
    "response contains the correct answer. Otherwise, answer no. If the response is equivalent to the correct "
    "answer or contains all the intermediate steps to get the correct answer, you should also answer yes. If "
    "the response only contains a subset of the information required by the answer, answer no. In addition, do "
    "not penalize off-by-one errors for the number of days. If the question asks for the number of "
    "days/weeks/months, etc., and the model makes off-by-one errors (e.g., predicting 19 days when the answer "
    "is 18), the model's response is still correct. \n\nQuestion: {}\n\nCorrect Answer: {}\n\nModel Response: "
    "{}\n\nIs the model response correct? Answer yes or no only."
)
_KNOWLEDGE_UPDATE = (
    "I will give you a question, a correct answer, and a response from a model. Please answer yes if the "
    "response contains the correct answer. Otherwise, answer no. If the response contains some previous "
    "information along with an updated answer, the response should be considered as correct as long as the "
    "updated answer is the required answer.\n\nQuestion: {}\n\nCorrect Answer: {}\n\nModel Response: {}\n\nIs "
    "the model response correct? Answer yes or no only."
)
_PREFERENCE = (
    "I will give you a question, a rubric for desired personalized response, and a response from a model. "
    "Please answer yes if the response satisfies the desired response. Otherwise, answer no. The model does "
    "not need to reflect all the points in the rubric. The response is correct as long as it recalls and "
    "utilizes the user's personal information correctly.\n\nQuestion: {}\n\nRubric: {}\n\nModel Response: "
    "{}\n\nIs the model response correct? Answer yes or no only."
)
_ABSTENTION = (
    "I will give you an unanswerable question, an explanation, and a response from a model. Please answer yes "
    "if the model correctly identifies the question as unanswerable. The model could say that the information "
    "is incomplete, or some other information is given but the asked information is not.\n\nQuestion: "
    "{}\n\nExplanation: {}\n\nModel Response: {}\n\nDoes the model correctly identify the question as "
    "unanswerable? Answer yes or no only."
)
# ----------------------------------------------------------------------------------------------------------


def build_grading_prompt(task: str, question: str, answer: str, response: str, *, abstention: bool) -> str:
    """The official get_anscheck_prompt, verbatim — including its fail-fast dispatch. `abstention` uses the
    unanswerable template regardless of task; otherwise the per-type template is chosen, and an unknown
    non-abstention task raises (as upstream does), so a mislabelled or drifted question is caught loudly rather
    than silently graded under the wrong rubric."""
    if abstention:
        template = _ABSTENTION
    elif task in ("single-session-user", "single-session-assistant", "multi-session"):
        template = _DEFAULT
    elif task == "temporal-reasoning":
        template = _TEMPORAL
    elif task == "knowledge-update":
        template = _KNOWLEDGE_UPDATE
    elif task == "single-session-preference":
        template = _PREFERENCE
    else:
        raise ValueError(f"unknown LongMemEval task type: {task!r}")
    return template.format(question, answer, response)


def _prompt_hash() -> str:
    joined = "\x00".join([_DEFAULT, _TEMPORAL, _KNOWLEDGE_UPDATE, _PREFERENCE, _ABSTENTION])
    return hashlib.sha256(joined.encode("utf-8")).hexdigest()


# The pinned identity of the embedded prompts; recorded in every run's provenance so a score is reproducible.
PROMPT_HASH = _prompt_hash()


class Judge:
    """Grade an answer against its gold answer with the official prompt, caching each decision. `complete` maps
    a prompt to the raw model response text; the OpenAI wiring is injected via `openai_judge` so tests need no
    live client."""

    def __init__(self, complete: Callable[[str], str], model: str, rubric_version: str = RUBRIC_VERSION) -> None:
        self._complete = complete
        self.model = model
        self.rubric_version = rubric_version
        self.prompt_hash = PROMPT_HASH

    def _key(self, question: Question, answer: str, variant: str = "") -> CacheKey:
        return CacheKey(
            question_id=question.question_id,
            answer_hash=hash_answer(answer),
            judge_model=self.model,
            rubric_version=self.rubric_version,
            variant=variant,
        )

    def _prompt(self, question: Question, answer: str) -> str:
        return build_grading_prompt(
            question.question_type,
            question.question,
            question.answer,
            answer,
            abstention=question.is_abstention,
        )

    def _decision(self, raw: str) -> JudgeDecision:
        # Official label parsing: the judge is instructed to answer yes/no only.
        return JudgeDecision(
            correct="yes" in raw.lower(),
            reasoning=raw.strip(),
            judge_model=self.model,
            rubric_version=self.rubric_version,
        )

    def score(self, question: Question, answer: str, cache: JudgeCache) -> JudgeDecision:
        key = self._key(question, answer)
        cached = cache.get(key)
        if cached is not None:
            return cached
        decision = self._decision(self._complete(self._prompt(question, answer)))
        cache.put(key, decision)
        return decision

    def score_many(
        self,
        pairs: Sequence[tuple[Question, str]],
        cache: JudgeCache,
        *,
        batch: BatchProvider | None = None,
        resume: ResumeStore | None = None,
        resume_key: str | None = None,
        sleep: Callable[[float], None] | None = None,
        stats: RunStats | None = None,
        variant: str = "",
    ) -> dict[str, JudgeDecision]:
        """Grade many (question, answer) pairs, returning question_id -> decision. Cache hits are served
        without a model call; only cache misses are judged. With `batch=None` the misses are graded
        synchronously (one call each); with a batch provider they are graded in ONE Batch-API job (the same
        grading prompt bytes, matched back on question_id). Either way each fresh decision is cached, so a
        re-run judges only genuinely new (answer, rubric, variant) pairs. `variant` scopes the cache (a variance
        trial passes its index so each trial re-judges). `stats` records the judge call count by mode — a
        Batch-API item the batch dropped and had to fill synchronously is counted as a sync call (it bills at
        the full rate)."""
        decisions: dict[str, JudgeDecision] = {}
        misses: list[tuple[Question, str, CacheKey, str]] = []
        for question, answer in pairs:
            key = self._key(question, answer, variant)
            cached = cache.get(key)
            if cached is not None:
                decisions[question.question_id] = cached
                if stats is not None:
                    stats.cache_hits += 1
            else:
                misses.append((question, answer, key, self._prompt(question, answer)))

        if not misses:
            return decisions

        if stats is not None:
            for _, _, _, prompt in misses:
                stats.judge_input_tokens += estimate_tokens(prompt)

        if batch is None:
            if stats is not None:
                stats.judge_sync_calls += len(misses)
            for question, _answer, key, prompt in misses:
                decision = self._decision(self._complete(prompt))
                cache.put(key, decision)
                decisions[question.question_id] = decision
            return decisions

        requests = [
            BatchRequest(custom_id=question.question_id, system="", prompt=prompt)
            for question, _answer, _key, prompt in misses
        ]
        outcome = run_batch(batch, requests, resume=resume, resume_key=resume_key, sleep=sleep)
        if stats is not None:
            fallbacks = len(outcome.fallback_ids)
            stats.judge_batch_calls += len(misses) - fallbacks
            stats.judge_sync_calls += fallbacks
        for question, _answer, key, _prompt in misses:
            decision = self._decision(outcome.results[question.question_id])
            cache.put(key, decision)
            decisions[question.question_id] = decision
        return decisions


def openai_judge(client: OpenAI, model: str = DEFAULT_JUDGE_MODEL) -> Judge:
    """A Judge backed by the OpenAI Chat Completions API at temperature 0 (deterministic grading)."""

    def complete(prompt: str) -> str:
        response = client.chat.completions.create(
            model=model,
            messages=[{"role": "user", "content": prompt}],
            temperature=0,
            max_tokens=10,
        )
        return response.choices[0].message.content or ""

    return Judge(complete=complete, model=model)
