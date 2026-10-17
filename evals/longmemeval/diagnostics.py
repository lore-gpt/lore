"""Per-question diagnostics for a development run: where did each wrong answer go wrong?

A score says how many questions were missed; it does not say why, and the two common reasons call for opposite
fixes. Either the evidence never reached the answerer — it was not distilled, or it was distilled and not
retrieved — or it was in front of the answerer and the answer was still wrong. This module separates the two by
checking whether the gold answer appears in the context the system returned for the question, and it measures how
much of that context is anchored in time.

It reads only what a run already wrote — the report and the checkpointed contexts — so it needs no keys and no
database. The gold-answer check is a heuristic and says so: it only probes short answers, matches numbers on word
boundaries (so "25" is not found inside "250"), and marks long rubric-style answers as not probeable rather than
guessing. Use it on development subsets only; looking at a verdict subset this way is what sealing prevents.
"""

from __future__ import annotations

import re
from collections import Counter, defaultdict
from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from typing import Any

# A gold answer longer than this is a rubric ("The user would prefer suggestions that…"), not a value to look for.
MAX_PROBE_CHARS = 80

PACK_LINE = re.compile(r"^\s*\[\d+\]\s")
RELATIVE_TIME = re.compile(
    r"\b(?:today|yesterday|tomorrow|tonight|recently|this (?:morning|week|weekend|month|year)|"
    r"last (?:night|week|weekend|month|year))\b",
    re.IGNORECASE,
)
ABSOLUTE_DATE = re.compile(
    r"\b(?:19|20)\d{2}[-/]\d{1,2}[-/]\d{1,2}\b|"
    r"\b(?:january|february|march|april|may|june|july|august|september|october|november|december)\s+\d{1,2}"
    r"(?:st|nd|rd|th)?,?\s+(?:19|20)\d{2}\b",
    re.IGNORECASE,
)
_NUMERIC = re.compile(r"[\d$%.,:/-]+")
# Quotes are dropped from both sides before matching: a title may be stored with single quotes, double quotes,
# typographic quotes or none at all, and none of that is the value being looked for.
_QUOTES = str.maketrans("", "", "'\"" + "".join(map(chr, (0x2018, 0x2019, 0x201C, 0x201D))))

IN_CONTEXT = "in_context"
MISSING = "missing"
UNPROBEABLE = "unprobeable"


def needle(gold: str) -> str | None:
    """The part of a gold answer worth searching for, lowercased — or None when the answer is not a probeable value.

    Takes the first sentence ("7 days. 8 days (including the last day) is also acceptable." -> "7 days"), drops
    trailing punctuation, and removes quote characters."""
    text = gold.strip()
    if not text or len(text) > MAX_PROBE_CHARS:
        return None
    first = re.split(r"(?<=[.!?])\s+", text, maxsplit=1)[0]
    first = first.strip().rstrip(".!?").translate(_QUOTES).strip()
    return first.lower() or None


def found(probe: str, context: str) -> bool:
    """Whether a probe appears in a context. Numeric probes match on word boundaries so a value is not found inside
    a larger one; text probes match as case-insensitive substrings."""
    haystack = context.lower().translate(_QUOTES)
    if _NUMERIC.fullmatch(probe):
        return re.search(rf"(?<![\w.]){re.escape(probe)}(?![\w])", haystack) is not None
    return probe in haystack


@dataclass(frozen=True, slots=True)
class QuestionDiagnosis:
    question_id: str
    question_type: str
    correct: bool  # majority of trials
    evidence: str  # IN_CONTEXT | MISSING | UNPROBEABLE
    pack_lines: int
    relative_lines: int
    absolute_lines: int

    @property
    def failure(self) -> str | None:
        """For a wrong answer: 'not in context' (the evidence never reached the answerer), 'in context' (it did,
        and the answer was still wrong), or 'unprobeable'. None for a correct answer."""
        if self.correct:
            return None
        return {IN_CONTEXT: "in context", MISSING: "not in context"}.get(self.evidence, "unprobeable")


def diagnose(report: Mapping[str, Any], contexts: Mapping[str, str]) -> list[QuestionDiagnosis]:
    """Diagnose every question in a written report against the contexts its run returned. Abstention items are
    skipped: their gold behaviour is to decline, so there is no value to look for."""
    trials: dict[str, list[Mapping[str, Any]]] = defaultdict(list)
    for trial in report["variance_answer"]["trials"]:
        for verdict in trial:
            trials[str(verdict["question_id"])].append(verdict)
    out: list[QuestionDiagnosis] = []
    for qid in sorted(trials):
        if qid.endswith("_abs"):
            continue
        verdicts = trials[qid]
        correct = sum(bool(v["correct"]) for v in verdicts) * 2 > len(verdicts)
        context = contexts.get(qid, "")
        probe = needle(str(verdicts[0]["gold_answer"]))
        evidence = UNPROBEABLE if probe is None else (IN_CONTEXT if found(probe, context) else MISSING)
        lines = [ln for ln in context.splitlines() if PACK_LINE.match(ln)]
        out.append(
            QuestionDiagnosis(
                question_id=qid,
                question_type=str(verdicts[0]["question_type"]),
                correct=correct,
                evidence=evidence,
                pack_lines=len(lines),
                relative_lines=sum(1 for ln in lines if RELATIVE_TIME.search(ln)),
                absolute_lines=sum(1 for ln in lines if ABSOLUTE_DATE.search(ln)),
            )
        )
    return out


@dataclass(frozen=True, slots=True)
class Summary:
    """Totals over a diagnosed run."""

    questions: int
    correct: int
    failures: Mapping[str, int]  # failure kind -> count
    by_type: Mapping[str, tuple[int, int]]  # question type -> (correct, total)
    pack_lines: int
    relative_lines: int
    absolute_lines: int

    @property
    def relative_share(self) -> float:
        return self.relative_lines / self.pack_lines if self.pack_lines else 0.0

    @property
    def absolute_share(self) -> float:
        return self.absolute_lines / self.pack_lines if self.pack_lines else 0.0


def summarize(diagnoses: Sequence[QuestionDiagnosis]) -> Summary:
    by_type: dict[str, list[int]] = defaultdict(lambda: [0, 0])
    for d in diagnoses:
        by_type[d.question_type][1] += 1
        by_type[d.question_type][0] += int(d.correct)
    return Summary(
        questions=len(diagnoses),
        correct=sum(d.correct for d in diagnoses),
        failures=dict(Counter(d.failure for d in diagnoses if d.failure is not None)),
        by_type={t: (c, n) for t, (c, n) in sorted(by_type.items())},
        pack_lines=sum(d.pack_lines for d in diagnoses),
        relative_lines=sum(d.relative_lines for d in diagnoses),
        absolute_lines=sum(d.absolute_lines for d in diagnoses),
    )
