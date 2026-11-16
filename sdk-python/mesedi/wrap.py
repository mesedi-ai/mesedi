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
import inspect
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
from mesedi.egress import emit_environment_declaration
from mesedi.events import Execution, Status, utcnow_rfc3339
from mesedi.halt import Budget, MesediHalt
from mesedi.halt_stream import HaltStreamReader

F = TypeVar("F", bound=Callable[..., Any])


def wrap(
    func: Optional[F] = None,
    *,
    budget: Optional[Budget] = None,
    tenant_id: Optional[str] = None,
    agent_name: Optional[str] = None,
    execution_mode: Optional[str] = None,
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

    Per-tenant cost attribution::

        @mesedi.wrap(tenant_id=current_customer_id())
        def run_my_agent(query: str) -> str:
            ...

    The ``tenant_id`` is attached to every execution this decorated
    function produces and surfaces in the ``GET /reports/cost-by-tenant``
    rollup. SaaS hosts running one Mesedi project across N customers
    should set this so the cost report breaks down per end-user instead
    of collapsing into one project-wide total.

    Multi-agent ergonomics::

        @mesedi.wrap(agent_name="planner")
        def planner(query: str) -> str:
            # No need to repeat from_agent="planner" at each emit:
            mesedi.emit_agent_handoff(to_agent="reviewer", ...)
            ...

    The ``agent_name`` is stored on the execution context and is used
    as a fallback by ``emit_agent_handoff`` when the caller does NOT
    pass ``from_agent``. Explicit ``from_agent`` always wins. If neither
    is supplied inside ``@wrap``, ``emit_agent_handoff`` raises
    ``ValueError`` rather than silently emitting an ``unknown`` source
    agent into the topology graph.

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
        # `@mesedi.wrap(budget=..., tenant_id=..., agent_name=...)`,
        # return a factory that takes the actual function on the next
        # call.
        def factory(actual: F) -> F:
            return wrap(  # type: ignore[return-value]
                actual,
                budget=budget,
                tenant_id=tenant_id,
                agent_name=agent_name,
                execution_mode=execution_mode,
            )
        return factory

    # Async functions need an async wrapper. Until 2026-11-16 there was
    # only the plain one: calling an async function returns a coroutine
    # at once, so the run was recorded as completed in 0 ms before any of
    # its work happened, and everything inside it went to no run at all
    # (the outside review of 2026-11-15). Both wrappers share _Run below, so the
    # lifecycle cannot drift between them.
    if inspect.iscoroutinefunction(func):
        @functools.wraps(func)
        async def ainner(*args: Any, **kwargs: Any) -> Any:
            run = _Run(budget, tenant_id, agent_name, execution_mode)
            try:
                try:
                    result = await func(*args, **kwargs)
                except MesediHalt as halt_exc:
                    run.halted(halt_exc)
                    return None
                except BaseException as exc:
                    run.crashed(exc)
                    raise
                run.completed()
                return result
            finally:
                run.close()

        return ainner  # type: ignore[return-value] -- functools.wraps keeps the decorated signature at runtime; mypy cannot see that

    @functools.wraps(func)
    def inner(*args: Any, **kwargs: Any) -> Any:
        run = _Run(budget, tenant_id, agent_name, execution_mode)
        try:
            try:
                result = func(*args, **kwargs)
            except MesediHalt as halt_exc:
                # Halt is NOT a crash, it's a controlled stop the SDK
                # itself raised because a budget was exceeded. The
                # caller of the @wrap'd function sees a None return
                # rather than an exception.
                run.halted(halt_exc)
                return None
            except BaseException as exc:
                run.crashed(exc)
                # Re-raise with the original traceback intact.
                raise
            run.completed()
            return result
        finally:
            run.close()

    return inner  # type: ignore[return-value] -- functools.wraps keeps the decorated signature at runtime; mypy cannot see that


class _Run:
    """One wrapped execution's lifecycle, shared by the plain and async
    wrappers: start, the three ways it can end, and cleanup."""

    def __init__(
        self,
        budget: Optional[Budget],
        tenant_id: Optional[str],
        agent_name: Optional[str],
        execution_mode: Optional[str],
    ) -> None:
        self.client = get_client()
        execution_id = f"exec-{uuid.uuid4().hex[:12]}"
        # A run opened inside another run records the outer one as its
        # parent, as the multi-agent docs promise (the outside review of 2026-11-15). Read the
        # context BEFORE pushing this run's own.
        outer = current_execution_context()
        # Pass tenant_id straight through to the Execution dataclass so
        # start_payload() includes it in POST /executions. None (the
        # default) means "not supplied" and the field is omitted from
        # the wire body, matching the backend's omitempty contract.
        self.execution = Execution(
            execution_id=execution_id,
            tenant_id=tenant_id,
            parent_execution_id=outer.execution_id if outer is not None else None,
        )

        # Submit start AFTER constructing the Execution so the timer
        # captures only the user's function, not the SDK's own overhead.
        self.client.submit_execution_start(self.execution)
        self.start_wall = time.perf_counter()
        self.ctx_token = push_execution_context(
            execution_id,
            budget=budget,
            agent_name=agent_name,
        )

        # Declared execution mode, the sugar for
        # emit_environment_declaration: `@mesedi.wrap(
        # execution_mode="simulation")` states the run's intended
        # environment as the first event, so the backend's
        # environment_misapprehension detector has a boundary to
        # check egress against. None (the default) declares nothing:
        # no boundary, no possible firing.
        if execution_mode:
            emit_environment_declaration(execution_mode, declared_by="wrap")

        # Sub-slice 21b.2: if a budget is configured for this
        # execution, spawn a background SSE reader subscribed to
        # /executions/{id}/halt-stream. When the backend publishes a
        # halt, the reader calls tracker.signal_remote_halt(reason);
        # the next halt-safe boundary check then raises MesediHalt
        # with trigger="remote_signal". Fail-open: if the reader can't
        # subscribe (backend unreachable, etc.) the wrapped agent
        # still runs with whatever local budget it was given.
        self.halt_reader: Optional[HaltStreamReader] = None
        ctx = current_execution_context()
        if ctx is not None and ctx.budget_tracker is not None:
            self.halt_reader = HaltStreamReader(
                execution_id=execution_id,
                base_url=self.client.base_url,
                api_key=self.client.api_key,
                on_halt=ctx.budget_tracker.signal_remote_halt,
            )
            self.halt_reader.start()

    def _end(self, status: str) -> None:
        self.execution.status = status
        self.execution.duration_ms = _elapsed_ms(self.start_wall)
        self.execution.ended_at = utcnow_rfc3339()

    def completed(self) -> None:
        self._end(Status.COMPLETED)
        self.client.submit_execution_end(self.execution)

    def halted(self, halt_exc: MesediHalt) -> None:
        self._end(Status.HALTED)
        # Pack the halt reason into crash_signature so it shows up in
        # the dashboard's failure-group surface (reuses the existing
        # column rather than adding a new one).
        self.execution.crash_signature = f"halt:{halt_exc.trigger}"
        self.client.submit_execution_end(self.execution)

    def crashed(self, exc: BaseException) -> None:
        self._end(Status.CRASHED)
        self.execution.crash_signature = _crash_signature(exc)
        self.execution.crash_type = type(exc).__name__
        self.client.submit_execution_end(self.execution)

    def close(self) -> None:
        # Stop the halt-stream reader BEFORE popping the context, so any
        # in-flight signal_remote_halt() call against the tracker still
        # finds a valid context. Then pop the execution context on EVERY
        # exit path, return, exception, even keyboard-interrupt, so
        # nested wraps correctly restore the outer context.
        if self.halt_reader is not None:
            self.halt_reader.stop()
        pop_execution_context(self.ctx_token)


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
