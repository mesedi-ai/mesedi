"""@mesedi.wrap on async functions, the automatic parent link, and crash_type.

Three findings from the 2026-11-15 outside review, fixed 2026-11-16:

- D1: wrap on an ``async def`` recorded a 0 ms completed run before the
  coroutine had done anything, and its events attached to no run.
- D5: a run opened inside another never sent parent_execution_id, though
  the docs said it did and the backend's topology view reads it.
- D2: crashes sent only a hash, so a crash allowlist entry naming the
  exception type ("ValueError") could never match. crash_type now
  carries the class name.
"""

from __future__ import annotations

import asyncio
import sys
from typing import Any, List

import pytest

import mesedi
from mesedi.events import Execution
from mesedi.halt import MesediHalt

wrap_mod = sys.modules["mesedi.wrap"]
observe_mod = sys.modules["mesedi.observe"]


class _Capture:
    def __init__(self) -> None:
        self.starts: List[dict] = []
        self.ends: List[dict] = []
        self.events: List[Any] = []
        self.base_url = ""
        self.api_key = ""

    def submit_execution_start(self, e: Execution) -> None:
        self.starts.append(dict(e.start_payload(), execution_id=e.execution_id))

    def submit_execution_end(self, e: Execution) -> None:
        self.ends.append(dict(e.end_payload(), execution_id=e.execution_id))

    def submit_event(self, event: Any) -> None:
        self.events.append(event)


@pytest.fixture()
def cap(monkeypatch: pytest.MonkeyPatch) -> _Capture:
    c = _Capture()
    monkeypatch.setattr(wrap_mod, "get_client", lambda *a, **k: c)
    monkeypatch.setattr(observe_mod, "get_client", lambda *a, **k: c)
    return c


# ─── D1: async functions ──────────────────────────────────────────────


def test_async_run_is_timed_and_completed_after_the_await(cap: _Capture) -> None:
    @mesedi.wrap
    async def agent() -> str:
        await asyncio.sleep(0.05)
        return "done"

    assert asyncio.run(agent()) == "done"
    assert len(cap.ends) == 1
    assert cap.ends[0]["status"] == "completed"
    assert cap.ends[0]["duration_ms"] >= 45, "the run must cover the awaited work"


def test_async_events_attach_to_the_run(cap: _Capture) -> None:
    @mesedi.wrap
    async def agent() -> None:
        await asyncio.sleep(0)
        mesedi.emit_llm_call(model="m", user_message="hello")

    asyncio.run(agent())
    run_id = cap.starts[0]["execution_id"]
    assert cap.events, "the event inside the async run was dropped"
    assert all(e.execution_id == run_id for e in cap.events)


def test_async_crash_is_recorded_and_reraised(cap: _Capture) -> None:
    @mesedi.wrap
    async def agent() -> None:
        await asyncio.sleep(0)
        raise KeyError("missing")

    with pytest.raises(KeyError):
        asyncio.run(agent())
    assert cap.ends[0]["status"] == "crashed"
    assert cap.ends[0]["crash_type"] == "KeyError"
    assert len(cap.ends[0]["crash_signature"]) == 16


def test_async_halt_returns_none(cap: _Capture) -> None:
    @mesedi.wrap
    async def agent() -> str:
        await asyncio.sleep(0)
        raise MesediHalt(trigger="step_count", reason="test")

    assert asyncio.run(agent()) is None
    assert cap.ends[0]["status"] == "halted"


def test_async_wrapper_is_still_a_coroutine_function() -> None:
    import inspect

    @mesedi.wrap
    async def agent() -> None:
        return None

    assert inspect.iscoroutinefunction(agent)


def test_wrap_with_arguments_supports_async(cap: _Capture) -> None:
    @mesedi.wrap(agent_name="planner")
    async def agent() -> int:
        await asyncio.sleep(0.03)
        return 7

    assert asyncio.run(agent()) == 7
    assert cap.ends[0]["status"] == "completed"
    assert cap.ends[0]["duration_ms"] >= 25, "the run must cover the awaited work"


# ─── D5: parent link ──────────────────────────────────────────────────


def test_nested_run_records_its_parent(cap: _Capture) -> None:
    @mesedi.wrap
    def child() -> None:
        return None

    @mesedi.wrap
    def parent() -> None:
        child()

    parent()
    outer, inner = cap.starts[0], cap.starts[1]
    assert "parent_execution_id" not in outer, "a top-level run has no parent"
    assert inner["parent_execution_id"] == outer["execution_id"]


def test_sibling_after_nested_run_records_the_same_parent(cap: _Capture) -> None:
    @mesedi.wrap
    def child() -> None:
        return None

    @mesedi.wrap
    def parent() -> None:
        child()
        child()

    parent()
    outer = cap.starts[0]["execution_id"]
    assert [s["parent_execution_id"] for s in cap.starts[1:]] == [outer, outer]


def test_async_children_run_concurrently_record_the_same_parent(cap: _Capture) -> None:
    @mesedi.wrap
    async def child(n: int) -> int:
        await asyncio.sleep(0.01)
        return n

    @mesedi.wrap
    async def parent() -> List[int]:
        return list(await asyncio.gather(child(1), child(2), child(3)))

    assert asyncio.run(parent()) == [1, 2, 3]
    outer = cap.starts[0]["execution_id"]
    assert sorted(s.get("parent_execution_id") for s in cap.starts[1:]) == [outer] * 3


def test_run_after_a_finished_run_has_no_parent(cap: _Capture) -> None:
    @mesedi.wrap
    def agent() -> None:
        return None

    agent()
    agent()
    assert all("parent_execution_id" not in s for s in cap.starts)


# ─── D2: crash_type ───────────────────────────────────────────────────


def test_sync_crash_sends_the_exception_type(cap: _Capture) -> None:
    @mesedi.wrap
    def agent() -> None:
        raise ValueError("bad")

    with pytest.raises(ValueError):
        agent()
    assert cap.ends[0]["crash_type"] == "ValueError"


def test_completed_run_sends_no_crash_type(cap: _Capture) -> None:
    @mesedi.wrap
    def agent() -> None:
        return None

    agent()
    assert "crash_type" not in cap.ends[0]


def test_payloads_omit_unset_fields() -> None:
    e = Execution(execution_id="exec-x")
    assert "parent_execution_id" not in e.start_payload()
    assert "crash_type" not in e.end_payload()
    e = Execution(execution_id="exec-y", parent_execution_id="exec-p", crash_type="TypeError")
    assert e.start_payload()["parent_execution_id"] == "exec-p"
    assert e.end_payload()["crash_type"] == "TypeError"
