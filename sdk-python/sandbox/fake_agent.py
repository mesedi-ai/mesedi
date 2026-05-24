"""
Throwaway test agent that exercises the Mesedi backend ingest surface
using the local dev API key.

NOT the real SDK, that's Phase 2 (with @wrap decorator, Anthropic
monkey-patching, async event buffer, etc.). This file is exploration:
shape-match the wire format the backend already accepts, watch what a
realistic agent execution looks like end-to-end, see what an SDK actually
needs to do.

Prereqs:
  - Backend running: cd ../../backend && go run cmd/api/main.go
  - httpx installed: pip install httpx --break-system-packages
    (or use a venv if you prefer)

Run:
  python fake_agent.py

Verify the DB landed events:
  cd ../../backend
  sqlite3 mesedi-dev.db \\
    "SELECT execution_id, event_type, sequence FROM events ORDER BY sequence DESC LIMIT 10;"
"""

import time
import uuid
from datetime import datetime, timezone

import httpx

BASE_URL = "http://localhost:8080"
DEV_KEY = "mesedi_sk_dev_local_only"


def now_rfc3339() -> str:
    """RFC 3339 UTC timestamp with microsecond precision."""
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")


def main() -> None:
    client = httpx.Client(
        base_url=BASE_URL,
        headers={
            "Authorization": f"Bearer {DEV_KEY}",
            "Content-Type": "application/json",
            # Wire-format version. Backend rejects unsupported versions.
            # Bump on the SDK side any time we adopt a new schema.
            "X-Mesedi-Schema-Version": "1",
        },
        timeout=10.0,
    )

    execution_id = f"exec-{uuid.uuid4().hex[:12]}"
    print(f"\n=== Fake agent run: {execution_id} ===\n")

    # 1. Start the execution.
    r = client.post(
        "/executions",
        json={
            "execution_id": execution_id,
            "status": "started",
            "sdk_language": "python",
            "sdk_version": "0.0.0-sandbox",
        },
    )
    r.raise_for_status()
    print(f"[start]  POST /executions      → {r.status_code} {r.json()}")

    # 2. Simulate a realistic agent loop:
    #    5 LLM calls, each followed by a tool call. The kind of sequence
    #    that should produce 11 rows in SQLite (1 execution + 10 events).
    sequence = 0
    for step in range(1, 6):
        # LLM call
        sequence += 1
        r = client.post(
            "/events",
            json=[
                {
                    "event_id": f"evt-{uuid.uuid4().hex[:12]}",
                    "execution_id": execution_id,
                    "event_type": "llm_call",
                    "sequence": sequence,
                    "timestamp": now_rfc3339(),
                    "duration_ms": 850 + step * 50,
                    "payload": {
                        "model": "claude-opus-4-6",
                        "prompt_tokens": 120 + step * 10,
                        "completion_tokens": 200 + step * 20,
                        "system_prompt": "You are a helpful research assistant.",
                        "user_message": f"Step {step}: search for X.",
                        "response_text": f"Step {step}: found Y, summarizing.",
                        "latency_ms": 850 + step * 50,
                    },
                }
            ],
        )
        r.raise_for_status()
        print(f"[step {step}] POST /events llm_call  → {r.status_code} {r.json()}")

        # Tool call
        sequence += 1
        r = client.post(
            "/events",
            json=[
                {
                    "event_id": f"evt-{uuid.uuid4().hex[:12]}",
                    "execution_id": execution_id,
                    "event_type": "tool_call",
                    "sequence": sequence,
                    "timestamp": now_rfc3339(),
                    "duration_ms": 120 + step * 10,
                    "payload": {
                        "tool_name": "search_web",
                        "arguments_json": f'{{"query":"step {step}"}}',
                        "result_summary": "200 OK, 3 results",
                        "latency_ms": 120 + step * 10,
                    },
                }
            ],
        )
        r.raise_for_status()
        print(f"[step {step}] POST /events tool_call → {r.status_code} {r.json()}")

        # Pretend the agent did some work between steps.
        time.sleep(0.05)

    # 3. Mark the execution completed.
    r = client.patch(
        f"/executions/{execution_id}",
        json={
            "status": "completed",
            "duration_ms": 4500,
            "total_tokens_in": 750,
            "total_tokens_out": 1300,
            "estimated_cost_usd": 0.0145,
        },
    )
    r.raise_for_status()
    print(f"\n[done]   PATCH /executions/{{id}} → {r.status_code} {r.json()}")

    print(f"\n=== Done. Sent: 1 execution + 10 events ===")
    print(f"\nVerify in SQLite:")
    print(f"  cd ../../backend")
    print(
        f"  sqlite3 mesedi-dev.db \"SELECT event_type, sequence FROM events "
        f"WHERE execution_id='{execution_id}' ORDER BY sequence;\""
    )
    print(f"  # Expect 10 rows: alternating llm_call, tool_call")


if __name__ == "__main__":
    main()
