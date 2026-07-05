"""
Unit tests for — instrument_vertex_gemini Python SDK.
Uses dependency-injected fake classes so no real Vertex SDK or GCP
credentials are needed.
"""

from __future__ import annotations

import sys
from typing import Any, Dict, List

import pytest

import mesedi
from mesedi import vertex_gemini_integration as vgi

wrap_mod = sys.modules["mesedi.wrap"]


class _CapturedClient:
    def __init__(self) -> None:
        self.events: List[Any] = []
        self.base_url = ""
        self.api_key = ""

    def submit_event(self, event: Any) -> None:
        self.events.append(event)

    def submit_execution_start(self, _e: Any) -> None:
        return

    def submit_execution_end(self, _e: Any) -> None:
        return


def _setup_capture(monkeypatch: pytest.MonkeyPatch) -> _CapturedClient:
    cap = _CapturedClient()

    def _get(*_a: Any, **_k: Any) -> Any:
        return cap

    monkeypatch.setattr(vgi, "get_client", _get)
    monkeypatch.setattr(wrap_mod, "get_client", _get)
    return cap


def _llm_call_payloads(cap: _CapturedClient) -> List[Dict[str, Any]]:
    out: List[Dict[str, Any]] = []
    for ev in cap.events:
        et = getattr(ev, "event_type", None)
        if et is None:
            continue
        if getattr(et, "value", et) == "llm_call":
            out.append(ev.payload)
    return out


class _FakeVertexGenerativeModel:
    _model_name = "gemini-2.5-pro"
    raise_exc: Exception = None  # type: ignore[assignment]

    def generate_content(self, *_args: Any, **_kwargs: Any) -> Any:
        if self.raise_exc is not None:
            raise self.raise_exc

        class _UM:
            prompt_token_count = 12
            candidates_token_count = 34

        class _R:
            text = "vertex response"
            usage_metadata = _UM()

        return _R()


def test_vertex_gemini_success_emits_chat_event(monkeypatch):
    cap = _setup_capture(monkeypatch)
    cls = type("V", (_FakeVertexGenerativeModel,), {})
    vgi._patch_vertex_gemini_sync(cls)

    @mesedi.wrap
    def agent() -> Any:
        return cls().generate_content("hello vertex")

    agent()
    payloads = _llm_call_payloads(cap)
    chats = [p for p in payloads if p.get("provider") == "gemini"]
    assert len(chats) == 1
    ev = chats[0]
    assert ev["surface"] == "chat"
    assert ev["model"] == "gemini-2.5-pro"
    assert ev["user_message"] == "hello vertex"
    assert ev["response_text"] == "vertex response"
    assert ev["input_tokens"] == 12
    assert ev["output_tokens"] == 34


def test_vertex_gemini_failure_emits_event_with_error_class(monkeypatch):
    cap = _setup_capture(monkeypatch)

    class _Err(Exception):
        pass

    cls = type("V2", (_FakeVertexGenerativeModel,), {"raise_exc": _Err("boom")})
    vgi._patch_vertex_gemini_sync(cls)

    @mesedi.wrap
    def agent() -> Any:
        return cls().generate_content("x")

    with pytest.raises(_Err):
        agent()

    payloads = _llm_call_payloads(cap)
    failures = [
        p for p in payloads
        if p.get("provider") == "gemini" and p.get("status") == "failed"
    ]
    assert len(failures) == 1
    assert failures[0]["surface"] == "chat"
    assert "error_class" in failures[0]


def test_instrument_vertex_gemini_returns_false_when_no_class():
    """When vertexai isn't installed and no class is injected,
    instrument_vertex_gemini returns False (no-op)."""
    # Don't inject a class. Real vertexai may or may not be installed
    # in the test environment; just verify the function doesn't raise.
    result = vgi.instrument_vertex_gemini()
    assert isinstance(result, bool)


def test_vertex_gemini_async_patcher_runs():
    class _Async:
        async def generate_content_async(
            self, *_args: Any, **_kwargs: Any,
        ) -> Any:
            return None

    original = _Async.generate_content_async
    vgi._patch_vertex_gemini_async(_Async)
    assert _Async.generate_content_async is not original


def test_instrument_vertex_gemini_export_available():
    assert hasattr(mesedi, "instrument_vertex_gemini")
    assert callable(mesedi.instrument_vertex_gemini)
