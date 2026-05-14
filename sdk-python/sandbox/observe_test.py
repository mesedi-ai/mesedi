"""
End-to-end test of mesedi.checkpoint() + mesedi.validator_result().

Both are direct-emission helpers (not decorators). Each emits an event
inside a surrounding @mesedi.wrap context; both no-op gracefully when
called outside.

Prereqs:
  - Backend running:
      cd ../../backend && go run cmd/api/main.go
  - SDK installed in editable mode:
      cd ../ && python3 -m pip install -e .

Run:
  python3 observe_test.py

Verify in SQLite:
  cd ../../backend
  sqlite3 mesedi-dev.db "
    SELECT event_type, sequence,
           json_extract(payload, '$.name')    AS name,
           json_extract(payload, '$.passed')  AS passed,
           json_extract(payload, '$.severity') AS severity
    FROM events ORDER BY rowid DESC LIMIT 10;
    "
"""

import time

import mesedi

mesedi.configure(
    api_key="mesedi_sk_dev_local_only",
    base_url="http://localhost:8080",
)


@mesedi.wrap
def agent_with_observations(query: str) -> str:
    """Realistic agent flow: checkpoints + validators sprinkled in."""
    mesedi.checkpoint("started", input_length=len(query))

    # Pretend retrieval step
    time.sleep(0.01)
    results = [f"doc-{i}" for i in range(3)]
    mesedi.checkpoint(
        "after_retrieval",
        num_results=len(results),
        used_cache=False,
    )

    # Validate the retrieval succeeded
    if not results:
        mesedi.validator_result(
            name="non-empty-retrieval",
            passed=False,
            message="retrieval returned 0 documents",
            severity="critical",
        )
        return "no results"
    mesedi.validator_result(name="non-empty-retrieval", passed=True)

    # Pretend synthesis step
    time.sleep(0.01)
    answer = f"Based on {len(results)} documents, the answer is 42."
    mesedi.checkpoint("after_synthesis", answer_length=len(answer))

    # Validate the answer length
    if len(answer) < 10:
        mesedi.validator_result(
            name="answer-too-short",
            passed=False,
            message=f"answer was {len(answer)} chars (min 10)",
            severity="warning",
        )
    else:
        mesedi.validator_result(name="answer-too-short", passed=True)

    return answer


def _ms(seconds: float) -> str:
    return f"{seconds * 1000:.1f}ms"


if __name__ == "__main__":
    print("\n── Run 1: agent_with_observations (4 checkpoints + 2 validators) ──")
    t = time.perf_counter()
    result = agent_with_observations("what is the meaning of life?")
    print(f"  wall-clock: {_ms(time.perf_counter() - t)}")
    print(f"  result: {result}")

    print("\n── Run 2: checkpoint outside @wrap (should no-op silently) ──")
    mesedi.checkpoint("loose_checkpoint", note="fired outside any @wrap")
    mesedi.validator_result("loose_validator", passed=True)
    print("  both calls returned cleanly with no event recorded")

    print("\n── Flushing shipper queue... ──")
    t = time.perf_counter()
    ok = mesedi.flush(timeout=5.0)
    print(f"  flush ok={ok} in {_ms(time.perf_counter() - t)}")

    print("\n── Done. Verify in SQLite:")
    print("  cd ../../backend")
    print(
        '  sqlite3 mesedi-dev.db "SELECT event_type, sequence, '
        "json_extract(payload, '\\$.name') AS name, "
        "json_extract(payload, '\\$.passed') AS passed, "
        "json_extract(payload, '\\$.severity') AS severity "
        'FROM events ORDER BY rowid DESC LIMIT 10;"'
    )
