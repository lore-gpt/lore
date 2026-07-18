from pathlib import Path

from longmemeval.loader import deterministic_subset, load_questions, parse_question

FIXTURE = Path(__file__).resolve().parents[1] / "fixtures" / "clean_room.json"


def test_loads_clean_room_fixture() -> None:
    questions = load_questions(FIXTURE)
    assert {q.question_id for q in questions} == {"cr_ssu_01", "cr_ms_01", "cr_ssu_02_abs"}
    multi = next(q for q in questions if q.question_id == "cr_ms_01")
    assert multi.question_type == "multi-session"
    assert len(multi.sessions) == 3
    first_turn = multi.sessions[0].turns[0]
    assert first_turn.role == "user"
    assert first_turn.has_answer is True
    assert multi.sessions[0].date == "2031/02/02 (Sun) 08:30"


def test_abstention_flag_follows_the_id_suffix() -> None:
    by_id = {q.question_id: q for q in load_questions(FIXTURE)}
    assert by_id["cr_ssu_02_abs"].is_abstention is True
    assert by_id["cr_ssu_01"].is_abstention is False


def test_deterministic_subset_is_stable_and_sized() -> None:
    questions = load_questions(FIXTURE)
    first = [q.question_id for q in deterministic_subset(questions, 2)]
    second = [q.question_id for q in deterministic_subset(questions, 2)]
    assert first == second
    assert len(first) == 2
    # n >= total returns every question, id-sorted.
    everything = deterministic_subset(questions, 99)
    assert [q.question_id for q in everything] == sorted(q.question_id for q in questions)


def test_parse_tolerates_missing_session_ids_and_dates() -> None:
    question = parse_question(
        {
            "question_id": "x",
            "question_type": "single-session-user",
            "question": "?",
            "answer": "a",
            "haystack_sessions": [[{"role": "user", "content": "hi"}]],
        }
    )
    assert question.sessions[0].session_id == "session_0"
    assert question.sessions[0].date == ""
    assert question.sessions[0].turns[0].has_answer is False
