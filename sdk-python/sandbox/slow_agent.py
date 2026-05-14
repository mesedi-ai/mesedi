"""
Demo agent that exceeds the v0.0.1 time-budget threshold (1 second).

The backend's time-budget detector groups any @wrap-decorated execution
whose total duration > 1000ms into a failure_group with
failure_class=loops and a duration-bucketed signature
(time_budget_1s+, _10s+, _60s+, _10m+, _1h+).

This script fires three runs:
  1. A 0.5s agent — UNDER the threshold, no time-budget grouping.
  2. A 2s agent — 1s+ bucket, gets grouped into time_budget_1s+ group.
  3. A 12s agent — 10s+ bucket, gets grouped into time_budget_10s+ group.

After running, the dashboard's Failure groups table should show TWO
new rows (one per signature bucket), each with failure_class=loops
and an amber/warning badge.

Prereqs:
  - Backend running: cd ../../backend && go run cmd/api/main.go
  - SDK installed: pip install -e ..

Run:
  python3 slow_agent.py

Verify:
  http://localhost:8080/ui/ → should now show 3 failure groups
  (existing crashes + new time_budget_1s+ + new time_budget_10s+).
"""

import time

import mesedi

mesedi.configure(
    api_key="mesedi_sk_dev_local_only",
    base_url="http://localhost:8080",
)


@mesedi.wrap
def quick_agent(query: str) -> str:
    """500ms — under the 1s threshold, no time-budget grouping."""
    time.sleep(0.5)
    return f"finished quickly: {query}"


@mesedi.wrap
def slow_agent(query: str) -> str:
    """2s — over the 1s threshold, lands in time_budget_1s+ bucket."""
    time.sleep(2.0)
    return f"finished slowly: {query}"


@mesedi.wrap
def very_slow_agent(query: str) -> str:
    """12s — over the 10s threshold, lands in time_budget_10s+ bucket."""
    time.sleep(12.0)
    return f"finished after a long wait: {query}"


def _ms(seconds: float) -> str:
    return f"{seconds * 1000:.0f}ms"


if __name__ == "__main__":
    print("\n── Run 1: quick_agent (500ms — should NOT trigger time-budget) ──")
    t = time.perf_counter()
    quick_agent("ping")
    print(f"  wall-clock: {_ms(time.perf_counter() - t)}")

    print("\n── Run 2: slow_agent (2s — should land in time_budget_1s+) ──")
    t = time.perf_counter()
    slow_agent("medium")
    print(f"  wall-clock: {_ms(time.perf_counter() - t)}")

    print("\n── Run 3: very_slow_agent (12s — should land in time_budget_10s+) ──")
    print("  (this run takes 12 seconds, be patient)")
    t = time.perf_counter()
    very_slow_agent("long")
    print(f"  wall-clock: {_ms(time.perf_counter() - t)}")

    print("\n── Flushing shipper queue... ──")
    t = time.perf_counter()
    ok = mesedi.flush(timeout=5.0)
    print(f"  flush ok={ok} in {_ms(time.perf_counter() - t)}")

    print("\n── Done. Verify in dashboard or via curl: ──")
    print("  Dashboard: http://localhost:8080/ui/")
    print("  curl: curl -s -H 'Authorization: Bearer mesedi_sk_dev_local_only' \\")
    print("    -H 'X-Mesedi-Schema-Version: 1' \\")
    print("    http://localhost:8080/failure-groups | python3 -m json.tool")
