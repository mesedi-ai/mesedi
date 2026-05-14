"""
Example using the real Mesedi SDK (v0.0.3 — @wrap + @tool + async).

Demonstrates both decorators together: a @wrap'd agent function that
calls several @tool-decorated tool functions. Each invocation produces
one Execution row (from @wrap) and one tool_call event row per tool
call (from @tool), all linked by execution_id and ordered by sequence.

Prereqs:
  - Backend running:
      cd ../../backend && go run cmd/api/main.go
  - SDK installed locally in editable mode:
      cd ../  &&  python3 -m pip install -e .

Run:
  python3 real_agent.py

Verify in SQLite:
  cd ../../backend
  sqlite3 mesedi-dev.db "
    SELECT execution_id, status, duration_ms, crash_signature
    FROM executions ORDER BY started_at DESC LIMIT 5;
    "
  sqlite3 mesedi-dev.db "
    SELECT event_type, sequence,
           json_extract(payload, '$.tool_name') AS tool,
           json_extract(payload, '$.status')    AS status,
           duration_ms
    FROM events ORDER BY id DESC LIMIT 10;
    "
"""

import random
import time

import mesedi

mesedi.configure(
    api_key="mesedi_sk_dev_local_only",
    base_url="http://localhost:8080",
)


# ── @tool examples ───────────────────────────────────────────────────


@mesedi.tool
def search_web(query: str) -> list:
    """Pretend tool: simulate a web-search call."""
    time.sleep(0.01)
    return [f"result for {query!r} #{i}" for i in range(3)]


@mesedi.tool
def calculator(a: int, b: int, op: str = "+") -> int:
    """Pretend tool: simulate a calculator call."""
    if op == "+":
        return a + b
    if op == "*":
        return a * b
    raise ValueError(f"unsupported op: {op!r}")


@mesedi.tool
def flaky_database_lookup(key: str) -> str:
    """Pretend tool: randomly fails to demonstrate failed tool_call events."""
    time.sleep(0.005)
    if random.random() < 0.5:
        raise ConnectionError(f"db unreachable while looking up {key!r}")
    return f"value-for-{key}"


# ── @wrap examples ───────────────────────────────────────────────────


@mesedi.wrap
def agent_with_tools(query: str) -> str:
    """Run an agent that uses three tools, two reliable, one flaky."""
    results = search_web(query)
    total = calculator(len(results), 10, op="*")

    # Try the flaky lookup but recover if it fails — we want @wrap to
    # see a "completed" execution even when an inner @tool raises.
    try:
        cached = flaky_database_lookup(query)
    except ConnectionError:
        cached = "(cache miss)"

    return f"answer for {query!r}: {total} results, cached={cached}"


@mesedi.wrap
def agent_that_crashes(query: str) -> str:
    """Agent that fires a tool then crashes from its own code."""
    _ = search_web(query)
    raise RuntimeError(f"agent gave up on {query!r}")


def _ms(seconds: float) -> str:
    return f"{seconds * 1000:.1f}ms"


if __name__ == "__main__":
    # Deterministic-ish: seed so the flaky tool's outcome is reproducible.
    random.seed(7)

    print("\n── Run 1: agent_with_tools (3 tool calls inside) ──")
    t = time.perf_counter()
    result = agent_with_tools("pickleball clubs in miami")
    print(f"  wall-clock: {_ms(time.perf_counter() - t)}")
    print(f"  result: {result}")

    print("\n── Run 2: agent_that_crashes (1 tool call, then crash) ──")
    t = time.perf_counter()
    try:
        agent_that_crashes("invalid input")
    except RuntimeError as e:
        print(f"  wall-clock: {_ms(time.perf_counter() - t)}")
        print(f"  caught (re-raised by @wrap): {e}")

    print("\n── Run 3: tool called OUTSIDE @wrap (should run unobserved) ──")
    direct_result = calculator(2, 3)  # no surrounding @wrap
    print(f"  direct calculator(2, 3) = {direct_result} (no event recorded)")

    print("\n── Flushing shipper queue... ──")
    t = time.perf_counter()
    ok = mesedi.flush(timeout=5.0)
    print(f"  flush ok={ok} in {_ms(time.perf_counter() - t)}")

    print("\n── Done. Verify in SQLite:")
    print("  cd ../../backend")
    print(
        '  sqlite3 mesedi-dev.db "SELECT execution_id, status, duration_ms, '
        'crash_signature FROM executions ORDER BY started_at DESC LIMIT 5;"'
    )
    print()
    print(
        '  sqlite3 mesedi-dev.db "SELECT event_type, sequence, '
        "json_extract(payload, \\'\\$.tool_name\\') AS tool, "
        "json_extract(payload, \\'\\$.status\\') AS status, "
        'duration_ms FROM events ORDER BY id DESC LIMIT 10;"'
    )
