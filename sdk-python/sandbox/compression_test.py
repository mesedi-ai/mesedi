"""Sandbox round-trip test for SDK zstd compression.

Sends a real compressed events batch to a configured Mesedi backend
and verifies the backend accepted it (HTTP 200 from the ingest
endpoint). The backend must speak compression (v0.3.0 era or later;
the decompression middleware shipped in the api: accept zstd request
bodies commit).

Run against a local backend::

    MESEDI_API_KEY=mesedi_sk_test_... \\
    MESEDI_BASE_URL=http://localhost:8080 \\
    python sandbox/compression_test.py

Or against production with a test-mode key (do this only with a
project you own; the synthetic-customer project is the safe choice)::

    MESEDI_API_KEY=mesedi_sk_test_... \\
    python sandbox/compression_test.py
"""

from __future__ import annotations

import json
import logging
import os
import sys
import uuid

import httpx

from mesedi._compress import SUPPORTED_ENCODING, maybe_compress

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("mesedi.sandbox.compression")


def build_batch(execution_id: str, count: int = 100) -> list:
    """Build a count-event batch with repeating shape. Large enough
    after JSON-serialization to comfortably exceed the 1 KB compression
    threshold."""
    return [
        {
            "event_id": f"evt_{uuid.uuid4().hex[:16]}",
            "execution_id": execution_id,
            "event_type": "llm_call",
            "sequence": i,
            "timestamp": "2026-06-19T00:00:00Z",
            "payload": {
                "prompt": "summarize the following document",
                "response": "ok " * 20,
                "model": "claude-sonnet-4-6",
            },
        }
        for i in range(count)
    ]


def main() -> int:
    api_key = os.environ.get("MESEDI_API_KEY")
    if not api_key:
        print("MESEDI_API_KEY not set", file=sys.stderr)
        return 1
    base_url = os.environ.get("MESEDI_BASE_URL", "https://api.mesedi.ai").rstrip("/")

    execution_id = f"exec_sandbox_{uuid.uuid4().hex[:12]}"
    batch = build_batch(execution_id, count=100)
    serialized = json.dumps(batch, separators=(",", ":")).encode("utf-8")
    compressed, extra = maybe_compress(serialized)

    if extra.get("Content-Encoding") != SUPPORTED_ENCODING:
        print(
            f"sandbox: payload below compression threshold "
            f"({len(serialized)} bytes); test inconclusive",
            file=sys.stderr,
        )
        return 2

    ratio = len(compressed) / len(serialized)
    logger.info(
        "sandbox: serialized=%d bytes, compressed=%d bytes, ratio=%.2f",
        len(serialized),
        len(compressed),
        ratio,
    )

    # Step 1: create the execution so the events endpoint has an
    # execution to attach to. Uncompressed so the test isolates the
    # /events compression path.
    with httpx.Client(base_url=base_url, timeout=10.0) as client:
        client.headers["Authorization"] = f"Bearer {api_key}"
        client.headers["X-Mesedi-Schema-Version"] = "1"

        exec_resp = client.post(
            "/executions",
            json={
                "execution_id": execution_id,
                "agent_name": "compression_sandbox",
                "started_at": "2026-06-19T00:00:00Z",
            },
        )
        if exec_resp.status_code >= 400:
            print(
                f"sandbox: POST /executions failed: {exec_resp.status_code} "
                f"{exec_resp.text[:200]}",
                file=sys.stderr,
            )
            return 3

        # Step 2: send the compressed batch. The headers dict here is
        # what the SDK shipper would send in production.
        events_resp = client.post(
            "/events",
            content=compressed,
            headers={
                "Content-Type": "application/json",
                "Content-Encoding": SUPPORTED_ENCODING,
            },
        )

    if events_resp.status_code >= 400:
        print(
            f"sandbox: POST /events failed: {events_resp.status_code} "
            f"{events_resp.text[:500]}",
            file=sys.stderr,
        )
        return 4

    print(
        f"sandbox: PASS — backend accepted {len(batch)} events "
        f"(compressed {len(serialized)}→{len(compressed)} bytes, "
        f"ratio {ratio:.2f}, response {events_resp.status_code})"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
