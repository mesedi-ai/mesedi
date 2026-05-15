"""
Remote-halt demo — sub-slice 21b.2.

Demonstrates the SSE remote-halt channel end-to-end inside a single
Python process:

  1. `@mesedi.wrap(budget=...)` enters, spawning a background SSE
     reader subscribed to /executions/{id}/halt-stream on the backend.
  2. Inside the wrapped function, we launch a SECOND background
     thread that sleeps for 2 seconds then POSTs to /halt for our own
     execution_id. This stands in for the dashboard / a detector
     triggering the halt remotely.
  3. The reader thread receives the SSE event, calls
     `tracker.signal_remote_halt(reason)`.
  4. The next halt-safe boundary check inside the agent — at a
     `@tool` entry or at `mesedi.checkpoint()` — raises MesediHalt
     with `trigger="remote_signal"`.
  5. `@mesedi.wrap`'s exception handler catches the halt, marks the
     execution `status=halted` with `crash_signature="halt:remote_signal"`,
     and returns None. The agent's `finally` block runs.

Prereqs:
  - Backend running with sub-slice 21b.1 deployed (the SSE endpoints).
    cd ../../backend && go run cmd/api/main.go
  - SDK installed: pip install -e ..

Run:
  python3 halt_remote_test.py

Verify in SQLite afterward:
  cd ../../backend
  sqlite3 mesedi-dev.db \
    "SELECT execution_id, status, duration_ms, crash_signature
     FROM executions WHERE crash_signature='halt:remote_signal'
     ORDER BY started_at DESC LIMIT 5;"
"""

import threading
import time

import httpx

import mesedi
from mesedi import Budget
from mesedi._context import current_execution_context

mesedi.configure(
    api_key="mesedi_sk_dev_local_only",
    base_url="http://localhost:8080",
)

_BASE_URL = "http://localhost:8080"
_API_KEY = "mesedi_sk_dev_local_only"


@mesedi.tool
def slow_tool() -> str:
    """Each call sleeps 300ms. Emits one tool_call event per
    invocation; budget check fires at every @tool entry."""
    time.sleep(0.3)
    return "done"


def _trigger_halt_after(execution_id: str, delay_seconds: float) -> None:
    """Wait, then POST /halt for the given execution.

    Runs in its own thread, separate from the wrapped agent. Stands
    in for the dashboard / a detector publishing the halt — in
    production this would be triggered by an HTTP call from the
    operator-facing surface, not from inside the agent itself.
    """
    time.sleep(delay_seconds)
    try:
        r = httpx.post(
            f"{_BASE_URL}/executions/{execution_id}/halt",
            headers={
                "Authorization": f"Bearer {_API_KEY}",
                "X-Mesedi-Schema-Version": "1",
                "Content-Type": "application/json",
            },
            json={"reason": "remote halt from halt_remote_test.py"},
            timeout=5.0,
        )
        print(f"  [trigger] POST /halt → status={r.status_code} body={r.text}")
    except Exception as exc:  # noqa: BLE001 — demo script, log and continue
        print(f"  [trigger] POST /halt failed: {exc}")


@mesedi.wrap(budget=Budget(max_wall_clock_seconds=60.0))
def runaway_remote_halt_agent() -> str:
    """Loops 100 times at 300ms each — would finish in 30s if left
    alone, well within the 60s wall-clock budget. The trigger thread
    fires a halt at t=2s; the agent halts at the next @tool entry
    (~1-2 iterations later)."""
    ctx = current_execution_context()
    assert ctx is not None, "must be called inside @mesedi.wrap"
    print(f"  [agent] started, execution_id={ctx.execution_id}")

    # Schedule the halt 2 seconds from now. Daemon thread so it can't
    # block the agent's exit.
    t = threading.Thread(
        target=_trigger_halt_after,
        args=(ctx.execution_id, 2.0),
        daemon=True,
        name="halt-test-trigger",
    )
    t.start()

    cleanup_ran = []
    try:
        for i in range(100):
            slow_tool()
            print(f"  [agent] iteration {i + 1} completed")
        return "all 100 iterations done"
    finally:
        cleanup_ran.append(True)
        print(f"  [agent] finally block ran (cleanup_ran={cleanup_ran})")


def _ms(seconds: float) -> str:
    return f"{seconds * 1000:.0f}ms"


if __name__ == "__main__":
    print("\n── Remote-halt demo ──")
    print("  Expected: agent runs ~6-7 iterations, halt arrives via SSE,")
    print("            agent halts at next @tool entry, finally block runs.")
    print("            Result is None (controlled stop, no exception).")
    print()
    t = time.perf_counter()
    result = runaway_remote_halt_agent()
    print(f"\n  wall-clock: {_ms(time.perf_counter() - t)}")
    print(f"  result: {result!r}  (None means halted cleanly)")

    print("\n── Flushing shipper queue... ──")
    t = time.perf_counter()
    ok = mesedi.flush(timeout=5.0)
    print(f"  flush ok={ok} in {_ms(time.perf_counter() - t)}")

    print("\n── Done. Verify in SQLite: ──")
    print("  cd ../../backend")
    print('  sqlite3 mesedi-dev.db "SELECT execution_id, status, '
          'duration_ms, crash_signature FROM executions WHERE '
          "crash_signature='halt:remote_signal' "
          'ORDER BY started_at DESC LIMIT 5;"')
