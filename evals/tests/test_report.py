import json
from pathlib import Path

from longmemeval._types import Provenance, Verdict
from longmemeval.report import SystemSummary, TrialReport, aggregate


def _provenance(n: int) -> Provenance:
    return Provenance(
        dataset="xiaowu0162/longmemeval-cleaned",
        dataset_revision="abc123",
        split="s",
        n=n,
        judge_model="gpt-4o-2024-08-06",
        judge_prompt_hash="deadbeef" * 8,
        answerer_model="claude-haiku-4-5",
        extraction_model="claude-haiku-4-5",
        generated_at="2031-01-01T00:00:00Z",
    )


def _verdict(qid: str, qtype: str, correct: bool) -> Verdict:
    return Verdict(
        question_id=qid,
        question_type=qtype,
        generated_answer="a",
        gold_answer="g",
        correct=correct,
        judge_model="gpt-4o-2024-08-06",
        rubric_version="longmemeval-official-v1",
    )


def test_accuracy_overall_and_by_type() -> None:
    verdicts = (
        _verdict("q1", "multi-session", True),
        _verdict("q2", "multi-session", False),
        _verdict("q3", "temporal-reasoning", True),
    )
    report = TrialReport("lore", _provenance(3), verdicts, cache_hit_rate=0.0)
    assert report.n == 3
    assert report.accuracy == 2 / 3
    assert report.accuracy_by_type() == {"multi-session": 0.5, "temporal-reasoning": 1.0}


def test_artifact_embeds_the_provenance_chain(tmp_path: Path) -> None:
    report = TrialReport("lore", _provenance(1), (_verdict("q1", "multi-session", True),), cache_hit_rate=1.0)
    out = tmp_path / "reports" / "run.json"
    report.write(out)
    data = json.loads(out.read_text("utf-8"))
    prov = data["provenance"]
    # Every leg of the same-judge reproducibility chain is recorded.
    assert prov["dataset_revision"] == "abc123"
    assert prov["judge_model"] == "gpt-4o-2024-08-06"
    assert prov["answerer_model"] == "claude-haiku-4-5"
    assert prov["extraction_model"] == "claude-haiku-4-5"
    assert len(prov["judge_prompt_hash"]) == 64
    assert data["accuracy"] == 1.0


def test_system_summary_mean_and_std() -> None:
    trials = [
        TrialReport("lore", _provenance(2), (_verdict("q1", "t", True), _verdict("q2", "t", True)), 0.0),
        TrialReport("lore", _provenance(2), (_verdict("q1", "t", True), _verdict("q2", "t", False)), 0.0),
    ]
    summary = aggregate("lore", trials)
    assert summary.mean == 0.75  # (1.0 + 0.5) / 2
    assert summary.std > 0.0
    # A single trial has zero variance, not an error.
    assert SystemSummary("lore", (0.9,)).std == 0.0
