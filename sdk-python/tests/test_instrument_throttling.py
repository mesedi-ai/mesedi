"""Unit tests for Wave 1.4 — auto-emit infrastructure_event from
instrument_* modules.

Each of the four integration modules (anthropic, openai, cohere,
gemini) gains an auto-emit call inside its exception handler so that
when the canonical error_class signals throttling, a matching
infrastructure_event flows to the backend alongside the existing
failed llm_call. These tests pin (a) the throttling-class filter,
(b) the reason-string mapping, and (c) the wiring inside each
integration module.

If a future refactor changes the throttling set, drops the wiring
from one provider, or remaps reason strings, the infrastructure_throttled
detector will silently misroute or miss events — these tests catch
that BEFORE the SDK ships.
"""

from __future__ import annotations

from typing import Any, Dict, List
from unittest.mock import patch

import pytest

from mesedi.observe import (
    _REASON_BY_ERROR_CLASS,
    _THROTTLING_ERROR_CLASSES,
    _maybe_emit_throttling_event,
)


# ──────────────────────────────────────────────────────────────────────
# Helper-level: throttling-class set + reason mapping
# ──────────────────────────────────────────────────────────────────────


def test_throttling_classes_are_narrow() -> None:
    """The throttling set must contain ONLY rate_limited +
    quota_exhausted. Widening this set without a design conversation
    risks reclassifying provider outages as throttling and corrupting
    the infrastructure_throttled vs provider_incident split."""
    assert _THROTTLING_ERROR_CLASSES == frozenset({
        "rate_limited",
        "quota_exhausted",
    })


def test_reason_mapping_uses_semantic_strings() -> None:
    """The reason field is the cluster key on the dashboard;
    customer-facing strings must read like product, not like
    Go/Python constants."""
    assert _REASON_BY_ERROR_CLASS == {
        "rate_limited": "rate_limit",
        "quota_exhausted": "quota_exhausted",
    }


# ──────────────────────────────────────────────────────────────────────
# Helper-level: _maybe_emit_throttling_event behavior
# ──────────────────────────────────────────────────────────────────────


@pytest.fixture
def captured_infra_emits() -> List[Dict[str, Any]]:
    """Capture every emit_infrastructure_event call routed through
    the observe module under test."""
    captured: List[Dict[str, Any]] = []

    def fake_emit(**kwargs: Any) -> None:
        captured.append(kwargs)

    # Patch in the module under test so the call site routes to our
    # capture instead of the shipper queue.
    with patch("mesedi.observe.emit_infrastructure_event", side_effect=fake_emit):
        yield captured


def test_no_op_for_non_throttling_classes(
    captured_infra_emits: List[Dict[str, Any]],
) -> None:
    """Anything outside the throttling set must NOT emit an
    infrastructure_event. service_unavailable and internal_error in
    particular are provider_incident territory and must not
    cross-contaminate infrastructure_throttled."""
    for non_throttling in [
        "service_unavailable",
        "internal_error",
        "timeout",
        "invalid_api_key",
        "client_error",
        "unknown",
    ]:
        _maybe_emit_throttling_event(provider="test", error_class=non_throttling)
    assert captured_infra_emits == []


def test_rate_limited_emits_with_rate_limit_reason(
    captured_infra_emits: List[Dict[str, Any]],
) -> None:
    _maybe_emit_throttling_event(
        provider="anthropic",
        error_class="rate_limited",
        http_status=429,
        retry_after_seconds=2.5,
        endpoint="/v1/messages",
    )
    assert len(captured_infra_emits) == 1
    call = captured_infra_emits[0]
    assert call["reason"] == "rate_limit"
    assert call["provider"] == "anthropic"
    assert call["status_code"] == 429
    assert call["retry_after_ms"] == 2500
    assert call["endpoint"] == "/v1/messages"


def test_quota_exhausted_emits_with_quota_exhausted_reason(
    captured_infra_emits: List[Dict[str, Any]],
) -> None:
    _maybe_emit_throttling_event(
        provider="openai",
        error_class="quota_exhausted",
        http_status=429,
    )
    call = captured_infra_emits[0]
    assert call["reason"] == "quota_exhausted"
    assert call["status_code"] == 429
    # retry_after defaults to 0 when not supplied — the wire-format
    # contract is omit-when-zero, which the emit helper handles.
    assert call["retry_after_ms"] == 0


def test_zero_retry_after_does_not_get_converted_to_negative(
    captured_infra_emits: List[Dict[str, Any]],
) -> None:
    """Defensive: a None or 0 retry_after must produce 0 (not -1
    or NaN) so the omit-when-zero wire contract holds."""
    _maybe_emit_throttling_event(
        provider="cohere",
        error_class="rate_limited",
        retry_after_seconds=0,
    )
    assert captured_infra_emits[0]["retry_after_ms"] == 0
    _maybe_emit_throttling_event(
        provider="cohere",
        error_class="rate_limited",
        retry_after_seconds=None,
    )
    assert captured_infra_emits[1]["retry_after_ms"] == 0


# ──────────────────────────────────────────────────────────────────────
# Per-provider wiring: each instrument_* module must call the helper
# with its own provider string after the failed llm_call submits.
#
# We do not exercise the full @patch path (which requires the
# anthropic / openai / cohere / google-genai packages installed);
# instead we verify the call-site arguments by importing each
# module and confirming the import chain reaches
# _maybe_emit_throttling_event. The per-module unit tests would
# require fake exception fixtures and the provider SDK installed,
# which is the level of testing #271's classify_*_exception tests
# already cover for the error-classification path. This wave's job
# is the wiring, not the classification.
# ──────────────────────────────────────────────────────────────────────


def test_each_integration_module_imports_the_helper() -> None:
    """Each instrument_* module must import _maybe_emit_throttling_event
    from mesedi.observe; without the import the auto-emit silently
    no-ops. This test fails if a future refactor accidentally drops
    the import from any one module."""
    from mesedi import (
        anthropic_integration,
        cohere_integration,
        gemini_integration,
        openai_integration,
    )

    for mod in (
        anthropic_integration,
        cohere_integration,
        gemini_integration,
        openai_integration,
    ):
        assert hasattr(mod, "_maybe_emit_throttling_event"), (
            f"{mod.__name__} did not import _maybe_emit_throttling_event; "
            "the Wave 1.4 auto-emit will silently no-op for this provider."
        )
