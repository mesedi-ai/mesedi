"""The wrap-level execution_mode sugar: `@mesedi.wrap(
execution_mode="simulation")` must state the run's intended
environment as its first event, and omitting it must declare
nothing. Twin of the wrap sugar cases in
sdk-typescript/src/egress_environment.test.ts."""

from __future__ import annotations

from typing import Any, List
from unittest.mock import patch

import importlib

# The package's `from mesedi.wrap import wrap` shadows the submodule
# attribute with the function, so plain `import mesedi.wrap as m`
# binds the function too. importlib returns the real module.
egress_module = importlib.import_module("mesedi.egress")
wrap_module = importlib.import_module("mesedi.wrap")


class _CaptureClient:
    base_url = "http://fake.invalid"
    api_key = "mesedi_sk_test"

    def __init__(self) -> None:
        self.events: List[Any] = []
        self.starts: List[Any] = []
        self.ends: List[Any] = []

    def submit_event(self, event: Any) -> None:
        self.events.append(event)

    def submit_execution_start(self, execution: Any) -> None:
        self.starts.append(execution)

    def submit_execution_end(self, execution: Any) -> None:
        self.ends.append(execution)


def test_wrap_execution_mode_declares_first() -> None:
    client = _CaptureClient()
    with (
        patch.object(wrap_module, "get_client", return_value=client),
        patch.object(egress_module, "get_client", return_value=client),
    ):
        @wrap_module.wrap(execution_mode="simulation")
        def agent() -> str:
            return "done"

        assert agent() == "done"

    decls = [e for e in client.events if e.event_type == "environment_declaration"]
    assert len(decls) == 1
    assert decls[0].payload["mode"] == "simulation"
    assert decls[0].payload["declared_by"] == "wrap"
    assert decls[0].sequence == 1
    assert len(client.starts) == 1 and len(client.ends) == 1


def test_wrap_without_mode_declares_nothing() -> None:
    client = _CaptureClient()
    with (
        patch.object(wrap_module, "get_client", return_value=client),
        patch.object(egress_module, "get_client", return_value=client),
    ):
        @wrap_module.wrap
        def agent() -> str:
            return "quiet"

        assert agent() == "quiet"

    assert [e for e in client.events if e.event_type == "environment_declaration"] == []
