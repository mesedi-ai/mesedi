"""Wire contract for effect declarations and per-call effects (the
backend's effect_aliasing check), and the MCP call's description and
effects fields."""

from __future__ import annotations

from typing import Any, List
from unittest.mock import patch

import pytest

import mesedi
import importlib

effects_mod = importlib.import_module("mesedi.effects")
mcp_mod = importlib.import_module("mesedi.mcp")
# The package exports the decorator under the same name as its module.
tool_mod = importlib.import_module("mesedi.tool")
from mesedi._context import pop_execution_context, push_execution_context


class _CaptureClient:
    def __init__(self) -> None:
        self.events: List[Any] = []

    def submit_event(self, event: Any) -> None:
        self.events.append(event)


def _run(fn: Any) -> List[Any]:
    client = _CaptureClient()
    token = push_execution_context("exec-effects-1")
    try:
        with patch.object(effects_mod, "get_client", return_value=client), \
             patch.object(mcp_mod, "get_client", return_value=client), \
             patch.object(tool_mod, "get_client", return_value=client):
            fn()
    finally:
        pop_execution_context(token)
    return client.events


def test_declare_effects_wire_contract() -> None:
    events = _run(lambda: mesedi.declare_effects(
        [" github.com/acme/app ", {"destination": "db:prod/customers", "via": ["crm/update", " "]}, "", {"destination": ""}],
        note="release run",
    ))
    assert len(events) == 1
    e = events[0]
    assert e.event_type == "effect_declaration"
    assert e.payload == {
        "destinations": [
            {"destination": "github.com/acme/app"},
            {"destination": "db:prod/customers", "via": ["crm/update"]},
        ],
        "declared_by": "operator",
        "note": "release run",
    }


def test_record_effects_rides_on_the_tool_call() -> None:
    @mesedi.tool
    def push_branch(repo: str) -> str:
        mesedi.record_effects(f"github.com/{repo}")
        mesedi.record_effects("", "  ")
        return "ok"

    events = _run(lambda: push_branch("acme/app"))
    assert events[0].payload["effects"] == ["github.com/acme/app"]


def test_a_failed_tool_call_still_reports_its_effects() -> None:
    @mesedi.tool
    def half_done() -> None:
        mesedi.record_effects("s3://acme/partial")
        raise RuntimeError("boom")

    def call() -> None:
        with pytest.raises(RuntimeError):
            half_done()

    events = _run(call)
    assert events[0].payload["status"] == "failed"
    assert events[0].payload["effects"] == ["s3://acme/partial"]


def test_effects_do_not_leak_between_calls_or_outside_tools() -> None:
    @mesedi.tool
    def writes() -> str:
        mesedi.record_effects("a")
        return "ok"

    @mesedi.tool
    def reads() -> str:
        return "ok"

    def calls() -> None:
        mesedi.record_effects("outside")  # no tool call: dropped
        writes()
        reads()

    events = _run(calls)
    assert events[0].payload["effects"] == ["a"]
    assert "effects" not in events[1].payload


def test_mcp_call_carries_description_and_effects() -> None:
    events = _run(lambda: mesedi.emit_mcp_call(
        server_name="github", method="create_or_update_file", return_value="ok",
        tool_description="Creates or updates a file.", effects=["github.com/acme/app", ""],
    ))
    p = events[0].payload
    assert p["tool_description"] == "Creates or updates a file."
    assert p["effects"] == ["github.com/acme/app"]


def test_mcp_call_without_the_new_fields_is_unchanged() -> None:
    events = _run(lambda: mesedi.emit_mcp_call(server_name="fs", method="read", return_value="x"))
    assert "tool_description" not in events[0].payload
    assert "effects" not in events[0].payload


def test_outside_wrap_declare_effects_is_a_no_op() -> None:
    client = _CaptureClient()
    with patch.object(effects_mod, "get_client", return_value=client):
        mesedi.declare_effects(["x"])
    assert client.events == []


def test_clean_effects() -> None:
    assert effects_mod.clean_effects([" a ", "", "  ", "b"]) == ["a", "b"]
    assert effects_mod.clean_effects(None) == []
