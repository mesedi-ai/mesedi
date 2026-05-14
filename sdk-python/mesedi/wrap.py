"""
@wrap decorator — the simplest possible agent observation primitive.

A function decorated with @wrap becomes "an agent execution" from the
Mesedi backend's point of view. The decorator records start, completion
(or crash), wall-clock duration, and a stable crash signature suitable
for the Phase 3 crash-grouping detector.

Design goals:
  - **Never break the wrapped function.** If the Mesedi backend is down,
    if the network is flaky, if the api_key is wrong — the agent code
    must still run and its result must still flow through unchanged.
    Observation failures are logged but swallowed.
  - **Re-raise exceptions transparently.** If the wrapped function
    raises, the SAME exception (with its original traceback) propagates
    to the caller. The decorator records `status=crashed` along the way
    but does NOT consume the exception.
  - **Synchronous in v0.0.1.** Async buffering lands in the next slice.
    Today every call to a @wrap-decorated function adds two synchronous
    HTTP round-trips of latency. Acceptable for local dev; will be
    optimized before any production deployment.
"""

from __future__ import annotations

import functools
import hashlib
import logging
import time
import traceback
import uuid
from typing import Any, Callable, TypeVar

from mesedi.client import get_client
from mesedi.events import Execution, Status, utcnow_rfc3339

F = TypeVar("F", bound=Callable[..., Any])

logger = logging.getLogger("mesedi.wrap")


def wrap(func: F) -> F:
    """Decorate a function so each call is recorded as an agent execution.

    Example:

        import mesedi

        mesedi.configure(api_key="mesedi_sk_...")

        @mesedi.wrap
        def run_my_agent(query: str) -> str:
            # ... agent logic ...
            return answer

        run_my_agent("hello")  # → backend records 1 execution

    Behavior:
      - On entry: POST /executions (status=started).
      - On normal return: PATCH /executions/{id} (status=completed,
        ended_at, duration_ms). Returns the function's return value
        unchanged.
      - On any raised exception: PATCH /executions/{id}
        (status=crashed, crash_signature), then re-raises the original
        exception with original traceback preserved.
      - On Mesedi backend errors during observation: logs a warning and
        proceeds. The wrapped function's result is never affected by
        observation failures.
    """

    @functools.wraps(func)
    def inner(*args: Any, **kwargs: Any) -> Any:
        client = get_client()
        execution_id = f"exec-{uuid.uuid4().hex[:12]}"
        execution = Execution(execution_id=execution_id)

        start_wall = time.perf_counter()

        # ── observe: start ────────────────────────────────────────────
        try:
            client.create_execution(execution)
        except Exception as obs_err:
            # Fail-open: log but proceed unobserved. The wrapped
            # function's behavior must NEVER be blocked by a Mesedi
            # outage.
            logger.warning(
                "mesedi: create_execution failed (running unobserved): %s",
                obs_err,
            )
            return func(*args, **kwargs)

        # ── run the actual function ───────────────────────────────────
        try:
            result = func(*args, **kwargs)
        except BaseException as exc:
            execution.status = Status.CRASHED
            execution.ended_at = utcnow_rfc3339()
            execution.duration_ms = _elapsed_ms(start_wall)
            execution.crash_signature = _crash_signature(exc)
            try:
                client.update_execution(execution)
            except Exception as obs_err:
                logger.warning(
                    "mesedi: update_execution (crashed) failed: %s",
                    obs_err,
                )
            # Re-raise with original traceback intact. `raise` (no
            # argument) does the right thing inside an except block.
            raise

        # ── observe: completed ────────────────────────────────────────
        execution.status = Status.COMPLETED
        execution.ended_at = utcnow_rfc3339()
        execution.duration_ms = _elapsed_ms(start_wall)
        try:
            client.update_execution(execution)
        except Exception as obs_err:
            logger.warning(
                "mesedi: update_execution (completed) failed: %s",
                obs_err,
            )
        return result

    return inner  # type: ignore[return-value]


def _elapsed_ms(start_wall: float) -> int:
    """Wall-clock elapsed milliseconds since a perf_counter() reading."""
    return int((time.perf_counter() - start_wall) * 1000)


def _crash_signature(exc: BaseException) -> str:
    """Stable short hash for grouping identical crashes.

    Computes SHA-256 over: exception class name + first 5 lines of the
    formatted traceback (deepest frames; the most recent call is the
    most identifying). Truncates to 16 hex chars — collisions across
    distinct crash sites are astronomically unlikely at this length and
    the shorter signature is easier to display in dashboards.

    Mirrors the grouping signature the backend's Phase 3a crash
    detector will compute (so SDK-side and backend-side groupings agree
    when both are computing). Keep these two implementations in sync.
    """
    tb_lines = traceback.format_exception(type(exc), exc, exc.__traceback__)
    fingerprint_input = f"{type(exc).__name__}\n" + "\n".join(tb_lines[:5])
    return hashlib.sha256(fingerprint_input.encode("utf-8")).hexdigest()[:16]
