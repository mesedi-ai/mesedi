"""Model Context Protocol call observation.

Moved verbatim out of observe.py on 2026-09-10, paying down that
file's size ratchet while its TypeScript twin (src/mcp.ts) made the
mirror move; both SDKs now keep MCP observation in a module of its
own. Re-exported from observe and from the package root, so every
existing import keeps working.
"""

from __future__ import annotations

import uuid
from typing import Any, Dict

from mesedi._context import current_execution_context
from mesedi._jcs import input_schema_hash
from mesedi.client import get_client
from mesedi.events import Event, EventType, utcnow_rfc3339

__all__ = ["emit_mcp_call"]


def emit_mcp_call(
    server_name: str,
    method: str,
    server_url: str = "",
    arguments: Any = None,
    return_value: Any = None,
    latency_ms: int = 0,
    error: str = "",
    error_class: str = "",
    input_schema: Any = None,
) -> None:
    """Emit an ``mcp_call`` event for one Model Context Protocol
    server invocation.

    Use this when your agent talks to an MCP server (Anthropic's
    filesystem / github servers, a customer-hosted MCP server, etc.).
    The dashboard renders MCP calls in a distinct chip so cost
    attribution can break down by server identity, and the existing
    tool_failures detector picks up failed MCP calls when ``error``
    or ``error_class`` is non-empty.

    Typical caller pattern::

        start = time.perf_counter()
        try:
            result = mcp_client.invoke(server, method, args)
            mesedi.emit_mcp_call(
                server_name=server,
                method=method,
                arguments=args,
                return_value=result,
                latency_ms=int((time.perf_counter() - start) * 1000),
            )
            return result
        except mcp.MCPError as exc:
            mesedi.emit_mcp_call(
                server_name=server,
                method=method,
                arguments=args,
                error=str(exc),
                error_class="hard_error",
                latency_ms=int((time.perf_counter() - start) * 1000),
            )
            raise

    Outside @wrap: no-op. Halt-safe (budget check runs first).

    Args:
        server_name: Stable identifier for the MCP server
            ("filesystem", "github", "crm-mcp"). Used as the primary
            grouping dimension on the dashboard.
        method: The MCP method invoked ("read_file", "list_resources",
            etc.). Combined with server_name to form a per-method
            cluster signature on failure.
        server_url: Optional. The MCP server's URL or stdio target,
            captured for the expanded payload view. Useful when
            multiple instances of the same server name run with
            different configs.
        arguments: The method arguments (any JSON-serializable value).
        return_value: The successful return value. Omit on error.
        latency_ms: Wall-clock duration in milliseconds.
        error: Error message when the call failed.
        error_class: Classifier for the failure ("hard_error",
            "soft_error", "timeout", "server_unreachable",
            "method_not_found").
        input_schema: Optional. The method's DECLARED input schema as
            the MCP server advertises it (the inputSchema member of
            the tool definition). Only a canonical SHA-256 of it is
            sent, feeding the definition-drift detector; the schema
            itself never leaves the process. Pass it fresh from the
            server's current tool listing on each call rather than a
            copy cached at startup, a poisoned server swaps the
            definition between calls and a cached copy hides exactly
            that swap.
    """
    ctx = current_execution_context()
    if ctx is None:
        return

    ctx.check_budget()
    if ctx.budget_tracker is not None:
        ctx.budget_tracker.increment_steps()

    payload: Dict[str, Any] = {
        "server_name": server_name,
        "method": method,
    }
    if server_url:
        payload["server_url"] = server_url
    if arguments is not None:
        payload["arguments"] = arguments
    if return_value is not None:
        payload["return_value"] = return_value
    if latency_ms:
        payload["latency_ms"] = int(latency_ms)
    if error:
        payload["error"] = error
    if error_class:
        payload["error_class"] = error_class
    if schema_hash := input_schema_hash(input_schema):
        payload["input_schema_hash"] = schema_hash

    client = get_client()
    client.submit_event(Event(
        event_id=f"evt-{uuid.uuid4().hex[:12]}",
        execution_id=ctx.execution_id,
        event_type=EventType.MCP_CALL,
        sequence=ctx.next_sequence(),
        timestamp=utcnow_rfc3339(),
        duration_ms=latency_ms,
        payload=payload,
    ))

