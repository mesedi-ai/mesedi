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


def test_datetime_coerces_to_typed_sentinel() -> None:
    """#270.b: datetime must produce a typed sentinel dict so the
    structural fingerprint can distinguish a datetime field from a
    plain string field. The pre-#270.b code used default=str which
    collapsed both shapes to {key: str} and silently lost drift."""
    import datetime

    dt = datetime.datetime(2026, 6, 21, 12, 0, 0)
    out = _structured_return_value({"when": dt})
    assert isinstance(out, dict)
    assert out["when"] == {"__type__": "datetime", "value": "2026-06-21T12:00:00"}


def test_uuid_coerces_to_typed_sentinel() -> None:
    import uuid

    u = uuid.UUID("12345678-1234-5678-1234-567812345678")
    out = _structured_return_value({"id": u})
    assert isinstance(out, dict)
    assert out["id"] == {
        "__type__": "uuid",
        "value": "12345678-1234-5678-1234-567812345678",
    }


def test_decimal_coerces_to_typed_sentinel() -> None:
    """Decimal preserves precision via str(); the typed sentinel
    keeps it distinct from a plain string in the fingerprint."""
    from decimal import Decimal

    out = _structured_return_value({"price": Decimal("19.99")})
    assert out["price"] == {"__type__": "decimal", "value": "19.99"}


def test_bytes_coerces_to_typed_sentinel_with_size() -> None:
    """Bytes get a 64-char ascii preview plus the original size so
    the dashboard can show SOMETHING without spilling binary."""
    out = _structured_return_value({"blob": b"hello world"})
    assert out["blob"]["__type__"] == "bytes"
    assert out["blob"]["size"] == 11
    assert out["blob"]["value"] == "hello world"


def test_path_coerces_to_typed_sentinel() -> None:
    from pathlib import Path

    out = _structured_return_value({"file": Path("/tmp/x.txt")})
    assert out["file"] == {"__type__": "path", "value": "/tmp/x.txt"}


def test_set_coerces_to_typed_sentinel_sorted() -> None:
    """Sets aren't JSON-encodable; tag as 'set' with a sorted member
    list so the order-insensitive nature is reflected in the shape."""
    out = _structured_return_value({"tags": {"c", "a", "b"}})
    assert out["tags"]["__type__"] == "set"
    assert out["tags"]["value"] == ["a", "b", "c"]


def test_custom_class_coerces_to_typed_object_sentinel() -> None:
    """User classes get a class-name tag so 'returns User' vs
    'returns AdminUser' are distinguishable even when both happen
    to repr similarly."""
    class WidgetModel:
        def __init__(self) -> None:
            self.x = 1

        def __repr__(self) -> str:
            return "WidgetModel(x=1)"

    out = _structured_return_value({"item": WidgetModel()})
    assert out["item"]["__type__"] == "object"
    assert out["item"]["class"] == "WidgetModel"
    assert out["item"]["value"] == "WidgetModel(x=1)"


def test_datetime_string_field_has_distinct_fingerprint_from_real_datetime() -> None:
    """The whole POINT of #270.b: a real datetime and a stringified
    datetime must produce DIFFERENT structural fingerprints so the
    detector catches drift between them. Pre-#270.b both collapsed
    to {key: str}."""
    import datetime
    import json

    dt = datetime.datetime(2026, 6, 21, 12, 0, 0)
    typed = _structured_return_value({"created_at": dt})
    plain = _structured_return_value({"created_at": "2026-06-21T12:00:00"})

    # Direct shape inequality.
    assert typed != plain
    # And their JSON encodings differ — the detector's structural
    # hash will see distinct fingerprints.
    assert json.dumps(typed, sort_keys=True) != json.dumps(
        plain, sort_keys=True
    )


def test_nan_and_inf_coerce_to_typed_sentinel() -> None:
    """NaN / Inf are not JSON-encodable per RFC 8259. The walker
    must convert them rather than letting json.dumps refuse, so
    the caller never gets an unexpected None."""
    out = _structured_return_value({"score": float("nan")})
    assert out["score"]["__type__"] == "object"
    assert out["score"]["class"] == "float"
    assert "nan" in out["score"]["value"].lower()


def test_non_string_dict_keys_are_coerced() -> None:
    """JSON requires string keys. Numeric / boolean / None keys
    must be coerced to their JSON-compatible string form so the
    object encodes without crashing the SDK."""
    out = _structured_return_value({1: "one", True: "yes", None: "null-key"})
    # All three keys land as strings; values pass through.
    assert "1" in out
    assert "yes" in out.values()


def test_circular_reference_returns_none() -> None:
    """Circular structures defeat the walker; the helper returns
    None so the caller can omit the field rather than ship a
    corrupted value."""
    a: dict = {}
    a["self"] = a  # circular ref
    assert _structured_return_value(a) is None


def test_circular_list_reference_returns_none() -> None:
    """Same as the dict case but for self-referential lists."""
    a: list = []
    a.append(a)
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
