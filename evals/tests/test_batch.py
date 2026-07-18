import json
from collections.abc import Callable
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest

from longmemeval.batch import (
    AnthropicBatchProvider,
    BatchError,
    BatchRequest,
    BatchStatus,
    OpenAIBatchProvider,
    ResumeStore,
    run_batch,
)


class FakeBatchProvider:
    """A BatchProvider fake: applies a pure function to each request. `ready_after` controls how many polls
    until READY; `drop` custom_ids are omitted from collect (forcing the sync-fallback path)."""

    def __init__(
        self, name: str, fn: Callable[[BatchRequest], str], *, ready_after: int = 1, drop: tuple[str, ...] = ()
    ) -> None:
        self.name = name
        self._fn = fn
        self.submitted: list[list[BatchRequest]] = []
        self.completed_one: list[str] = []
        self._ready_after = ready_after
        self._drop = set(drop)
        self._batches: dict[str, list[BatchRequest]] = {}
        self._polls = 0
        self._counter = 0

    def submit(self, requests: Any) -> str:
        self._counter += 1
        batch_id = f"{self.name}-{self._counter}"
        self._batches[batch_id] = list(requests)
        self.submitted.append(list(requests))
        self._polls = 0
        return batch_id

    def poll(self, batch_id: str) -> BatchStatus:
        self._polls += 1
        return BatchStatus.READY if self._polls >= self._ready_after else BatchStatus.PENDING

    def collect(self, batch_id: str) -> dict[str, str]:
        return {r.custom_id: self._fn(r) for r in self._batches[batch_id] if r.custom_id not in self._drop}

    def complete_one(self, request: BatchRequest) -> str:
        self.completed_one.append(request.custom_id)
        return self._fn(request)


def _reqs() -> list[BatchRequest]:
    return [BatchRequest(custom_id="a", system="", prompt="Pa"), BatchRequest(custom_id="b", system="", prompt="Pb")]


def test_run_batch_happy_path_joins_on_custom_id() -> None:
    provider = FakeBatchProvider("p", lambda r: r.prompt.upper())
    outcome = run_batch(provider, _reqs(), sleep=lambda s: None)
    assert outcome.results == {"a": "PA", "b": "PB"}
    assert outcome.fallback_ids == []  # nothing dropped -> no fallback
    assert not provider.completed_one


def test_run_batch_empty_makes_no_calls() -> None:
    provider = FakeBatchProvider("p", lambda r: r.prompt)
    outcome = run_batch(provider, [], sleep=lambda s: None)
    assert outcome.results == {}
    assert outcome.fallback_ids == []
    assert not provider.submitted


def test_run_batch_polls_until_ready() -> None:
    provider = FakeBatchProvider("p", lambda r: r.prompt, ready_after=3)
    sleeps: list[float] = []
    run_batch(provider, _reqs(), poll_interval=0.5, sleep=sleeps.append)
    assert sleeps == [0.5, 0.5]  # two pending polls, then ready


def test_run_batch_sync_fallback_reports_dropped_items() -> None:
    provider = FakeBatchProvider("p", lambda r: r.prompt.upper(), drop=("b",))
    outcome = run_batch(provider, _reqs(), sleep=lambda s: None)
    assert outcome.results == {"a": "PA", "b": "PB"}  # 'b' filled synchronously
    assert provider.completed_one == ["b"]  # only the dropped one went through complete_one
    assert outcome.fallback_ids == ["b"]  # and it is REPORTED, so a caller can count it as a sync (full-rate) call


def test_run_batch_resumes_a_persisted_batch_without_resubmitting(tmp_path: Path) -> None:
    provider = FakeBatchProvider("p", lambda r: r.prompt.upper())
    reqs = _reqs()
    prior_id = provider.submit(reqs)  # a prior "process" submitted; the id was persisted before a crash
    resume = ResumeStore(tmp_path / "resume.json")
    resume.put("p", prior_id)

    outcome = run_batch(provider, reqs, resume=resume, sleep=lambda s: None)
    assert outcome.results == {"a": "PA", "b": "PB"}
    assert len(provider.submitted) == 1  # reused the persisted batch — did NOT submit again
    assert resume.get("p") is None  # cleared after a successful collect


def test_run_batch_resume_key_overrides_provider_name(tmp_path: Path) -> None:
    provider = FakeBatchProvider("p", lambda r: r.prompt.upper())
    resume = ResumeStore(tmp_path / "resume.json")
    run_batch(provider, _reqs(), resume=resume, resume_key="p.t1", sleep=lambda s: None)
    # The persisted id lived under the namespaced key, not the bare provider name — so a sibling trial's batch
    # under "p.t0" could never be grafted onto this one.
    assert resume.get("p.t1") is None  # cleared on success
    assert resume.get("p") is None


def test_run_batch_clear_on_success_false_keeps_the_id(tmp_path: Path) -> None:
    provider = FakeBatchProvider("p", lambda r: r.prompt.upper())
    resume = ResumeStore(tmp_path / "resume.json")
    run_batch(provider, _reqs(), resume=resume, clear_on_success=False, sleep=lambda s: None)
    # A caller running a later phase under the same run can defer clearing; a crash in between re-collects.
    assert resume.get("p") is not None


def test_run_batch_failed_clears_resume_so_a_rerun_submits_fresh(tmp_path: Path) -> None:
    class Failing(FakeBatchProvider):
        def poll(self, batch_id: str) -> BatchStatus:
            return BatchStatus.FAILED

    provider = Failing("p", lambda r: r.prompt)
    resume = ResumeStore(tmp_path / "resume.json")
    with pytest.raises(BatchError):
        run_batch(provider, _reqs(), resume=resume, sleep=lambda s: None)
    # Terminal failure must NOT wedge: the dead id is cleared so a re-run submits a fresh batch instead of
    # re-polling the same dead one forever.
    assert resume.get("p") is None


def test_run_batch_timeout_keeps_resume_id(tmp_path: Path) -> None:
    provider = FakeBatchProvider("p", lambda r: r.prompt, ready_after=999)
    resume = ResumeStore(tmp_path / "resume.json")
    with pytest.raises(BatchError):
        run_batch(provider, _reqs(), resume=resume, max_polls=3, sleep=lambda s: None)
    # Not-finished-yet is NOT terminal — the id is kept so a re-run resumes the (possibly still-running) batch.
    assert resume.get("p") is not None


# --- OpenAI provider on-wire shape -------------------------------------------------------------------------


class _FakeOpenAIFiles:
    def __init__(self) -> None:
        self.uploaded_jsonl = ""

    def create(self, *, file: Any, purpose: str) -> Any:
        assert purpose == "batch"
        self.uploaded_jsonl = file.read().decode("utf-8")
        return SimpleNamespace(id="file-in")

    def content(self, file_id: str) -> Any:
        line = json.dumps(
            {"custom_id": "a", "response": {"status_code": 200, "body": {"choices": [{"message": {"content": "yes"}}]}}}
        )
        return SimpleNamespace(text=line)


class _FakeOpenAIBatches:
    def __init__(self, status: str = "completed", output_file_id: str | None = "file-out") -> None:
        self.created: dict[str, Any] = {}
        self._status = status
        self._output_file_id = output_file_id

    def create(self, *, input_file_id: str, endpoint: str, completion_window: str) -> Any:
        self.created = {"input_file_id": input_file_id, "endpoint": endpoint, "completion_window": completion_window}
        return SimpleNamespace(id="batch-1")

    def retrieve(self, batch_id: str) -> Any:
        return SimpleNamespace(status=self._status, output_file_id=self._output_file_id)


class _FakeOpenAIChat:
    def __init__(self) -> None:
        self.completions = self

    def create(self, **kwargs: Any) -> Any:
        return SimpleNamespace(choices=[SimpleNamespace(message=SimpleNamespace(content="no"))])


class FakeOpenAI:
    def __init__(self, status: str = "completed") -> None:
        self.files = _FakeOpenAIFiles()
        self.batches = _FakeOpenAIBatches(status)
        self.chat = _FakeOpenAIChat()


def test_openai_provider_submits_jsonl_and_collects() -> None:
    client = FakeOpenAI()
    provider = OpenAIBatchProvider(client, "gpt-4o", name="judge", max_tokens=10)  # type: ignore[arg-type]
    batch_id = provider.submit([BatchRequest("a", system="sys", prompt="grade this")])
    assert batch_id == "batch-1"
    # One JSONL line, the required fields, and the system+user messages in order.
    line = json.loads(client.files.uploaded_jsonl.strip())
    assert line["custom_id"] == "a"
    assert line["method"] == "POST"
    assert line["url"] == "/v1/chat/completions"
    assert line["body"]["model"] == "gpt-4o"
    assert line["body"]["messages"] == [
        {"role": "system", "content": "sys"},
        {"role": "user", "content": "grade this"},
    ]
    assert client.batches.created["completion_window"] == "24h"
    assert provider.poll("batch-1") is BatchStatus.READY
    assert provider.collect("batch-1") == {"a": "yes"}
    assert provider.complete_one(BatchRequest("z", system="", prompt="x")) == "no"


def test_openai_poll_maps_status() -> None:
    def status_of(s: str) -> BatchStatus:
        return OpenAIBatchProvider(FakeOpenAI(s), "gpt-4o").poll("b")  # type: ignore[arg-type]

    assert status_of("in_progress") is BatchStatus.PENDING
    assert status_of("completed") is BatchStatus.READY
    assert status_of("expired") is BatchStatus.READY  # partials still collectable
    assert status_of("failed") is BatchStatus.FAILED


# --- Anthropic provider on-wire shape ----------------------------------------------------------------------


def _text_block(text: str) -> Any:
    return SimpleNamespace(type="text", text=text)


class _FakeAnthropicBatches:
    def __init__(self, status: str = "ended") -> None:
        self.created_requests: list[dict[str, Any]] = []
        self._status = status

    def create(self, *, requests: Any) -> Any:
        self.created_requests = list(requests)
        return SimpleNamespace(id="msgbatch-1")

    def retrieve(self, batch_id: str) -> Any:
        return SimpleNamespace(processing_status=self._status)

    def results(self, batch_id: str) -> Any:
        yield SimpleNamespace(
            custom_id="a",
            result=SimpleNamespace(type="succeeded", message=SimpleNamespace(content=[_text_block("A")])),
        )
        yield SimpleNamespace(custom_id="b", result=SimpleNamespace(type="errored"))  # dropped from collect


class _FakeAnthropicMessages:
    def __init__(self, status: str = "ended") -> None:
        self.batches = _FakeAnthropicBatches(status)

    def create(self, **kwargs: Any) -> Any:
        return SimpleNamespace(content=[_text_block("sync-fallback")])


class FakeAnthropic:
    def __init__(self, status: str = "ended") -> None:
        self.messages = _FakeAnthropicMessages(status)


def test_anthropic_provider_submits_inline_and_streams_results() -> None:
    client = FakeAnthropic()
    provider = AnthropicBatchProvider(client, "claude-haiku-4-5", name="answerer", max_tokens=256)  # type: ignore[arg-type]
    batch_id = provider.submit(
        [BatchRequest("a", system="sys", prompt="answer"), BatchRequest("b", system="", prompt="q")]
    )
    assert batch_id == "msgbatch-1"
    reqs = client.messages.batches.created_requests
    assert reqs[0]["custom_id"] == "a"
    assert reqs[0]["params"]["model"] == "claude-haiku-4-5"
    assert reqs[0]["params"]["max_tokens"] == 256
    assert reqs[0]["params"]["system"] == "sys"
    assert reqs[0]["params"]["messages"] == [{"role": "user", "content": "answer"}]
    assert "system" not in reqs[1]["params"]  # empty system prompt is omitted
    assert provider.poll("msgbatch-1") is BatchStatus.READY
    # Only the succeeded item is collected; the errored one is left for the caller's sync fallback.
    assert provider.collect("msgbatch-1") == {"a": "A"}
    assert provider.complete_one(BatchRequest("z", system="", prompt="x")) == "sync-fallback"


def test_anthropic_poll_pending_until_ended() -> None:
    provider = AnthropicBatchProvider(FakeAnthropic("in_progress"), "m")  # type: ignore[arg-type]
    assert provider.poll("b") is BatchStatus.PENDING
