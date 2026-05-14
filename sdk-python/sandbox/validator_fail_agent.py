"""
Demo agent that fires failing validator_result events.

The backend's validator-failures detector groups any @wrap-decorated
execution containing at least one validator_result event with
payload.passed=False into a failure_group with
failure_class=validator_failures and signature=validator_name.

This is the silent-quality-degradation pattern: the agent ran to
completion, but the downstream quality check (schema conformance,
output non-emptiness, factuality, safety, etc.) failed.

Three runs:
  1. agent_with_passing_validators — both validators pass. Not grouped.
  2. agent_with_empty_output — schema validator passes, but the
     non_empty_response validator fails (signature: non_empty_response).
  3. agent_with_schema_failure — non_empty validator passes, but
     schema_conformance fails (signature: schema_conformance).

Two distinct validator names → two separate failure groups.

Prereqs:
  - Backend running: cd ../../backend && go run cmd/api/main.go
  - SDK installed: pip install -e ..

Run:
  python3 validator_fail_agent.py
"""

import time

import mesedi

mesedi.configure(
    api_key="mesedi_sk_dev_local_only",
    base_url="http://localhost:8080",
)


@mesedi.wrap
def agent_with_passing_validators(query: str) -> str:
    """Both validators pass — should NOT trigger validator-failures."""
    answer = f"Answer to {query!r}: a reasonable response."
    mesedi.validator_result("schema_conformance", passed=True)
    mesedi.validator_result("non_empty_response", passed=True)
    return answer


@mesedi.wrap
def agent_with_empty_output(query: str) -> str:
    """non_empty_response validator fails — should group as
    validator_failures with signature=non_empty_response."""
    answer = ""  # uh oh
    mesedi.validator_result("schema_conformance", passed=True)
    mesedi.validator_result(
        "non_empty_response",
        passed=False,
        message=f"response was empty (0 chars)",
        severity="error",
    )
    return answer


@mesedi.wrap
def agent_with_schema_failure(query: str) -> str:
    """schema_conformance validator fails — should group as
    validator_failures with signature=schema_conformance."""
    answer = "not a JSON object even though we promised one"
    mesedi.validator_result(
        "schema_conformance",
        passed=False,
        message="expected JSON object, got prose",
        severity="critical",
    )
    mesedi.validator_result("non_empty_response", passed=True)
    return answer


def _ms(seconds: float) -> str:
    return f"{seconds * 1000:.0f}ms"


if __name__ == "__main__":
    print("\n── Run 1: agent_with_passing_validators — should NOT be grouped ──")
    t = time.perf_counter()
    agent_with_passing_validators("hi")
    print(f"  wall-clock: {_ms(time.perf_counter() - t)}")

    print("\n── Run 2: agent_with_empty_output — should group as non_empty_response ──")
    t = time.perf_counter()
    agent_with_empty_output("hi")
    print(f"  wall-clock: {_ms(time.perf_counter() - t)}")

    print("\n── Run 3: agent_with_schema_failure — should group as schema_conformance ──")
    t = time.perf_counter()
    agent_with_schema_failure("hi")
    print(f"  wall-clock: {_ms(time.perf_counter() - t)}")

    print("\n── Flushing shipper queue... ──")
    t = time.perf_counter()
    ok = mesedi.flush(timeout=5.0)
    print(f"  flush ok={ok} in {_ms(time.perf_counter() - t)}")

    print("\n── Done. Verify: ──")
    print("  Dashboard: http://localhost:8080/ui/")
    print("  Expected: 2 new failure groups (non_empty_response, schema_conformance)")
