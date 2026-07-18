"""Batch execution: run ~hundreds of answerer or judge prompts through a provider's Batch API (OpenAI file
batches, Anthropic Message Batches) at ~half the synchronous rate. A provider exposes submit -> poll -> collect
plus a `complete_one` synchronous single-call used both as the sync path and as the per-item fallback when the
batch drops an individual request. All results are joined back on `custom_id` (order is never preserved by
either API), so the eval row a result belongs to is unambiguous.

Both real providers hold a live SDK client and are the ONLY place the openai/anthropic SDKs are touched, so the
module imports them lazily; the fakes in the tests implement the same tiny Protocol with no network or key."""

from __future__ import annotations

import io
import json
import time
from collections.abc import Callable, Sequence
from dataclasses import dataclass
from enum import Enum
from pathlib import Path
from typing import TYPE_CHECKING, Any, Protocol, cast

if TYPE_CHECKING:
    from anthropic import Anthropic
    from openai import OpenAI


class BatchStatus(str, Enum):
    """Where a submitted batch is in its lifecycle, collapsed to the three states the harness acts on."""

    PENDING = "pending"  # still validating / running
    READY = "ready"  # finished (fully, or expired-with-partials) — collect now
    FAILED = "failed"  # whole batch failed validation / was cancelled — do not collect


class BatchError(RuntimeError):
    """A batch failed as a whole (validation/cancellation) or never finished within the poll budget."""


@dataclass(frozen=True, slots=True)
class BatchRequest:
    """One request in a batch. `custom_id` is the join key back to the eval row — it must be unique within a
    batch and match `^[a-zA-Z0-9_-]{1,64}$` (the Anthropic constraint; question ids already satisfy it)."""

    custom_id: str
    system: str  # system prompt; "" means none
    prompt: str  # the user message


@dataclass(frozen=True, slots=True)
class BatchOutcome:
    """The result of run_batch: text per custom_id, plus the custom_ids that the batch dropped and were filled
    synchronously (they bill at the full rate, so a caller counting a mode split must count them as sync)."""

    results: dict[str, str]
    fallback_ids: list[str]


class BatchProvider(Protocol):
    """A model behind a Batch API. `name` is the resume-store key; `complete_one` is a synchronous single call
    with the SAME model/params as the batch, used as the sync path and the per-item fallback."""

    name: str

    def submit(self, requests: Sequence[BatchRequest]) -> str: ...
    def poll(self, batch_id: str) -> BatchStatus: ...
    def collect(self, batch_id: str) -> dict[str, str]: ...
    def complete_one(self, request: BatchRequest) -> str: ...


class ResumeStore:
    """A tiny JSON file mapping a provider name to its in-flight batch id, so a run that dies after submitting
    but before collecting resumes the SAME batch instead of paying for it twice. Cleared once collected."""

    def __init__(self, path: Path) -> None:
        self._path = path

    def _load(self) -> dict[str, str]:
        if not self._path.exists():
            return {}
        data: dict[str, str] = json.loads(self._path.read_text("utf-8"))
        return data

    def get(self, name: str) -> str | None:
        return self._load().get(name)

    def put(self, name: str, batch_id: str) -> None:
        data = self._load()
        data[name] = batch_id
        self._path.parent.mkdir(parents=True, exist_ok=True)
        self._path.write_text(json.dumps(data, indent=2, sort_keys=True), "utf-8")

    def clear(self, name: str) -> None:
        data = self._load()
        if data.pop(name, None) is not None:
            self._path.write_text(json.dumps(data, indent=2, sort_keys=True), "utf-8")


def run_batch(
    provider: BatchProvider,
    requests: Sequence[BatchRequest],
    *,
    resume: ResumeStore | None = None,
    resume_key: str | None = None,
    clear_on_success: bool = True,
    poll_interval: float = 60.0,
    max_polls: int = 24 * 60,  # tolerate the full 24h Batch-API window at one poll per minute
    sleep: Callable[[float], None] | None = None,
) -> BatchOutcome:
    """Submit the requests as one batch, poll until ready, collect, and fill any dropped individual request via
    the provider's synchronous `complete_one` (so a handful of errored/expired items never forces a full
    re-submit). Returns a BatchOutcome (text per custom_id + the synchronously-filled ids). An empty request
    list makes no calls.

    Durability: a `resume` store persists the batch id right after submit; on re-entry an existing id is polled
    and (re-)collected instead of re-submitting — re-collecting a finished batch is free, so a crash after
    submit never re-pays. `resume_key` overrides the store key (default `provider.name`); a multi-batch caller
    must namespace it (e.g. per trial) so batches never alias. `clear_on_success=False` keeps the id after a
    successful collect, so a caller that runs a LATER phase under the same run can defer clearing until that
    phase is also done (a crash in between then re-collects, not re-submits). A whole-batch FAILED clears the
    id before raising, so a re-run submits fresh rather than re-polling a dead batch forever."""
    key = resume_key if resume_key is not None else provider.name
    if not requests:
        return BatchOutcome(results={}, fallback_ids=[])

    do_sleep = sleep if sleep is not None else time.sleep
    batch_id = resume.get(key) if resume is not None else None
    if batch_id is None:
        batch_id = provider.submit(requests)
        if resume is not None:
            resume.put(key, batch_id)

    for _ in range(max_polls):
        status = provider.poll(batch_id)
        if status is BatchStatus.READY:
            break
        if status is BatchStatus.FAILED:
            # Terminal failure: drop the dead id so a re-run submits a fresh batch instead of wedging on it.
            if resume is not None:
                resume.clear(key)
            raise BatchError(f"{provider.name} batch {batch_id} failed")
        do_sleep(poll_interval)
    else:
        # Not-finished-yet: keep the id (the batch may still complete) so a re-run resumes it.
        raise BatchError(f"{provider.name} batch {batch_id} did not finish within {max_polls} polls")

    results = dict(provider.collect(batch_id))
    # Per-item fallback: any request the batch dropped (errored/expired) is completed synchronously so the
    # result set is always complete for the whole request list. These bill at the full rate (reported as sync).
    fallback_ids: list[str] = []
    for request in requests:
        if request.custom_id not in results:
            results[request.custom_id] = provider.complete_one(request)
            fallback_ids.append(request.custom_id)

    if resume is not None and clear_on_success:
        resume.clear(key)
    return BatchOutcome(results=results, fallback_ids=fallback_ids)


def _openai_messages(request: BatchRequest) -> list[dict[str, str]]:
    messages: list[dict[str, str]] = []
    if request.system:
        messages.append({"role": "system", "content": request.system})
    messages.append({"role": "user", "content": request.prompt})
    return messages


class OpenAIBatchProvider:
    """OpenAI Batch API over /v1/chat/completions. submit uploads a JSONL file (one request per line) and
    creates a 24h batch; collect downloads the output file and parses per-line results (matched on custom_id).
    `complete_one` is the equivalent single Chat Completions call."""

    def __init__(
        self,
        client: OpenAI,
        model: str,
        *,
        name: str = "openai",
        temperature: float = 0,
        max_tokens: int = 16,
    ) -> None:
        self._client = client
        self._model = model
        self.name = name
        self._temperature = temperature
        self._max_tokens = max_tokens

    def _body(self, request: BatchRequest) -> dict[str, Any]:
        return {
            "model": self._model,
            "messages": _openai_messages(request),
            "temperature": self._temperature,
            "max_tokens": self._max_tokens,
        }

    def submit(self, requests: Sequence[BatchRequest]) -> str:
        lines = [
            json.dumps(
                {"custom_id": r.custom_id, "method": "POST", "url": "/v1/chat/completions", "body": self._body(r)}
            )
            for r in requests
        ]
        payload = io.BytesIO(("\n".join(lines)).encode("utf-8"))
        uploaded = self._client.files.create(file=payload, purpose="batch")
        batch = self._client.batches.create(
            input_file_id=uploaded.id, endpoint="/v1/chat/completions", completion_window="24h"
        )
        return str(batch.id)

    def poll(self, batch_id: str) -> BatchStatus:
        status = self._client.batches.retrieve(batch_id).status
        if status == "completed":
            return BatchStatus.READY
        if status == "expired":  # partial results still available in the output file → collect + fallback
            return BatchStatus.READY
        if status in ("failed", "cancelled", "cancelling"):
            return BatchStatus.FAILED
        return BatchStatus.PENDING

    def collect(self, batch_id: str) -> dict[str, str]:
        batch = self._client.batches.retrieve(batch_id)
        output_file_id = batch.output_file_id
        if not output_file_id:
            return {}
        text = self._client.files.content(output_file_id).text
        results: dict[str, str] = {}
        for line in text.splitlines():
            if not line.strip():
                continue
            obj = json.loads(line)
            response = obj.get("response")
            if response and response.get("status_code") == 200:
                content = response["body"]["choices"][0]["message"]["content"]
                results[obj["custom_id"]] = content or ""
        return results

    def complete_one(self, request: BatchRequest) -> str:
        response = self._client.chat.completions.create(
            model=self._model,
            messages=cast("Any", _openai_messages(request)),  # runtime accepts role/content dicts
            temperature=self._temperature,
            max_tokens=self._max_tokens,
        )
        return response.choices[0].message.content or ""


class AnthropicBatchProvider:
    """Anthropic Message Batches. submit posts the requests inline (no file upload); collect streams the
    results (matched on custom_id). `complete_one` is the equivalent single Messages call."""

    def __init__(
        self,
        client: Anthropic,
        model: str,
        *,
        name: str = "anthropic",
        temperature: float = 0,
        max_tokens: int = 512,
    ) -> None:
        self._client = client
        self._model = model
        self.name = name
        self._temperature = temperature
        self._max_tokens = max_tokens

    def _params(self, request: BatchRequest) -> dict[str, Any]:
        params: dict[str, Any] = {
            "model": self._model,
            "max_tokens": self._max_tokens,
            "temperature": self._temperature,
            "messages": [{"role": "user", "content": request.prompt}],
        }
        if request.system:
            params["system"] = request.system
        return params

    def submit(self, requests: Sequence[BatchRequest]) -> str:
        # The SDK accepts plain dicts for requests/params at runtime; cast past its stricter TypedDict.
        batch = self._client.messages.batches.create(
            requests=cast("Any", [{"custom_id": r.custom_id, "params": self._params(r)} for r in requests])
        )
        return str(batch.id)

    def poll(self, batch_id: str) -> BatchStatus:
        status = self._client.messages.batches.retrieve(batch_id).processing_status
        return BatchStatus.READY if status == "ended" else BatchStatus.PENDING

    def collect(self, batch_id: str) -> dict[str, str]:
        results: dict[str, str] = {}
        for entry in self._client.messages.batches.results(batch_id):
            if entry.result.type == "succeeded":
                message = entry.result.message
                results[entry.custom_id] = "".join(
                    block.text for block in message.content if block.type == "text"
                ).strip()
        return results

    def complete_one(self, request: BatchRequest) -> str:
        kwargs = self._params(request)
        message = self._client.messages.create(**kwargs)
        return "".join(block.text for block in message.content if block.type == "text").strip()
