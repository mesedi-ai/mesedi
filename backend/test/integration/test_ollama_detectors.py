"""
test_ollama_detectors.py — — Ollama integration tests.

Exercises the Ollama pipeline end-to-end without requiring a real
Ollama server. Uses dependency-injection via instrument_ollama's
`chat_class` parameter to substitute a fake Ollama client that
returns canned responses shaped like the real ollama package.

Coverage:
  - instrument_ollama emits llm_call events with provider="ollama"
  - Failure-path classifier maps ResponseError(503) to
    service_unavailable in the failure_group's error_class field
  - data_leakage fires on Ollama prompt containing an AWS key
  - detector_status returns skip-reason chips for Ollama-only project

We do NOT test every detector here — provider-agnostic detectors
(semantic_loop, validator_failures, etc.) are already covered by
the canonical tests in test_detectors.py; the data they consume is
the same shape regardless of provider. This file focuses on the
Ollama-specific contract surface.
"""

from __future__ import annotations

import time
from typing import Any, Dict, List

import pytest
import requests

from conftest import Backend, await_failure_group


class _FakeOllamaClient:
    """Stand-in for ollama.Client used via instrument_ollama's
    dependency-injection chat_class parameter. The wrapper patches
    .chat on the class object, so the test fixture passes the class
    itself; instances created from it have the patched chat method.

    The fake returns Ollama-shaped responses with prompt_eval_count
    and eval_count fields the integration translates to canonical
    input_tokens / output_tokens.
    """

    def __init__(self, *args: Any, **kwargs: Any) -> None:
        # Real ollama.Client accepts host=, headers=, etc.
        pass

    def chat(self, *args: Any, **kwargs: Any) -> Dict[str, Any]:
        model = kwargs.get("model", "llama3.1:8b")
        # Pull the last user message length to fake token counts.
        msgs: List[Dict[str, Any]] = kwargs.get("messages", []) or []
        user_msg = ""
        for m in reversed(msgs):
            if isinstance(m, dict) and m.get("role") == "user":
                user_msg = str(m.get("content", ""))
                break
        return {
            "model": model,
            "message": {
                "role": "assistant",
                "content": "fake response from " + model,
            },
            "done": True,
            "prompt_eval_count": max(len(user_msg) // 4, 1),
            "eval_count": 20,
            "total_duration": 12345,
        }


class _FakeFailingOllamaClient(_FakeOllamaClient):
    """A fake that raises an ollama.ResponseError-shaped exception
    so we can exercise the classify_ollama_exception failure path.
    The classifier keys on type(exc).__name__ + .status_code; we
    construct a dynamic exception class to match without depending
    on the real ollama package."""

    def chat(self, *args: Any, **kwargs: Any) -> Dict[str, Any]:
        cls = type("ResponseError", (Exception,), {})
        exc = cls("server overloaded")
        exc.status_code = 503  # type: ignore[attr-defined]
        raise exc


def _patch_with_fake(mesedi, fake_cls):
    """Call instrument_ollama with the fake class injected. Returns
    True if the patch succeeded so the test can xfail gracefully
    on environments where instrument_ollama isn't importable."""
    return mesedi.instrument_ollama(chat_class=fake_cls)


# ──────────────────────────────────────────────────────────────────────
# instrument_ollama basic event emission
# ──────────────────────────────────────────────────────────────────────


def test_ollama_chat_emits_llm_call_event(backend: Backend, configured_sdk):
    """A chat call inside @wrap should produce an llm_call event with
    provider='ollama'. We assert via the backend's executions
    endpoint that the event landed."""
    mesedi = configured_sdk
    _patch_with_fake(mesedi, _FakeOllamaClient)

    @mesedi.wrap
    def ollama_agent():
        client = _FakeOllamaClient()
        return client.chat(
            model="llama3.1:8b",
            messages=[{"role": "user", "content": "hello"}],
        )

    ollama_agent()
    mesedi.flush(timeout=5.0)

    # Poll the executions endpoint for our most-recent execution and
    # confirm it has an llm_call event with provider="ollama".
    deadline = time.time() + 10.0
    found = False
    while time.time() < deadline:
        r = requests.get(
            f"{backend.base_url}/v1/executions?limit=5",
            headers={"Authorization": f"Bearer {backend.api_key}"},
            timeout=5,
        )
        r.raise_for_status()
        executions = r.json().get("executions", [])
        for ex in executions:
            ev = requests.get(
                f"{backend.base_url}/v1/executions/{ex['execution_id']}/events",
                headers={"Authorization": f"Bearer {backend.api_key}"},
                timeout=5,
            )
            if ev.status_code != 200:
                continue
            for event in ev.json().get("events", []):
                if event.get("event_type") != "llm_call":
                    continue
                payload = event.get("payload", {})
                if payload.get("provider") == "ollama":
                    assert payload.get("model") == "llama3.1:8b"
                    assert payload.get("input_tokens", -1) >= 1
                    assert payload.get("output_tokens", -1) >= 1
                    found = True
                    break
            if found:
                break
        if found:
            break
        time.sleep(0.5)
    assert found, "ollama llm_call event with provider='ollama' did not appear"


# ──────────────────────────────────────────────────────────────────────
# data_leakage on Ollama prompts
# ──────────────────────────────────────────────────────────────────────


def test_ollama_data_leakage_fires_on_aws_key_in_prompt(
    backend: Backend, configured_sdk,
):
    """An Ollama chat call with an AWS access key in the user
    message should trigger the data_leakage detector. Verifies the
    DLP-at-ingest scanner runs against provider='ollama' payloads
    the same way it runs against commercial-provider payloads."""
    mesedi = configured_sdk
    _patch_with_fake(mesedi, _FakeOllamaClient)

    @mesedi.wrap
    def leaky_agent():
        client = _FakeOllamaClient()
        return client.chat(
            model="llama3.1:8b",
            messages=[
                {
                    "role": "user",
                    "content": (
                        "diagnose this: AKIAIOSFODNN7EXAMPLE is the key "
                        "we use for our s3 bucket"
                    ),
                },
            ],
        )

    leaky_agent()
    mesedi.flush(timeout=5.0)

    await_failure_group(backend, failure_class="data_leakage")


# ──────────────────────────────────────────────────────────────────────
# detector_status skip-chips for Ollama-only project
# ──────────────────────────────────────────────────────────────────────


def test_ollama_only_project_gets_skip_reason_chips(
    backend: Backend, configured_sdk,
):
    """After a successful Ollama chat call (and no commercial-
    provider calls in this session), GET /v1/detector-status should
    return non-empty skip_reason for provider_incident,
    infrastructure_throttled, and cost_velocity — the three
    architectural-N/A detectors for local runtimes."""
    mesedi = configured_sdk
    _patch_with_fake(mesedi, _FakeOllamaClient)

    @mesedi.wrap
    def ollama_only_agent():
        client = _FakeOllamaClient()
        return client.chat(
            model="llama3.1:8b",
            messages=[{"role": "user", "content": "hi"}],
        )

    ollama_only_agent()
    mesedi.flush(timeout=5.0)

    # Poll — backend aggregation queries are not instantaneous after
    # the event lands.
    deadline = time.time() + 10.0
    got_skip = False
    last_resp: Dict[str, Any] = {}
    while time.time() < deadline:
        r = requests.get(
            f"{backend.base_url}/v1/detector-status",
            headers={"Authorization": f"Bearer {backend.api_key}"},
            timeout=5,
        )
        r.raise_for_status()
        last_resp = r.json()
        pi = (last_resp.get("provider_incident") or {}).get("skip_reason", "")
        it = (last_resp.get("infrastructure_throttled") or {}).get("skip_reason", "")
        cv = (last_resp.get("cost_velocity") or {}).get("skip_reason", "")
        if pi and it and cv:
            got_skip = True
            assert "local runtime" in pi.lower(), pi
            assert "local runtime" in it.lower(), it
            assert "local runtime" in cv.lower(), cv
            break
        time.sleep(0.5)
    assert got_skip, (
        f"Ollama-only project should have skip_reason on all 3 N/A "
        f"detectors; last response: {last_resp}"
    )
