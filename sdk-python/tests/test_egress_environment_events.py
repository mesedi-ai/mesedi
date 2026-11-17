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


def test_the_no_passwords_setting_reaches_recorded_addresses() -> None:
    """Round 3, M2: "host:8080/a@b" is unreadable by default and keeps
    its host and port only when the client says addresses never carry
    passwords."""
    for setting, want in ((False, "unreadable"), (True, "host.example.com:8080")):
        client = _CaptureClient()
        client.addresses_never_contain_passwords = setting  # type: ignore[attr-defined]
        token = push_execution_context("exec-egress-m2")
        try:
            with patch.object(egress, "get_client", return_value=client):
                egress.emit_egress("https://host.example.com:8080/a@b")
        finally:
            pop_execution_context(token)
        assert client.events[0].payload["destination"] == want, setting


def test_configure_reads_the_no_passwords_setting(monkeypatch: Any) -> None:
    import mesedi.client as client_mod

    monkeypatch.setenv("MESEDI_ADDRESSES_NEVER_CONTAIN_PASSWORDS", "1")
    c = client_mod.configure(api_key="mesedi_sk_test_m2", base_url="http://127.0.0.1:9")
    try:
        assert c.addresses_never_contain_passwords is True
        c2 = client_mod.configure(api_key="mesedi_sk_test_m2", base_url="http://127.0.0.1:9",
                                  addresses_never_contain_passwords=False)
        assert c2.addresses_never_contain_passwords is False
    finally:
        client_mod._default_client.close()
        client_mod._default_client = None


def test_addresses_never_contain_passwords_reads_the_client() -> None:
    class _C:
        addresses_never_contain_passwords = True

    with patch.object(egress, "get_client", return_value=_C()):
        assert egress._addresses_never_contain_passwords() is True

    def _unconfigured() -> Any:
        raise RuntimeError("not configured")

    with patch.object(egress, "get_client", side_effect=_unconfigured):
        assert egress._addresses_never_contain_passwords() is False
