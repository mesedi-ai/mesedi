"""Unit tests for ``mesedi._truncate``.

Run with::

    pip install -e .[dev]
    pytest tests/
"""

from __future__ import annotations

import json

from mesedi._truncate import (
    DEFAULT_MAX_PAYLOAD_BYTES,
    MARKER_ORIGINAL_BYTES,
    MARKER_TRUNCATED,
    MARKER_TRUNCATED_FIELDS,
    MIN_KEEP_CHARS,
    TRUNCATION_SUFFIX,
    maybe_truncate,
)


def _byte_size(payload: dict) -> int:
    return len(json.dumps(payload, separators=(",", ":")).encode("utf-8"))


def test_below_cap_passes_through_unchanged() -> None:
    """Tiny payloads never get the truncation markers stamped on them.
    The customer's data goes out exactly as supplied."""
    payload = {"prompt": "hello", "response": "world"}
    out = maybe_truncate(payload)
    assert out is payload  # same reference; no copy was needed
    assert MARKER_TRUNCATED not in out
    assert MARKER_ORIGINAL_BYTES not in out
    assert MARKER_TRUNCATED_FIELDS not in out


def test_oversized_string_field_gets_truncated() -> None:
    """A payload with one giant string field at the top level should
    have that field shortened until the whole payload fits, and the
    other fields should be left intact."""
    payload = {
        "prompt": "summarize the doc",
        "response": "x" * (DEFAULT_MAX_PAYLOAD_BYTES * 2),
    }
    original_bytes = _byte_size(payload)
    out = maybe_truncate(payload)
    assert out[MARKER_TRUNCATED] is True
    assert out[MARKER_ORIGINAL_BYTES] == original_bytes
    assert "response" in out[MARKER_TRUNCATED_FIELDS]
    assert "prompt" not in out[MARKER_TRUNCATED_FIELDS]
    assert out["prompt"] == "summarize the doc"  # short field untouched
    assert TRUNCATION_SUFFIX in out["response"]
    assert _byte_size(out) <= DEFAULT_MAX_PAYLOAD_BYTES + 200  # markers add ~80 bytes


def test_multiple_oversized_fields_all_get_truncated() -> None:
    """When several fields are individually large enough to overflow the
    budget, the helper shrinks each in turn so the total still fits."""
    payload = {
        "prompt": "a" * (DEFAULT_MAX_PAYLOAD_BYTES // 2),
        "response": "b" * (DEFAULT_MAX_PAYLOAD_BYTES // 2),
        "stack_trace": "c" * (DEFAULT_MAX_PAYLOAD_BYTES // 2),
    }
    out = maybe_truncate(payload)
    assert out[MARKER_TRUNCATED] is True
    truncated = set(out[MARKER_TRUNCATED_FIELDS])
    # At least two of the three should have been shortened
    assert len(truncated) >= 2
    assert _byte_size(out) <= DEFAULT_MAX_PAYLOAD_BYTES + 200


def test_custom_cap_drives_truncation() -> None:
    """Callers can dial the cap up or down. A 1 KB cap on an otherwise
    legal payload forces truncation."""
    payload = {"response": "x" * 5000}  # ~5 KB, well under default
    out = maybe_truncate(payload, max_bytes=1024)
    assert out[MARKER_TRUNCATED] is True
    assert out[MARKER_ORIGINAL_BYTES] > 1024
    assert TRUNCATION_SUFFIX in out["response"]


def test_short_strings_are_preserved() -> None:
    """Strings shorter than MIN_KEEP_CHARS never get truncated even
    when the payload is overall too big. A field that holds an ID or a
    short label stays intact."""
    payload = {
        "execution_id": "exec_abc123",
        "prompt": "x" * (DEFAULT_MAX_PAYLOAD_BYTES * 2),
    }
    out = maybe_truncate(payload)
    assert out["execution_id"] == "exec_abc123"
    assert "execution_id" not in out[MARKER_TRUNCATED_FIELDS]


def test_non_dict_payload_passes_through_unchanged() -> None:
    """Defensive: customers might pass a list or a string as payload
    even though the type hint says Dict. Bail out without raising."""
    list_payload = [1, 2, 3]
    assert maybe_truncate(list_payload) is list_payload  # type: ignore[arg-type]
    string_payload = "hello"
    assert maybe_truncate(string_payload) is string_payload  # type: ignore[arg-type]


def test_truncated_field_keeps_at_least_min_chars() -> None:
    """Even an extremely oversized field gets MIN_KEEP_CHARS of its
    original content preserved so the customer can see at least the
    start of what was sent."""
    payload = {"response": "x" * (DEFAULT_MAX_PAYLOAD_BYTES * 100)}
    out = maybe_truncate(payload)
    assert MIN_KEEP_CHARS <= len(out["response"]) - len(TRUNCATION_SUFFIX)


def test_realistic_llm_event_under_cap() -> None:
    """A typical LLM-call event (prompt + response + model + tokens)
    fits comfortably under the default cap. Sanity check that the 99%
    case doesn't accidentally get marked truncated."""
    payload = {
        "model": "claude-sonnet-4-6",
        "prompt": "Summarize the following document in three sentences." * 5,
        "response": "The document discusses..." * 50,
        "prompt_tokens": 245,
        "completion_tokens": 87,
        "latency_ms": 1240,
    }
    out = maybe_truncate(payload)
    # Untouched (returned by reference)
    assert out is payload
