"""Effect declarations and per-call effects, for the backend's
effect_aliasing check: two paths reach the same side effect and only
one is watched.

The class and its one-line spec come from a thread in the LangChain
community i-made-this channel and from Alessandro Prandini's writeup
"Four ways an agent walked past my command hook" (digline.dev).

Declare-then-verify on destinations. A run states where it may make
changes, optionally with the tools allowed to make each one
(``declare_effects``). Tool and MCP calls report what they changed
(``record_effects`` inside a ``@mesedi.tool`` function, or
``effects=`` on ``emit_mcp_call``). The backend flags a change to a
place not declared, or by a tool not listed for it. A run with no
declaration is not checked.

Destinations are your own names for the things changed: a repository,
a table, a bucket path. Name the underlying resource, not the route,
because two names for one place look like two places.
"""

from __future__ import annotations

import contextvars
import uuid
from typing import Any, Dict, Iterable, List, Optional, Union

from mesedi._context import current_execution_context
from mesedi.client import get_client
from mesedi.events import Event, EventType, utcnow_rfc3339

# The list a @mesedi.tool call collects record_effects() into; None
# outside a tool call.
_current_effects: contextvars.ContextVar[Optional[List[str]]] = contextvars.ContextVar(
    "mesedi_current_effects", default=None
)


def record_effects(*destinations: str) -> None:
    """Report the places the current ``@mesedi.tool`` call changed.

    Call it from inside the tool function, once or several times::

        @mesedi.tool
        def push_branch(repo: str) -> str:
            ...
            mesedi.record_effects(f"github.com/{repo}")

    The destinations ride on the tool's own ``tool_call`` event as
    ``effects``. Outside a ``@mesedi.tool`` call: no-op.
    """
    bucket = _current_effects.get()
    if bucket is None:
        return
    for d in destinations:
        if isinstance(d, str) and d.strip():
            bucket.append(d.strip())


def declare_effects(
    destinations: Iterable[Union[str, Dict[str, Any]]],
    declared_by: str = "operator",
    note: str = "",
) -> None:
    """Emit an ``effect_declaration`` event: where this run may make
    changes.

    Each destination is a string ("any tool may change it") or a dict
    ``{"destination": "...", "via": ["tool_a", "server/method"]}``
    naming the only tools allowed to change it; MCP tools are named
    "server/method". A destination ending in ``*`` covers everything
    that starts with the text before it. Declarations in one run add
    up. Emit near the start of the run.

    Outside @wrap: no-op.
    """
    ctx = current_execution_context()
    if ctx is None:
        return
    out: List[Dict[str, Any]] = []
    for d in destinations:
        if isinstance(d, str):
            if d.strip():
                out.append({"destination": d.strip()})
            continue
        dest = str(d.get("destination", "")).strip()
        if not dest:
            continue
        entry: Dict[str, Any] = {"destination": dest}
        via = [str(v).strip() for v in d.get("via") or [] if str(v).strip()]
        if via:
            entry["via"] = via
        out.append(entry)

    payload: Dict[str, Any] = {"destinations": out}
    if declared_by:
        payload["declared_by"] = declared_by
    if note:
        payload["note"] = note

    get_client().submit_event(Event(
        event_id=f"evt-{uuid.uuid4().hex[:12]}",
        execution_id=ctx.execution_id,
        event_type=EventType.EFFECT_DECLARATION,
        sequence=ctx.next_sequence(),
        timestamp=utcnow_rfc3339(),
        payload=payload,
    ))


def clean_effects(effects: Optional[Iterable[str]]) -> List[str]:
    """Trimmed, non-empty destination strings, order kept."""
    if not effects:
        return []
    return [e.strip() for e in effects if isinstance(e, str) and e.strip()]
