"""
@tool decorator — observe a function as an agent tool invocation.

When called from inside a ``@mesedi.wrap``-decorated function, each
invocation emits a ``tool_call`` event linked to the surrounding
execution. The event carries:

  - ``tool_name``: the function's ``__name__`` (or override via
    ``@tool(name="...")`` — coming in a later sub-slice)
  - ``arguments``: sanitized, truncated repr of positional + keyword args
  - ``status``: "ok" if the function returned, "failed" if it raised
  - ``result_summary``: truncated repr of the return value (on success)
  - ``exception_type`` / ``exception_message``: on failure
  - ``duration_ms``: wall-clock time spent in the tool

If ``@tool`` is called OUTSIDE a ``@wrap`` context (no active execution
in the context var), the wrapped function runs normally and no event is
emitted — fail-open, matching the design of @wrap.

Exception semantics: exceptions raised by the tool propagate to the
caller unchanged. The tool_call event records the failure but does NOT
consume the exception. Whether the surrounding agent recovers (catches
the exception and continues) or propagates further (lets @wrap mark
the whole execution crashed) is up to the agent code.
"""

from __future__ import annotations

import functools
import time
import uuid
from typing import Any, Callable, Dict, TypeVar

from mesedi._context import current_execution_context
from mesedi.client import get_client
from mesedi.events import Event, EventType, utcnow_rfc3339

F = TypeVar("F", bound=Callable[..., Any])

# Payload-truncation budgets. Tools that take or return huge strings
# (a whole web page, a long prompt, etc.) should not blow up the events
# table — the truncated repr is enough for debugging and pattern
# recognition.
_MAX_ARG_REPR = 200
_MAX_RESULT_REPR = 500
_MAX_EXC_MSG = 500


def tool(func: F) -> F:
    """Decorate a function as an observable tool call.

    Example::

        import mesedi

        @mesedi.tool
        def search_web(query: str) -> list[str]:
            ...

        @mesedi.wrap
        def run_agent(question: str) -> str:
            results = search_web(question)
            return f"Found {len(results)} results"

    Each call to ``search_web`` from inside ``run_agent`` will emit a
    ``tool_call`` event tagged with the enclosing execution_id and the
    next sequence number for that execution.
    """

    @functools.wraps(func)
    def inner(*args: Any, **kwargs: Any) -> Any:
        ctx = current_execution_context()
        if ctx is None:
            # No active execution — run unobserved. This is the
            # fail-open path for tests / scripts that call a tool
            # directly without going through @wrap.
            return func(*args, **kwargs)

        client = get_client()
        tool_name = getattr(func, "__name__", "<unknown>")
        sequence = ctx.next_sequence()
        event_id = f"evt-{uuid.uuid4().hex[:12]}"
        args_summary = _summarize_args(args, kwargs)

        start_wall = time.perf_counter()
        try:
            result = func(*args, **kwargs)
        except BaseException as exc:
            duration_ms = _elapsed_ms(start_wall)
            payload: Dict[str, Any] = {
                "tool_name": tool_name,
                "arguments": args_summary,
                "status": "failed",
                "exception_type": type(exc).__name__,
                "exception_message": _truncate(str(exc), _MAX_EXC_MSG),
            }
            client.submit_event(Event(
                event_id=event_id,
                execution_id=ctx.execution_id,
                event_type=EventType.TOOL_CALL,
                sequence=sequence,
                timestamp=utcnow_rfc3339(),
                duration_ms=duration_ms,
                payload=payload,
            ))
            raise

        duration_ms = _elapsed_ms(start_wall)
        payload = {
            "tool_name": tool_name,
            "arguments": args_summary,
            "status": "ok",
            "result_summary": _truncate(repr(result), _MAX_RESULT_REPR),
        }
        client.submit_event(Event(
            event_id=event_id,
            execution_id=ctx.execution_id,
            event_type=EventType.TOOL_CALL,
            sequence=sequence,
            timestamp=utcnow_rfc3339(),
            duration_ms=duration_ms,
            payload=payload,
        ))
        return result

    return inner  # type: ignore[return-value]


def _elapsed_ms(start_wall: float) -> int:
    return int((time.perf_counter() - start_wall) * 1000)


def _summarize_args(args: Any, kwargs: Any) -> Dict[str, Any]:
    """Produce a JSON-friendly, length-bounded summary of call arguments."""
    return {
        "args": [_truncate(repr(a), _MAX_ARG_REPR) for a in args],
        "kwargs": {k: _truncate(repr(v), _MAX_ARG_REPR) for k, v in kwargs.items()},
    }


def _truncate(s: str, max_len: int) -> str:
    """Truncate s to max_len chars, indicating truncation with an ellipsis."""
    if len(s) <= max_len:
        return s
    # -3 to leave room for the "..." marker; the truncated string is
    # still <= max_len total.
    return s[: max_len - 3] + "..."
