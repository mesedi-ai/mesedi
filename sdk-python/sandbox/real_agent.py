"""
Example using the real Mesedi SDK (v0.0.2 — async event shipper).

This is the same demo as before, but the SDK now ships executions and
events asynchronously via a background daemon thread. The wrapped
function should return in essentially "function-time only" — no HTTP
round-trip latency from the @wrap decorator itself.

Prereqs:
  - Backend running:
      cd ../../backend && go run cmd/api/main.go
  - SDK installed locally in editable mode:
      cd ../  &&  python3 -m pip install -e .

Run:
  python3 real_agent.py

Verify in SQLite:
  cd ../../backend
  sqlite3 mesedi-dev.db \\
    "SELECT execution_id, status, duration_ms, crash_signature
     FROM executions
     ORDER BY started_at DESC LIMIT 5;"
"""

import time

import mesedi

mesedi.configure(
    api_key="mesedi_sk_dev_local_only",
    base_url="http://localhost:8080",
)


@mesedi.wrap
def successful_agent(query: str) -> str:
    """Pretend to be a real agent. Sleeps 50ms then returns an answer."""
    time.sleep(0.05)
    return f"Answer to {query!r}: 42"


@mesedi.wrap
def crashing_agent(query: str) -> str:
    """Pretend to be a buggy agent. Sleeps 20ms then crashes."""
    time.sleep(0.02)
    raise ValueError(f"Could not parse {query!r}")


@mesedi.wrap
def instant_agent() -> str:
    """Returns immediately. Shows the SDK overhead in isolation."""
    return "done"


def _ms(seconds: float) -> str:
    return f"{seconds * 1000:.1f}ms"


if __name__ == "__main__":
    # ── Latency demo ─────────────────────────────────────────────────
    # With the async shipper, wall-clock time for the wrapped call
    # should be very close to "function body only" — the @wrap overhead
    # is just dataclass construction + queue enqueue, both
    # microseconds-scale.

    print("\n── Run 1: instant agent (showing @wrap overhead) ──")
    t = time.perf_counter()
    _ = instant_agent()
    print(f"  wall-clock: {_ms(time.perf_counter() - t)} (target: < 5ms)")

    print("\n── Run 2: successful agent (50ms sleep) ──")
    t = time.perf_counter()
    result = successful_agent("what is the meaning of life?")
    print(f"  wall-clock: {_ms(time.perf_counter() - t)} (target: ~50ms)")
    print(f"  result: {result}")

    print("\n── Run 3: crashing agent (20ms sleep, then ValueError) ──")
    t = time.perf_counter()
    try:
        crashing_agent("bad input")
    except ValueError as e:
        print(f"  wall-clock: {_ms(time.perf_counter() - t)} (target: ~20ms)")
        print(f"  caught (re-raised by @wrap, as expected): {e}")

    # ── Sync barrier ─────────────────────────────────────────────────
    # The shipper is async, so executions may not have landed at the
    # backend yet when these lines run. flush() blocks until the
    # background thread has drained the queue.

    print("\n── Flushing shipper queue... ──")
    t = time.perf_counter()
    ok = mesedi.flush(timeout=5.0)
    print(f"  flush ok={ok} in {_ms(time.perf_counter() - t)}")

    print("\n── Done. Three executions recorded.")
    print("Verify in SQLite:")
    print("  cd ../../backend")
    print(
        '  sqlite3 mesedi-dev.db "SELECT execution_id, status, '
        'duration_ms, crash_signature FROM executions ORDER BY '
        'started_at DESC LIMIT 5;"'
    )
