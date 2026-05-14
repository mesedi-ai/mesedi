"""
Demo agent that emits enough events to trigger the step-count detector.

The backend's step-count detector groups any @wrap-decorated execution
that emitted more than 10 events into a failure_group with
failure_class=loops and a count-bucketed signature (step_count_10+,
_50+, _100+, _500+, _5000+).

This script fires two runs:
  1. A polite agent emitting 5 tool calls — UNDER the threshold.
  2. A chatty agent emitting 15 tool calls — over the threshold,
     lands in step_count_10+ bucket.

After running, the dashboard's Failure groups table should show ONE
new row (failure_class=loops, signature=step_count_10+) in addition
to the existing crashes + time_budget groups.

Prereqs:
  - Backend running: cd ../../backend && go run cmd/api/main.go
  - SDK installed: pip install -e ..

Run:
  python3 chatty_agent.py
"""

import time

import mesedi

mesedi.configure(
    api_key="mesedi_sk_dev_local_only",
    base_url="http://localhost:8080",
)


@mesedi.tool
def echo(s: str) -> str:
    """Trivial tool — every call emits a tool_call event."""
    return s


@mesedi.wrap
def polite_agent(query: str) -> str:
    """Emits 5 tool_call events. Under the step-count threshold."""
    out = []
    for i in range(5):
        out.append(echo(f"{query}-{i}"))
    return " | ".join(out)


@mesedi.wrap
def chatty_agent(query: str) -> str:
    """Emits 15 tool_call events. Over the step-count threshold (10).

    Lands in the step_count_10+ bucket because 15 falls in [10, 50).
    A 60-event agent would land in step_count_50+, etc.
    """
    out = []
    for i in range(15):
        out.append(echo(f"{query}-{i}"))
    return " | ".join(out)


def _ms(seconds: float) -> str:
    return f"{seconds * 1000:.0f}ms"


if __name__ == "__main__":
    print("\n── Run 1: polite_agent (5 tool_calls — under threshold) ──")
    t = time.perf_counter()
    result = polite_agent("hi")
    print(f"  wall-clock: {_ms(time.perf_counter() - t)}")
    print(f"  emitted: 5 tool_call events")

    print("\n── Run 2: chatty_agent (15 tool_calls — over threshold) ──")
    t = time.perf_counter()
    result = chatty_agent("hi")
    print(f"  wall-clock: {_ms(time.perf_counter() - t)}")
    print(f"  emitted: 15 tool_call events")

    print("\n── Flushing shipper queue... ──")
    t = time.perf_counter()
    ok = mesedi.flush(timeout=5.0)
    print(f"  flush ok={ok} in {_ms(time.perf_counter() - t)}")

    print("\n── Done. Verify: ──")
    print("  Dashboard: http://localhost:8080/ui/")
    print("  Expected: 4 failure groups visible (crashes + time_budget_1s+")
    print("            + time_budget_10s+ + step_count_10+).")
