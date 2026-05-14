"""
@wrap decorator — observe a function as an agent execution.

A function decorated with @wrap becomes "an agent execution" from the
Mesedi backend's point of view. The decorator records start, completion
(or crash), wall-clock duration, and a stable crash signature suitable
for the Phase 3 crash-grouping detector.

Since sub-slice 3 (@tool), @wrap also pushes an execution context onto
a ``contextvars`` ContextVar so that ``@mesedi.tool``-decorated
functions called inside the wrapped body can attach their tool_call
events to this execution.

**Design goals:**

  - **Never break the wrapped function.** Mesedi outages must NOT
    block the agent. Observation failures are logged inside the
    shipper thread and swallowed.
  - **Re-raise exceptions transparently.** The original exception, with
    its original traceback, propagates to the caller. @wrap records
    ``status=crashed`` but never consumes the exception.
  - **Async-by-default.** ``submit_*`` returns immediately; actual HTTP
    happens in the background shipper thread. Latency added to the
    wrapped call is dataclass-construction + queue-enqueue —
    microseconds-scale.
  - **Nested @wrap calls.** A @wrap'd function calling another @wrap'd
    function produces two distinct executions; the inner one becomes
    "current" while it runs, then the outer context is restored on
    return. Tool calls always attach to the innermost active execution.
"""

from __future__ import annotations

import functools
import hashlib
import time
import traceback
import uuid
from typing import Any, Callable, TypeVar

from mesedi._context import pop_execution_context, push_execution_context
from mesedi.client import get_client
from mesedi.events import Execution, Status, utcnow_rfc3339

F = TypeVar("F", bound=Callable[..., Any])


def wrap(func: F) -> F:
    """Decorate a function so each call is recorded as an agent execution.

    Example::

        import mesedi

        mesedi.configure(api_key="mesedi_sk_...")

        @mesedi.wrap
        def run_my_agent(query: str) -> str:
            # ... agent logic ...
            return answer

        run_my_agent("hello")  # → backend records 1 execution

    Behavior:
      - On entry: enqueue POST /executions (status=started). Returns
        immediately; HTTP happens in the background shipper thread.
        Push an execution context onto the ContextVar so any
        @mesedi.tool calls inside the function attach to this run.
      - On normal return: enqueue PATCH /executions/{id}
        (status=completed, ended_at, duration_ms). Returns the
        function's return value unchanged.
      - On any raised exception: enqueue PATCH /executions/{id}
        (status=crashed, crash_signature), then re-raise the original
        exception with its original traceback.
      - Always: pop the execution context, even on exception.
    """

    @functools.wraps(func)
    def inner(*args: Any, **kwargs: Any) -> Any:
        client = get_client()
        execution_id = f"exec-{uuid.uuid4().hex[:12]}"
        execution = Execution(execution_id=execution_id)

        # Submit start AFTER constructing the Execution so the timer
        # captures only the user's function — not the SDK's own overhead.
        client.submit_execution_start(execution)
        start_wall = time.perf_counter()
        ctx_token = push_execution_context(execution_id)

        try:
            try:
                result = func(*args, **kwargs)
            except BaseException as exc:
                execution.status = Status.CRASHED
                execution.duration_ms = _elapsed_ms(start_wall)
                execution.ended_at = utcnow_rfc3339()
                execution.crash_signature = _crash_signature(exc)
                client.submit_execution_end(execution)
                # Re-raise with the original traceback intact. `raise`
                # (no argument) re-raises the active exception.
                raise

            execution.status = Status.COMPLETED
            execution.duration_ms = _elapsed_ms(start_wall)
            execution.ended_at = utcnow_rfc3339()
            client.submit_execution_end(execution)
            return result
        finally:
            # Pop the execution context on EVERY exit path — return,
            # exception, even keyboard-interrupt — so nested wraps
            # correctly restore the outer context.
            pop_execution_context(ctx_token)

    return inner  # type: ignore[return-value]


def _elapsed_ms(start_wall: float) -> int:
    """Wall-clock elapsed milliseconds since a perf_counter() reading."""
    return int((time.perf_counter() - start_wall) * 1000)


def _crash_signature(exc: BaseException) -> str:
    """Stable short hash for grouping identical crashes.

    Computes SHA-256 over: exception class name + first 5 lines of the
    formatted traceback (most-recent-call frames are the most
    identifying). Truncates to 16 hex chars — collisions across
    distinct crash sites are astronomically unlikely at this length and
    the shorter signature is easier to display in dashboards.

    Mirrors the grouping signature the backend's Phase 3a crash
    detector will compute. Keep these two implementations in sync.
    """
    tb_lines = traceback.format_exception(type(exc), exc, exc.__traceback__)
    fingerprint_input = f"{type(exc).__name__}\n" + "\n".join(tb_lines[:5])
    return hashlib.sha256(fingerprint_input.encode("utf-8")).hexdigest()[:16]
