"""Egress and environment-declaration emitters.

Added 2026-09-14 when the egress-visibility deferral was reopened.
Their own module from birth: observe.py sits at the size the audit
caps, and mcp.py set the precedent that emit helpers live in topical
modules.
"""

from __future__ import annotations

import uuid
from typing import Any, Dict

from mesedi._context import current_execution_context
from mesedi.client import get_client
from mesedi.events import Event, EventType, utcnow_rfc3339


def _normalize_egress_destination(destination: str) -> str:
    """Reduce a destination to host or host:port. A full URL loses its
    scheme, path, query, fragment and any user:password@ part here on
    purpose: identical services must cluster, and query strings and
    userinfo are where secrets ride. A port survives only if it is all
    digits, so a malformed URL cannot smuggle part of a password out as
    a "port". Cases: spec/egress_destination_vectors.json (shared with
    the TypeScript SDK)."""
    d = destination.strip()
    if "://" in d:
        d = d.split("://", 1)[1]
    for cut in ("/", "?", "#"):
        if cut in d:
            d = d.split(cut, 1)[0]
    # Everything up to the last @ is userinfo (user, or user:password).
    # Until 2026-11-15 it was kept and sent (outside review finding D13).
    if "@" in d:
        d = d.rsplit("@", 1)[1]
    return _host_and_numeric_port(d)


def _host_and_numeric_port(d: str) -> str:
    """Keep host, plus :port only when the port is all ASCII digits.
    Bracketed IPv6 hosts keep their brackets."""
    if d.startswith("["):
        end = d.find("]")
        if end == -1:
            return d
        host, rest = d[: end + 1], d[end + 1 :]
        port = rest[1:] if rest.startswith(":") else ""
        return host + rest if port.isascii() and port.isdigit() else host
    if ":" in d:
        host, port = d.split(":", 1)
        return d if port.isascii() and port.isdigit() else host
    return d


def emit_egress(
    destination: str,
    protocol: str = "",
    tool_name: str = "",
    bytes_out: int = 0,
    note: str = "",
) -> None:
    """Emit an ``egress`` event recording one outbound network contact
    by the agent's environment.

    Report, never infer: call this from the host application or
    sandbox that actually opened the connection. Mesedi cannot see a
    connection nobody reports. The destination is normalized to host
    or host:port; any scheme, path, query string, fragment and
    user:password@ part is dropped so identical services cluster and
    secrets in URLs never leave the process. Versions before 0.8.0
    kept the user:password@ part.

    The backend's covert_coordination detector counts distinct runs
    converging on one destination, and environment_misapprehension
    fires when a declared-simulation run reaches live destinations.

    Outside @wrap: no-op.
    """
    ctx = current_execution_context()
    if ctx is None:
        return

    ctx.check_budget()
    if ctx.budget_tracker is not None:
        ctx.budget_tracker.increment_steps()

    payload: Dict[str, Any] = {
        "destination": _normalize_egress_destination(destination),
    }
    if protocol:
        payload["protocol"] = protocol
    if tool_name:
        payload["tool_name"] = tool_name
    if bytes_out:
        payload["bytes_out"] = int(bytes_out)
    if note:
        payload["note"] = note

    client = get_client()
    client.submit_event(Event(
        event_id=f"evt-{uuid.uuid4().hex[:12]}",
        execution_id=ctx.execution_id,
        event_type=EventType.EGRESS,
        sequence=ctx.next_sequence(),
        timestamp=utcnow_rfc3339(),
        payload=payload,
    ))


def emit_environment_declaration(
    mode: str,
    declared_by: str = "operator",
    note: str = "",
) -> None:
    """Emit an ``environment_declaration`` event stating what
    environment this run is SUPPOSED to be in.

    Well-known modes are ``"live"``, ``"simulation"`` and
    ``"staging"``; any other string is stored verbatim and treated as
    not-live. Emit once, near the start of the run. The backend's
    environment_misapprehension detector compares later observations
    against this claim; it checks the declared boundary, never the
    model's beliefs.

    Outside @wrap: no-op.
    """
    ctx = current_execution_context()
    if ctx is None:
        return

    payload: Dict[str, Any] = {"mode": mode.strip().lower()}
    if declared_by:
        payload["declared_by"] = declared_by
    if note:
        payload["note"] = note

    client = get_client()
    client.submit_event(Event(
        event_id=f"evt-{uuid.uuid4().hex[:12]}",
        execution_id=ctx.execution_id,
        event_type=EventType.ENVIRONMENT_DECLARATION,
        sequence=ctx.next_sequence(),
        timestamp=utcnow_rfc3339(),
        payload=payload,
    ))
