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
# Cap for the JSON-serialized return_value field on the wire.
# The SDK uses this as a generous upper bound; the backend applies
# its OWN per-project cap (default 8 KB) at fingerprint time so
# customers tune storage / fingerprint policy from the dashboard
# without an SDK redeploy (see #270.a, migration 042).
#
# Raised from the v0.5.0 hardcoded 2048 to 16384 (16 KB) so the
# SDK no longer aggressively truncates returns that the backend
# would happily fingerprint. Larger returns still get the
# "<truncated>" sentinel — the cap exists to bound bandwidth and
# memory, not to enforce policy. Customer-facing policy lives in
# the backend per-project config.
_MAX_RETURN_VALUE_JSON = 16384


def _structured_return_value(result: Any) -> Any:
    """Return ``result`` as a JSON-native, schema-preserving Python
    value suitable for embedding in the ``return_value`` payload
    field, or ``None`` when the result is unserializable.

    The backend's ``tool_schema_drift`` detector computes a
    structural fingerprint (sorted keys + value types) from this
    field. The original v0.5.0 implementation used
    ``json.dumps(default=str)`` which collapsed every non-JSON-native
    value to a plain string — so ``{"created_at": datetime}`` and
    ``{"created_at": "regular string"}`` produced an IDENTICAL
    fingerprint, masking real schema drift between the two shapes
    (#270.b).

    Fix: type-tagged sentinels. A datetime becomes
    ``{"__type__": "datetime", "value": "..."}``; a UUID becomes
    ``{"__type__": "uuid", "value": "..."}``; etc. The structural
    hash now sees a distinct shape for typed-vs-plain-string fields,
    so the detector catches drift between them.

    The ``__type__`` tag vocabulary is closed:
      - ``"datetime"``, ``"uuid"``, ``"decimal"``, ``"bytes"``,
        ``"path"``: well-known stdlib types
      - ``"object"``: custom class, payload includes ``"class"`` name
      - ``"unserializable"``: last-resort tag for values that even
        ``repr()`` rejects

    Pure JSON-native inputs (str/int/float/bool/None/list/dict)
    pass through unchanged so the common case has zero overhead and
    is byte-identical to a hand-written JSON.

    Returns ``None`` for inputs whose final encoded size exceeds the
    cap (replaced by ``"<truncated>"``) or whose walking raises
    (e.g. circular refs detected via ``id``-tracking visited set).
    """
    try:
        walked = _walk_for_shape(result, set())
    except _StructuredWalkError:
        return None
    try:
        encoded = json.dumps(walked, ensure_ascii=False)
    except (TypeError, ValueError):
        return None
    if len(encoded) > _MAX_RETURN_VALUE_JSON:
        return "<truncated>"
    # Round-trip-safe by construction: every non-native value already
    # got tagged as a typed sentinel above. Returning ``walked``
    # directly avoids the parse cost; ``json.loads(encoded)`` would
    # produce an identical structure.
    return walked


class _StructuredWalkError(Exception):
    """Raised internally when the walker can't produce a serializable
    output (circular reference, irrecoverable object). Caught by
    :func:`_structured_return_value` to omit the field cleanly."""


def _walk_for_shape(value: Any, visited: set) -> Any:
    """Recursively coerce ``value`` to a JSON-native form that
    preserves type information via typed sentinels. ``visited``
    tracks object id()s on the current path so circular refs raise
    rather than infinite-loop.

    Primitive types pass through. Containers recurse. Non-JSON-
    native objects produce a ``{"__type__": "...", ...}`` sentinel.
    """
    # Fast path: JSON-native primitives.
    if value is None or isinstance(value, (str, int, float, bool)):
        # Note: bool is intentionally allowed (it's a JSON-native
        # type). int subclassing of bool is handled correctly by
        # json.dumps later.
        # Reject NaN / Infinity which json.dumps will refuse anyway.
        if isinstance(value, float) and (
            value != value or value in (float("inf"), float("-inf"))
        ):
            return {"__type__": "object", "class": "float", "value": str(value)}
        return value

    # Container types: recurse, guarding against cycles.
    if isinstance(value, dict):
        return _walk_dict(value, visited)
    if isinstance(value, (list, tuple)):
        return _walk_sequence(value, visited)

    # Well-known stdlib types — tag and stringify so the structural
    # fingerprint distinguishes them from plain strings.
    return _coerce_typed_sentinel(value)


def _walk_dict(value: dict, visited: set) -> dict:
    obj_id = id(value)
    if obj_id in visited:
        raise _StructuredWalkError("circular dict reference")
    visited.add(obj_id)
    try:
        out: Dict[str, Any] = {}
        for k, v in value.items():
            # JSON object keys must be strings; coerce numerics /
            # booleans / None to their JSON representations so the
            # final object is encodable.
            key = k if isinstance(k, str) else _coerce_dict_key(k)
            out[key] = _walk_for_shape(v, visited)
        return out
    finally:
        visited.discard(obj_id)


def _walk_sequence(value: Any, visited: set) -> list:
    obj_id = id(value)
    if obj_id in visited:
        raise _StructuredWalkError("circular sequence reference")
    visited.add(obj_id)
    try:
        return [_walk_for_shape(v, visited) for v in value]
    finally:
        visited.discard(obj_id)


def _coerce_dict_key(k: Any) -> str:
    """JSON requires string keys. Coerce numerics / booleans / None
    using json.dumps so the resulting string matches what
    json.dumps(allow_non_string_keys=False) would emit."""
    if k is None:
        return "null"
    if isinstance(k, bool):
        return "true" if k else "false"
    if isinstance(k, (int, float)):
        return str(k)
    return repr(k)


def _coerce_typed_sentinel(value: Any) -> Dict[str, Any]:
    """Produce the ``{"__type__": "...", "value": "..."}`` sentinel
    for a non-JSON-native value. The tag vocabulary is closed so the
    backend's structural fingerprint sees a stable shape regardless
    of which specific datetime/UUID/Decimal subclass was used."""
    import datetime as _dt
    import uuid as _uuid
    import decimal as _dec
    from pathlib import PurePath

    if isinstance(value, _dt.datetime):
        return {"__type__": "datetime", "value": value.isoformat()}
    if isinstance(value, _dt.date):
        return {"__type__": "date", "value": value.isoformat()}
    if isinstance(value, _dt.time):
        return {"__type__": "time", "value": value.isoformat()}
    if isinstance(value, _dt.timedelta):
        return {"__type__": "timedelta", "value": str(value)}
    if isinstance(value, _uuid.UUID):
        return {"__type__": "uuid", "value": str(value)}
    if isinstance(value, _dec.Decimal):
        return {"__type__": "decimal", "value": str(value)}
    if isinstance(value, (bytes, bytearray)):
        # Bytes are NOT JSON-encodable; tag and ascii-truncate so
        # the dashboard can show SOMETHING without spilling binary.
        try:
            preview = bytes(value)[:64].decode("ascii", errors="replace")
        except Exception:
            preview = ""
        return {"__type__": "bytes", "value": preview, "size": len(value)}
    if isinstance(value, PurePath):
        return {"__type__": "path", "value": str(value)}
    if isinstance(value, set):
        # set / frozenset aren't JSON-encodable; tag as a typed list
        # so the order-insensitive nature is preserved in the shape.
        return {
            "__type__": "set",
            "value": sorted(
                (_coerce_set_member(v) for v in value),
                key=lambda x: repr(x),
            ),
        }
    if isinstance(value, frozenset):
        return {
            "__type__": "frozenset",
            "value": sorted(
                (_coerce_set_member(v) for v in value),
                key=lambda x: repr(x),
            ),
        }
    # Pydantic / dataclass / custom class — tag with the class name
    # so structural drift between "returns User" and "returns
    # AdminUser" gets caught even when both happen to share field
    # names. Best-effort string preview via repr (truncated to keep
    # the sentinel small).
    cls_name = type(value).__name__
    try:
        preview = repr(value)
        if len(preview) > 200:
            preview = preview[:197] + "..."
    except Exception:
        preview = "<unrepresentable>"
    return {"__type__": "object", "class": cls_name, "value": preview}


def _coerce_set_member(v: Any) -> Any:
    """Set members must themselves be JSON-encodable for the sorted
    output to work. Primitives pass through; complex members reduce
    to their typed sentinel."""
    if v is None or isinstance(v, (str, int, float, bool)):
        return v
    return _coerce_typed_sentinel(v)


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
