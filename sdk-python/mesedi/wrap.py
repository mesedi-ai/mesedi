"""
@wrap decorator, observe a function as an agent execution.

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
    wrapped call is dataclass-construction + queue-enqueue , 
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
from typing import Any, Callable, Optional, TypeVar

from mesedi._context import (
    current_execution_context,
    pop_execution_context,
    push_execution_context,
)
from mesedi.client import get_client
from mesedi.events import Execution, Status, utcnow_rfc3339
from mesedi.halt import Budget, MesediHalt
from mesedi.halt_stream import HaltStreamReader

F = TypeVar("F", bound=Callable[..., Any])


def wrap(
    func: Optional[F] = None,
    *,
    budget: Optional[Budget] = None,
) -> Any:
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

    # Support both call shapes, bare `@mesedi.wrap` and
    # `@mesedi.wrap(budget=Budget(...))`. Detect which we got:
    # if `func` is None, we're in the "called with kwargs" form and
    # need to return a decorator factory. Otherwise this IS the
    # decorator.
    if func is None:
        # `@mesedi.wrap(budget=...)`, return a factory that takes
        # the actual function on the next call.
        def factory(actual: F) -> F:
            return wrap(actual, budget=budget)  # type: ignore[return-value]
        return factory

    @functools.wraps(func)
    def inner(*args: Any, **kwargs: Any) -> Any:
        client = get_client()
        execution_id = f"exec-{uuid.uuid4().hex[:12]}"
        execution = Execution(execution_id=execution_id)

        # Submit start AFTER constructing the Execution so the timer
        # captures only the user's function, not the SDK's own overhead.
        client.submit_execution_start(execution)
        start_wall = time.perf_counter()
        ctx_token = push_execution_context(execution_id, budget=budget)

        # Sub-slice 21b.2: if a budget is configured for this
        # execution, spawn a background SSE reader subscribed to
        # /executions/{id}/halt-stream. When the backend publishes a
        # halt, the reader calls tracker.signal_remote_halt(reason);
        # the next halt-safe boundary check then raises MesediHalt
        # with trigger="remote_signal". Fail-open: if the reader can't
        # subscribe (backend unreachable, etc.) the wrapped agent
        # still runs with whatever local budget it was given.
        halt_reader: Optional[HaltStreamReader] = None
        ctx = current_execution_context()
        if ctx is not None and ctx.budget_tracker is not None:
            halt_reader = HaltStreamReader(
                execution_id=execution_id,
                base_url=client.base_url,
                api_key=client.api_key,
                on_halt=ctx.budget_tracker.signal_remote_halt,
            )
            halt_reader.start()

        try:
            try:
                result = func(*args, **kwargs)
            except MesediHalt as halt_exc:
                # Halt is NOT a crash, it's a controlled stop the SDK
                # itself raised because a budget was exceeded. Mark
                # the execution `halted` with the trigger metadata
                # and return cleanly. We do NOT re-raise, the caller
                # of the @wrap'd function sees a None return (or
                # whatever the agent's normal "I gave up" value is)
                # rather than an exception.
                execution.status = Status.HALTED
                execution.duration_ms = _elapsed_ms(start_wall)
                execution.ended_at = utcnow_rfc3339()
                # Pack the halt reason into crash_signature so it shows
                # up in the dashboard's failure-group surface (reuses
                # the existing column rather than adding a new one).
                execution.crash_signature = f"halt:{halt_exc.trigger}"
                client.submit_execution_end(execution)
                return None
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
            # Stop the halt-stream reader BEFORE popping the context,
            # so any in-flight signal_remote_halt() call against the
            # tracker still finds a valid context. Daemon thread , 
            # safe to leave running; stop() just unblocks it.
            if halt_reader is not None:
                halt_reader.stop()
            # Pop the execution context on EVERY exit path, return,
            # exception, even keyboard-interrupt, so nested wraps
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
    identifying). Truncates to 16 hex chars, collisions across
    distinct crash sites are astronomically unlikely at this length and
    the shorter signature is easier to display in dashboards.

    Mirrors the grouping signature the backend's Phase 3a crash
    detector will compute. Keep these two implementations in sync.
    """
    tb_lines = traceback.format_exception(type(exc), exc, exc.__traceback__)
    fingerprint_input = f"{type(exc).__name__}\n" + "\n".join(tb_lines[:5])
    return hashlib.sha256(fingerprint_input.encode("utf-8")).hexdigest()[:16]
