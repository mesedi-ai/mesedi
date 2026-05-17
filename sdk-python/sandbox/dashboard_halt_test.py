"""
Dashboard-halt test — sub-slice 21c.

Companion to halt_remote_test.py, but with NO auto-trigger thread.
Lets you exercise the dashboard's operator Halt button end-to-end:

  1. This script starts a wrapped agent with a 60s wall-clock budget
     that calls slow_tool() 100 times at 300ms each (~30s normal
     runtime, well within budget).
  2. The script prints the execution_id at startup, then proceeds
     iterating. The SDK has opened an SSE halt-stream subscription
     in the background because the budget enables it.
  3. You open the dashboard at http://localhost:8080/ui/, find the
     running execution in the recent-executions list (status=started),
     click its row to open the detail page, click the red "⛔ Halt
     this execution" button at the top, confirm in the prompt.
  4. The dashboard POSTs /executions/{id}/halt. The SDK reader
     receives the SSE event, sets the remote-halt flag. The next
     @mesedi.tool boundary raises MesediHalt. The agent halts at
     that iteration, the finally block runs, the script exits with
     result=None.

Prereqs:
  - Backend running with sub-slice 21b.1 + 21c deployed.
  - SDK installed (mesedi v0.0.8+).

Run:
  python3 dashboard_halt_test.py

Verify in SQLite after:
  cd ../../backend
  sqlite3 mesedi-dev.db "SELECT execution_id, status, duration_ms, \
    crash_signature FROM executions WHERE \
    crash_signature='halt:remote_signal' \
    ORDER BY started_at DESC LIMIT 3;"
"""

import time

import mesedi
from mesedi import Budget
from mesedi._context import current_execution_context

mesedi.configure(
    api_key="mesedi_sk_dev_local_only",
    base_url="http://localhost:8080",
)


@mesedi.tool
def slow_tool() -> str:
    """300ms sleep per call. Emits one tool_call event per invocation;
    the halt-safe boundary check fires at each @tool entry."""
    time.sleep(0.3)
    return "done"


@mesedi.wrap(budget=Budget(max_wall_clock_seconds=60.0))
def dashboard_halt_agent() -> str:
    """Loops 100 times at 300ms each — ~30s normal runtime. The 60s
    wall-clock budget will NOT trip during normal execution; the
    only way this agent halts before completing is via the dashboard
    operator Halt button (the test scenario).

    Waits at startup for you to click before any iterations run.
    """
    ctx = current_execution_context()
    assert ctx is not None
    print()
    print("════════════════════════════════════════════════════════════════")
    print(f"  EXECUTION_ID:  {ctx.execution_id}")
    print("════════════════════════════════════════════════════════════════")
    print()
    print("  Open the dashboard:  http://localhost:8080/ui/")
    print("  Find the execution above in the Recent executions table,")
    print("  click into it, then click ⛔ Halt this execution.")
    print()
    print("  Starting in 3 seconds...")
    print()
    time.sleep(3.0)

    cleanup_ran = []
    try:
        for i in range(100):
            slow_tool()
            print(f"  [agent] iteration {i + 1} completed")
        return "all 100 iterations done — agent ran to completion (no halt clicked)"
    finally:
        cleanup_ran.append(True)
        print(f"\n  [agent] finally block ran (cleanup_ran={cleanup_ran})")


def _ms(seconds: float) -> str:
    return f"{seconds * 1000:.0f}ms"


if __name__ == "__main__":
    print("\n── Dashboard-halt demo ──")
    print("  No auto-trigger thread — YOU click Halt in the dashboard.")
    t = time.perf_counter()
    result = dashboard_halt_agent()
    print(f"\n  wall-clock: {_ms(time.perf_counter() - t)}")
    print(f"  result: {result!r}")
    if result is None:
        print("  → halted cleanly via dashboard")
    else:
        print("  → ran to completion — you didn't click Halt in time")

    print("\n── Flushing shipper queue... ──")
    t = time.perf_counter()
    ok = mesedi.flush(timeout=5.0)
    print(f"  flush ok={ok} in {_ms(time.perf_counter() - t)}")
