"""
Example using the real Mesedi SDK (v0.0.1).

This is the first time the actual SDK exercises the actual backend — the
predecessor `fake_agent.py` made the raw HTTP calls by hand to validate
the wire format. With the SDK in place, the same outcome (executions
recorded in SQLite) is achieved through a single decorator.

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
    """Pretend to be a real agent. Does fake work, returns an answer."""
    time.sleep(0.05)
    return f"Answer to {query!r}: 42"


@mesedi.wrap
def crashing_agent(query: str) -> str:
    """Pretend to be a buggy agent. Crashes deliberately."""
    time.sleep(0.02)
    raise ValueError(f"Could not parse {query!r}")


if __name__ == "__main__":
    print("\n── Run 1: successful agent ──")
    result = successful_agent("what is the meaning of life?")
    print(f"  result: {result}")

    print("\n── Run 2: crashing agent ──")
    try:
        crashing_agent("bad input")
    except ValueError as e:
        print(f"  caught (re-raised by @wrap, as expected): {e}")

    print("\n── Done. Two executions recorded.")
    print("Verify in SQLite:")
    print("  cd ../../backend")
    print(
        '  sqlite3 mesedi-dev.db "SELECT execution_id, status, '
        'duration_ms, crash_signature FROM executions ORDER BY '
        'started_at DESC LIMIT 5;"'
    )
