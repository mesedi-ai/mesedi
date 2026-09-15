"""Wire-format contract for the two event types added 2026-09-14 when
the egress deferral was reopened: `egress` and
`environment_declaration`. Pins the payload keys the backend
detectors (covert_coordination, environment_misapprehension) will
read, and the destination normalization that keeps URL paths and
query strings, where secrets ride, from ever leaving the process."""

from __future__ import annotations

from typing import Any, List
from unittest.mock import patch

from mesedi import egress
from mesedi._context import pop_execution_context, push_execution_context
from mesedi.egress import _normalize_egress_destination


class _CaptureClient:
    def __init__(self) -> None:
        self.events: List[Any] = []

    def submit_event(self, event: Any) -> None:
        self.events.append(event)


def test_destination_normalization_strips_scheme_path_and_query() -> None:
    cases = {
        "https://api.example.com/v1/users?token=SECRET": "api.example.com",
        "http://10.2.3.4:8443/path#frag": "10.2.3.4:8443",
        "api.example.com": "api.example.com",
        "api.example.com:443": "api.example.com:443",
        "  https://x.io/  ": "x.io",
    }
    for raw, want in cases.items():
        assert _normalize_egress_destination(raw) == want, raw


def test_emit_egress_wire_contract() -> None:
    client = _CaptureClient()
    token = push_execution_context("exec-egress-1")
    try:
        with patch.object(egress, "get_client", return_value=client):
            egress.emit_egress(
                "https://api.example.com/v1?k=SECRET",
                protocol="https",
                tool_name="web_fetch",
                bytes_out=512,
            )
    finally:
        pop_execution_context(token)

    assert len(client.events) == 1
    evt = client.events[0]
    assert evt.event_type == "egress"
    assert evt.execution_id == "exec-egress-1"
    assert evt.payload["destination"] == "api.example.com"
    assert "SECRET" not in str(evt.payload)
    assert evt.payload["protocol"] == "https"
    assert evt.payload["tool_name"] == "web_fetch"
    assert evt.payload["bytes_out"] == 512


def test_emit_environment_declaration_wire_contract() -> None:
    client = _CaptureClient()
    token = push_execution_context("exec-env-1")
    try:
        with patch.object(egress, "get_client", return_value=client):
            egress.emit_environment_declaration("  Simulation ")
    finally:
        pop_execution_context(token)

    assert len(client.events) == 1
    evt = client.events[0]
    assert evt.event_type == "environment_declaration"
    assert evt.payload["mode"] == "simulation"
    assert evt.payload["declared_by"] == "operator"


def test_both_emitters_are_noops_outside_wrap() -> None:
    client = _CaptureClient()
    with patch.object(egress, "get_client", return_value=client):
        egress.emit_egress("api.example.com")
        egress.emit_environment_declaration("live")
    assert client.events == []
