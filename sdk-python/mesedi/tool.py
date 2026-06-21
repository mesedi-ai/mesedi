"""
@tool decorator, observe a function as an agent tool invocation.

When called from inside a ``@mesedi.wrap``-decorated function, each
invocation emits a ``tool_call`` event linked to the surrounding
execution. The event carries:

  - ``tool_name``: the function's ``__name__`` (or override via
    ``@tool(name="...")``, coming in a later sub-slice)
  - ``arguments``: sanitized, truncated repr of positional + keyword args
  - ``status``: "ok" if the function returned, "failed" if it raised
  - ``result_summary``: truncated repr of the return value (on success)
  - ``exception_type`` / ``exception_message``: on failure
  - ``duration_ms``: wall-clock time spent in the tool

If ``@tool`` is called OUTSIDE a ``@wrap`` context (no active execution
in the context var), the wrapped function runs normally and no event is
emitted, fail-open, matching the design of @wrap.

Exception semantics: exceptions raised by the tool propagate to the
caller unchanged. The tool_call event records the failure but does NOT
consume the exception. Whether the surrounding agent recovers (catches
the exception and continues) or propagates further (lets @wrap mark
the whole execution crashed) is up to the agent code.
"""

from __future__ import annotations

import functools
import json
import time
import uuid
from typing import Any, Callable, Dict, Optional, TypeVar

from mesedi._context import current_execution_context
from mesedi.client import get_client
from mesedi.events import Event, EventType, utcnow_rfc3339

F = TypeVar("F", bound=Callable[..., Any])

# Payload-truncation budgets. Tools that take or return huge strings
# (a whole web page, a long prompt, etc.) should not blow up the events
# table, the truncated repr is enough for debugging and pattern
# recognition.
_MAX_ARG_REPR = 200
_MAX_RESULT_REPR = 500
_MAX_EXC_MSG = 500
# Cap for the JSON-serialized return_value field. Larger than
# _MAX_RESULT_REPR because the detector consumes the structural
# fingerprint, not the value text — preserving more of the structure
# is more valuable than a tight cap. 2048 chars covers typical tool
# responses (objects with a dozen keys, short string values) while
# still bounding pathological large returns.
_MAX_RETURN_VALUE_JSON = 2048


def _structured_return_value(result: Any) -> Any:
    """Return ``result`` as a JSON-native Python value (str/int/
    float/bool/None/list/dict) suitable for embedding in the
    ``return_value`` payload field, or ``None`` when the result is
    unserializable.

    The backend's ``tool_schema_drift`` detector computes a
    structural fingerprint (sorted keys + value types) from this
    field — it needs valid JSON structure, not a Python ``repr``.

    Implementation:
      1. Serialize once with ``default=str`` so non-JSON-native
         values (datetime, UUID, custom objects) get coerced to
         strings rather than crashing.
      2. Measure the serialized size; if it exceeds the cap, return
         the sentinel string ``"<truncated>"`` so the detector
         treats this call as non-comparable rather than mis-
         fingerprinting a partial structure.
      3. Round-trip through ``json.loads`` so the returned value is
         guaranteed JSON-native (the shipper's later ``json.dumps``
         call has no ``default=`` and would crash on raw datetime /
         UUID values).

    Returns ``None`` when serialization is impossible (e.g. a self-
    referential structure that defeats ``default=str``), so the
    caller can omit the field cleanly.
    """
    try:
        encoded = json.dumps(result, default=str, ensure_ascii=False)
    except (TypeError, ValueError):
        return None
    if len(encoded) > _MAX_RETURN_VALUE_JSON:
        return "<truncated>"
    try:
        return json.loads(encoded)
    except (TypeError, ValueError):
        return None


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
            # No active execution, run unobserved. This is the
            # fail-open path for tests / scripts that call a tool
            # directly without going through @wrap.
            return func(*args, **kwargs)

        # Halt-safe boundary: budget check runs BEFORE the tool's
        # work, so a halt fires at this checkpoint rather than mid-
        # tool. Lets standard try/finally cleanup unwind cleanly.
        ctx.check_budget()

        client = get_client()
        tool_name = getattr(func, "__name__", "<unknown>")
        sequence = ctx.next_sequence()
        event_id = f"evt-{uuid.uuid4().hex[:12]}"
        args_summary = _summarize_args(args, kwargs)
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.increment_steps()

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
        payload: Dict[str, Any] = {
            "tool_name": tool_name,
            "arguments": args_summary,
            "status": "ok",
            # Human-readable repr for dashboard display. Kept
            # alongside structured return_value below so the UI
            # can render whatever shape the customer's tool
            # actually returns.
            "result_summary": _truncate(repr(result), _MAX_RESULT_REPR),
        }
        # Structured JSON form for backend detectors (specifically
        # tool_schema_drift, which fingerprints the return shape).
        # Only present when the result is JSON-serializable;
        # detectors gracefully no-op on its absence.
        structured = _structured_return_value(result)
        if structured is not None:
            payload["return_value"] = structured
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
