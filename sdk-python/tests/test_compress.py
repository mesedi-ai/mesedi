"""Unit tests for ``mesedi._compress``.

Run with::

    pip install -e .[dev]
    pytest tests/
"""

from __future__ import annotations

import zstandard

from mesedi._compress import (
    DEFAULT_THRESHOLD_BYTES,
    SUPPORTED_ENCODING,
    maybe_compress,
)


def test_below_threshold_passes_through_unchanged() -> None:
    """Tiny payloads must skip compression: zstd framing overhead would
    otherwise inflate them."""
    body = b'{"hello":"world"}'
    out, headers = maybe_compress(body)
    assert out == body
    assert headers == {}


def test_at_threshold_compresses() -> None:
    """A payload at or above the threshold engages compression and
    carries the Content-Encoding header the backend recognizes."""
    body = b'{"prompt":"' + b"a" * (DEFAULT_THRESHOLD_BYTES + 10) + b'"}'
    out, headers = maybe_compress(body)
    assert out != body
    assert len(out) < len(body)
    assert headers == {"Content-Encoding": SUPPORTED_ENCODING}


def test_compressed_payload_round_trips() -> None:
    """Decompressing the SDK output with a fresh decoder yields the
    original bytes. Guards against accidentally shipping a different
    encoding."""
    original = b'{"event_type":"llm_call","payload":"' + b"x" * 2000 + b'"}'
    compressed, headers = maybe_compress(original)
    assert headers == {"Content-Encoding": SUPPORTED_ENCODING}
    decoder = zstandard.ZstdDecompressor()
    assert decoder.decompress(compressed) == original


def test_custom_threshold_forces_compression_on_small_payload() -> None:
    """Callers can override the threshold (useful for tests). Setting
    threshold below the body size engages compression on otherwise
    too-small payloads."""
    body = b'{"x":"abc"}'
    out, headers = maybe_compress(body, threshold_bytes=5)
    assert out != body
    assert headers == {"Content-Encoding": SUPPORTED_ENCODING}


def test_high_compressibility_yields_meaningful_ratio() -> None:
    """Sanity check: highly repetitive JSON (the shape of a batched
    events array) compresses to well under half its original size.
    Not a strict guarantee, just confirms the encoder is doing real
    work on real-shaped input."""
    # Build a 100-event batch with repeating structure, the shape the
    # real shipper sends in production at default batch size.
    parts = []
    for i in range(100):
        parts.append(
            '{"event_id":"evt_' + "x" * 24 + '",'
            '"execution_id":"exec_abc123","event_type":"llm_call",'
            '"payload":{"prompt":"summarize","response":"ok"}}'
        )
    original = ("[" + ",".join(parts) + "]").encode("utf-8")
    assert len(original) > DEFAULT_THRESHOLD_BYTES
    compressed, headers = maybe_compress(original)
    assert headers == {"Content-Encoding": SUPPORTED_ENCODING}
    assert len(compressed) < len(original) // 2
