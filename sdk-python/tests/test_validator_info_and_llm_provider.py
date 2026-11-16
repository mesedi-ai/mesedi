"""Validator severity "info" (the outside review of 2026-11-15) and emit_llm_call's provider and
error_class (the outside review of 2026-11-15), both from 0.9.0 (2026-11-16)."""

from __future__ import annotations

import logging
import sys
from typing import Any, List

import pytest

import mesedi

wrap_mod = sys.modules["mesedi.wrap"]
observe_mod = sys.modules["mesedi.observe"]


class _Capture:
    def __init__(self) -> None:
        self.events: List[Any] = []
        self.base_url = ""
        self.api_key = ""

    def submit_execution_start(self, _e: Any) -> None:
        return

    def submit_execution_end(self, _e: Any) -> None:
        return

    def submit_event(self, event: Any) -> None:
        self.events.append(event)


@pytest.fixture()
def cap(monkeypatch: pytest.MonkeyPatch) -> _Capture:
    c = _Capture()
    monkeypatch.setattr(wrap_mod, "get_client", lambda *a, **k: c)
    monkeypatch.setattr(observe_mod, "get_client", lambda *a, **k: c)
    monkeypatch.setattr(observe_mod, "_warned_severities", set())
    return c


def _severity(cap: _Capture, value: str) -> str:
    @mesedi.wrap
    def agent() -> None:
        mesedi.validator_result("v", passed=False, severity=value)

    agent()
    return cap.events[-1].payload["severity"]


@pytest.mark.parametrize("value", ["info", "warning", "error", "critical"])
def test_known_severities_pass_through(cap: _Capture, value: str) -> None:
    assert _severity(cap, value) == value


def test_unknown_severity_becomes_warning_not_critical(cap: _Capture, caplog: Any) -> None:
    with caplog.at_level(logging.WARNING, logger="mesedi"):
        assert _severity(cap, "fatal") == "warning"
        assert _severity(cap, "fatal") == "warning"
    warned = [r for r in caplog.records if "is not one of info, warning" in r.getMessage()]
    assert len(warned) == 1, "the warning is printed once per value"


def test_llm_call_carries_provider_and_error_class(cap: _Capture) -> None:
    @mesedi.wrap
    def agent() -> None:
        mesedi.emit_llm_call(model="gpt-4o", user_message="hi", status="failed",
                             provider="openai", error_class="rate_limited")

    agent()
    payload = cap.events[-1].payload
    assert payload["provider"] == "openai"
    assert payload["error_class"] == "rate_limited"
    assert payload["status"] == "failed"


def test_llm_call_without_provider_omits_the_fields(cap: _Capture) -> None:
    @mesedi.wrap
    def agent() -> None:
        mesedi.emit_llm_call(model="gpt-4o", user_message="hi")

    agent()
    payload = cap.events[-1].payload
    assert "provider" not in payload and "error_class" not in payload
