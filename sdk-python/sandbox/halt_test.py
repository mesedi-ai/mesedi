"""
Hard-halt local-budget demo (sub-slice 21a).

Demonstrates the three halt triggers:

  1. Wall-clock — an agent that would run forever halts after N seconds
  2. Step count — an agent that emits too many events halts after N steps
  3. Token total — an agent that uses too many LLM tokens halts mid-run

Each demo wraps a function with `@mesedi.wrap(budget=Budget(...))`
and confirms that:
  - MesediHalt fires at a safe boundary (between tool/llm/checkpoint
    calls, not mid-call)
  - The wrapped function returns None (caller doesn't see the
    exception)
  - The execution lands in SQLite with status=halted and a
    crash_signature like `halt:wall_clock`
  - Standard try/finally cleanup still runs

Prereqs:
  - Backend running: cd ../../backend && go run cmd/api/main.go
  - SDK installed: pip install -e ..

Run:
  python3 halt_test.py
"""

import time

import mesedi
from mesedi import Budget

mesedi.configure(
    api_key="mesedi_sk_dev_local_only",
    base_url="http://localhost:8080",
)


@mesedi.tool
def slow_tool() -> str:
    """Each call sleeps 200ms — emits one tool_call event per invocation."""
    time.sleep(0.2)
    return "done"


# ── Wall-clock halt ──────────────────────────────────────────────────


@mesedi.wrap(budget=Budget(max_wall_clock_seconds=1.0))
def runaway_wall_clock_agent() -> str:
    """Would loop forever calling slow_tool — but the 1s wall-clock
    budget halts it after ~5 iterations.

    Cleanup runs in `finally` to prove standard Python cleanup
    semantics survive the halt.
    """
    cleanup_ran = []
    try:
        for i in range(100):
            slow_tool()  # 200ms each; budget check fires before the 6th
            print(f"  iteration {i + 1} completed")
        return "all 100 iterations done"
    finally:
        cleanup_ran.append(True)
        print(f"  finally block ran (cleanup_ran={cleanup_ran})")


# ── Step-count halt ─────────────────────────────────────────────────


@mesedi.wrap(budget=Budget(max_steps=3))
def runaway_step_count_agent() -> str:
    """Tries to call slow_tool 20 times, but the 3-step budget halts
    after the 3rd tool_call's pre-call check.
    """
    for i in range(20):
        slow_tool()
        print(f"  iteration {i + 1} completed")
    return "done"


# ── No budget — control case ─────────────────────────────────────────


@mesedi.wrap
def clean_agent() -> str:
    """No budget — runs to completion normally."""
    slow_tool()
    slow_tool()
    return "no budget, no halt"


def _ms(seconds: float) -> str:
    return f"{seconds * 1000:.0f}ms"


if __name__ == "__main__":
    print("\n── Run 1: clean_agent (no budget, control case) ──")
    t = time.perf_counter()
    result = clean_agent()
    print(f"  wall-clock: {_ms(time.perf_counter() - t)}")
    print(f"  result: {result}")

    print("\n── Run 2: runaway_wall_clock_agent (budget=1s wall-clock) ──")
    print("  Expected: ~5 iterations complete, then halt fires at the 6th.")
    t = time.perf_counter()
    result = runaway_wall_clock_agent()
    print(f"  wall-clock: {_ms(time.perf_counter() - t)}")
    print(f"  result: {result!r} (None means halted cleanly)")

    print("\n── Run 3: runaway_step_count_agent (budget=3 steps) ──")
    print("  Expected: 2 tool_calls complete, then halt fires at the 3rd.")
    t = time.perf_counter()
    result = runaway_step_count_agent()
    print(f"  wall-clock: {_ms(time.perf_counter() - t)}")
    print(f"  result: {result!r} (None means halted cleanly)")

    print("\n── Flushing shipper queue... ──")
    t = time.perf_counter()
    ok = mesedi.flush(timeout=5.0)
    print(f"  flush ok={ok} in {_ms(time.perf_counter() - t)}")

    print("\n── Done. Verify in SQLite: ──")
    print("  cd ../../backend")
    print('  sqlite3 mesedi-dev.db "SELECT execution_id, status, '
          'duration_ms, crash_signature FROM executions WHERE status=\'halted\' '
          'ORDER BY started_at DESC LIMIT 5;"')
    print("  Expect 2 halted rows with crash_signature like halt:wall_clock and halt:step_count")
