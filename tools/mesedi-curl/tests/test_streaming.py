# Streaming tee-buffer tests.
#
# The contract: every byte must reach the sink immediately, AND the
# returned buffer must contain the full stream (up to MAX_BUFFER_BYTES).
# We simulate a `requests` response by giving stream_response_to_sink a
# tiny stub that yields chunks like the real iter_content does.

import io

from mesedi_curl import streaming


class _StubResponse:
    """Minimal stand-in for a requests.Response object."""

    def __init__(self, chunks):
        self._chunks = chunks
        self.headers = {}

    def iter_content(self, chunk_size):
        for c in self._chunks:
            yield c


def test_stream_response_writes_all_chunks_to_sink():
    chunks = [b"hello ", b"streaming ", b"world"]
    resp = _StubResponse(chunks)
    sink = io.BytesIO()
    buf = streaming.stream_response_to_sink(resp, sink)
    assert sink.getvalue() == b"hello streaming world"
    assert buf == b"hello streaming world"


def test_stream_response_skips_empty_chunks():
    """Real HTTP streams sometimes yield empty bytes between chunks."""
    chunks = [b"a", b"", b"b", b"", b"c"]
    resp = _StubResponse(chunks)
    sink = io.BytesIO()
    buf = streaming.stream_response_to_sink(resp, sink)
    assert sink.getvalue() == b"abc"
    assert buf == b"abc"


def test_stream_response_caps_buffer_but_keeps_streaming():
    """Beyond MAX_BUFFER_BYTES, sink still receives every byte; buffer
    is capped."""
    big_chunk = b"x" * (streaming.MAX_BUFFER_BYTES + 100)
    resp = _StubResponse([big_chunk])
    sink = io.BytesIO()
    buf = streaming.stream_response_to_sink(resp, sink)
    # Sink saw everything.
    assert len(sink.getvalue()) == streaming.MAX_BUFFER_BYTES + 100
    # Buffer was capped.
    assert len(buf) == streaming.MAX_BUFFER_BYTES


def test_detect_streaming_response_from_content_type():
    resp = _StubResponse([])
    resp.headers = {"Content-Type": "text/event-stream"}
    assert streaming.detect_streaming_response(resp) is True


def test_detect_streaming_response_from_chunked_no_length():
    resp = _StubResponse([])
    resp.headers = {"Transfer-Encoding": "chunked"}
    assert streaming.detect_streaming_response(resp) is True


def test_detect_streaming_response_negative_case():
    resp = _StubResponse([])
    resp.headers = {"Content-Type": "application/json", "Content-Length": "120"}
    assert streaming.detect_streaming_response(resp) is False
