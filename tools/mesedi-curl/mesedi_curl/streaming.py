# Streaming response handling.
#
# The hard requirement: every byte the upstream provider streams must
# reach stdout (or the user-specified output file) with the same
# latency it would have under plain curl. We are NOT allowed to delay
# any output for token math. Mesedi's recording happens AFTER the
# stream completes; the wrapper trades a tiny amount of memory for
# this guarantee.
#
# Implementation: iterate the response with stream=True, write each
# chunk to the output sink immediately, AND append it to an in-memory
# buffer that we hand back to the caller when the stream closes. The
# buffer's bounded by the response's natural size. If a malicious
# server tried to stream gigabytes, the user would see it streaming
# in real time and could kill the process; the buffer would grow but
# would never delay output. We do cap the buffer at 10 MB as a
# defensive measure (anything larger and the buffer just stops
# growing, but we keep streaming to stdout).
from __future__ import annotations

import io
import sys
from typing import BinaryIO, Iterable, Optional

import requests


# Cap on the in-memory response buffer. Real LLM responses are tens of
# KB at most; this exists to protect against pathologically large
# streamed responses. Beyond the cap, stdout still gets every byte;
# we just stop appending to the buffer.
MAX_BUFFER_BYTES = 10 * 1024 * 1024


def stream_response_to_sink(
    response: requests.Response,
    sink: BinaryIO,
    chunk_size: int = 4096,
) -> bytes:
    """Pipe the response body to `sink` while buffering for analysis.

    Returns the buffered bytes (truncated at MAX_BUFFER_BYTES). The
    sink is flushed after every chunk so streaming clients (Anthropic
    streaming, OpenAI delta SSE) see deltas with no added latency.

    The caller is responsible for closing `response`.
    """
    buf = io.BytesIO()
    buf_size = 0
    capped = False

    for chunk in response.iter_content(chunk_size=chunk_size):
        if not chunk:
            continue
        # Always write to the user-visible sink first. If this raises
        # (broken pipe, file closed), propagate it; the wrapper's
        # outer try/except handles cleanup.
        sink.write(chunk)
        sink.flush()

        if not capped:
            remaining = MAX_BUFFER_BYTES - buf_size
            if remaining <= 0:
                capped = True
            else:
                take = chunk if len(chunk) <= remaining else chunk[:remaining]
                buf.write(take)
                buf_size += len(take)
                if buf_size >= MAX_BUFFER_BYTES:
                    capped = True

    return buf.getvalue()


def detect_streaming_response(response: requests.Response) -> bool:
    """Best-effort detection of whether the response is an SSE/chunked stream.

    Used when the request body did not include `stream: true` but the
    server decided to chunk anyway (some proxies do this).
    """
    ctype = (response.headers.get("Content-Type") or "").lower()
    if "text/event-stream" in ctype:
        return True
    if response.headers.get("Transfer-Encoding", "").lower() == "chunked":
        # Chunked alone is not proof of streaming intent (lots of
        # ordinary JSON responses come back chunked), but combined
        # with no Content-Length it's a strong signal.
        if "Content-Length" not in response.headers:
            return True
    return False
