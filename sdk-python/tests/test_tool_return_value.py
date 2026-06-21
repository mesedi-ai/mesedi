"""
Tests for the structured ``return_value`` field added to @tool's
tool_call payload (#270).

The backend's tool_schema_drift detector fingerprints the return
shape from this field. These tests pin the contract so a future
edit to @tool that drops or mis-types the field fails CI before
reaching customers.
"""

from __future__ import annotations

import json

import pytest

from mesedi.tool import _structured_return_value, _MAX_RETURN_VALUE_JSON


# ──────────────────────────────────────────────────────────────────────
# JSON-native results pass through untouched
# ──────────────────────────────────────────────────────────────────────


@pytest.mark.parametrize(
    "value",
    [
        # Primitives.
        None,
        True,
        False,
        0,
        42,
        -3.14,
        "",
        "hello",
        # Nested structures the detector cares about most.
        [],
        [1, 2, 3],
        {},
        {"id": "a", "name": "widget", "price": 1.99},
        {"a": [1, 2, {"b": "c"}]},
    ],
)
def test_json_native_values_pass_through_unchanged(value):
    out = _structured_return_value(value)
    assert out == value


# ──────────────────────────────────────────────────────────────────────
# Non-JSON-native values get coerced via default=str
# ──────────────────────────────────────────────────────────────────────


def test_datetime_coerces_to_string() -> None:
    import datetime

    dt = datetime.datetime(2026, 6, 21, 12, 0, 0)
    out = _structured_return_value({"when": dt})
    # default=str on the datetime makes it the iso-ish str(dt)
    # value; the SHAPE is preserved (object with one string key
    # mapping to a string value), which is what the detector
    # actually fingerprints.
    assert isinstance(out, dict)
    assert "when" in out
    assert isinstance(out["when"], str)


def test_uuid_coerces_to_string() -> None:
    import uuid

    u = uuid.uuid4()
    out = _structured_return_value({"id": u})
    assert isinstance(out, dict)
    assert isinstance(out["id"], str)


def test_unserializable_returns_none() -> None:
    """A self-referential structure defeats json.dumps even with
    default=str, so the helper returns None and the caller omits
    the field rather than shipping a corrupted value."""
    a = {}
    a["self"] = a  # circular ref
    assert _structured_return_value(a) is None


# ──────────────────────────────────────────────────────────────────────
# Truncation
# ──────────────────────────────────────────────────────────────────────


def test_oversized_payload_returns_truncated_sentinel() -> None:
    huge = {"data": "x" * (_MAX_RETURN_VALUE_JSON + 100)}
    out = _structured_return_value(huge)
    assert out == "<truncated>"


def test_just_under_cap_passes_through() -> None:
    # Build a payload whose JSON serialization is comfortably
    # under the cap; assert it survives the round-trip intact.
    payload = {"data": "x" * (_MAX_RETURN_VALUE_JSON // 2)}
    out = _structured_return_value(payload)
    assert out == payload


# ──────────────────────────────────────────────────────────────────────
# Round-trip cleanliness: returned value must be JSON-native
# ──────────────────────────────────────────────────────────────────────


def test_returned_value_is_json_serializable_without_default() -> None:
    """The SDK shipper later calls json.dumps with no default=.
    The helper must guarantee the value it returns is JSON-native
    so that shipper call cannot crash on a residual datetime / UUID
    / etc.
    """
    import datetime

    out = _structured_return_value(
        {"when": datetime.datetime(2026, 6, 21), "id": "abc"}
    )
    # Plain json.dumps must succeed (no default= argument).
    assert json.dumps(out) is not None
