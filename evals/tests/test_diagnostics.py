from typing import Any

import pytest

from longmemeval.diagnostics import IN_CONTEXT, MISSING, UNPROBEABLE, diagnose, found, needle, summarize


@pytest.mark.parametrize(
    ("gold", "want"),
    [
        ("Transcriptionist.", "transcriptionist"),
        ("7 days. 8 days (including the last day) is also acceptable.", "7 days"),
        ("'The Nightingale' by Kristin Hannah", "the nightingale by kristin hannah"),
        ("25", "25"),
        ("", None),
        ("The user would prefer suggestions of Sony-compatible accessories or high-quality photography gear.", None),
    ],
)
def test_needle_takes_the_value_and_declines_rubrics(gold: str, want: str | None) -> None:
    assert needle(gold) == want


def test_a_number_is_not_found_inside_a_larger_one() -> None:
    """The mistake this guards against was made by hand once: a probe for "25" counted every "250" as evidence."""
    assert found("25", "User has added 25 new postcards")
    assert not found("25", "User raised 250 dollars")
    assert not found("4", "budget of 400")
    assert found("transcriptionist", "1. Bookkeeper 7. Transcriptionist")
    # A title is found whatever quotes the memory stored it with.
    assert found("the nightingale by kristin hannah", 'finished reading "The Nightingale" by Kristin Hannah')


def _report(rows: list[tuple[str, str, str, list[bool]]]) -> dict[str, Any]:
    """rows: (question_id, question_type, gold, per-trial correctness)."""
    n_trials = len(rows[0][3])
    trials = [
        [
            {"question_id": qid, "question_type": qtype, "gold_answer": gold, "correct": marks[t]}
            for qid, qtype, gold, marks in rows
        ]
        for t in range(n_trials)
    ]
    return {"variance_answer": {"trials": trials}}


def test_wrong_answers_are_split_by_where_the_evidence_was() -> None:
    report = _report(
        [
            ("q-right", "single-session-user", "Luna", [True, True, True]),
            ("q-lost", "single-session-assistant", "Transcriptionist", [False, False, False]),
            ("q-misread", "temporal-reasoning", "21 days", [False, True, False]),
            ("q-rubric", "single-session-preference", "x" * 200, [False, False, False]),
            ("q-skip_abs", "knowledge-update", "n/a", [True, True, True]),
        ]
    )
    contexts = {
        "q-right": "[1] User's cat is named Luna",
        "q-lost": "[1] User asked about jobs",
        "q-misread": "[1] (2023-01-10) started the book\n[2] took 21 days to finish today",
        "q-rubric": "[1] anything",
    }
    by_id = {d.question_id: d for d in diagnose(report, contexts)}

    assert "q-skip_abs" not in by_id, "abstention items have no value to look for"
    assert by_id["q-right"].failure is None
    assert by_id["q-lost"].evidence == MISSING and by_id["q-lost"].failure == "not in context"
    assert by_id["q-misread"].evidence == IN_CONTEXT and by_id["q-misread"].failure == "in context"
    assert by_id["q-rubric"].evidence == UNPROBEABLE

    summary = summarize(list(by_id.values()))
    assert (summary.correct, summary.questions) == (1, 4)
    assert summary.failures == {"not in context": 1, "in context": 1, "unprobeable": 1}
    # q-misread's context has one dated line and one relative one ("today").
    assert by_id["q-misread"].absolute_lines == 1 and by_id["q-misread"].relative_lines == 1
