"""
test_detectors.py — one test per customer-facing failure class.

Each test fires the SDK calls a real customer would emit to trigger
that detector, then asserts the matching failure_group appears in
the backend's public /failure-groups response within a timeout.

If a test fails, it means the SDK + backend pair has drifted from
detecting that class end-to-end — which is a customer-visible bug.
Fix the field-name mismatch / ordering issue / threshold logic that
the test reveals.

Test names match the customer-facing failure_class string the
backend stores. New detectors should land alongside a test here.
"""

from __future__ import annotations

import os

import pytest

from conftest import Backend, await_failure_group


# Tests that need a real Anthropic call get this mark. Suite-runners
# without ANTHROPIC_API_KEY in their env will skip them rather than
# fail loudly — the Go-side detector tests still run.
needs_anthropic = pytest.mark.skipif(
    not os.environ.get("ANTHROPIC_API_KEY"),
    reason="ANTHROPIC_API_KEY not set; skipping real-LLM scenario",
)


# ──────────────────────────────────────────────────────────────────────
# crashes
# ──────────────────────────────────────────────────────────────────────

def test_crashes(backend: Backend, configured_sdk):
    """Unhandled exception inside @wrap → backend records status=
    crashed and fires the crashes detector."""
    mesedi = configured_sdk

    @mesedi.wrap
    def crashing_agent():
        raise RuntimeError("inttest crash")

    with pytest.raises(RuntimeError):
        crashing_agent()
    mesedi.flush(timeout=5.0)

    await_failure_group(backend, failure_class="crashes")
