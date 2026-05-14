"""
Mesedi SDK — Guardians for Autonomous AI.

Public API:

    mesedi.configure(api_key=..., base_url=...)
        Configure the module-level default client. Reads MESEDI_API_KEY
        and MESEDI_BASE_URL from the environment as fallbacks.

    @mesedi.wrap
        Decorator that records a function call as an agent execution.
        Submits POST /executions on entry and PATCH /executions/{id} on
        exit (completed or crashed) via the async shipper thread.
        Re-raises any caught exception transparently.

    mesedi.flush(timeout=5.0)
        Block until the background shipper has drained all events
        submitted so far. Useful at end of scripts or in tests.

    mesedi.MesediClient
        Explicit client for advanced usage (e.g., multiple projects,
        custom batching, manual sync HTTP calls). Most callers should
        use mesedi.configure() + @mesedi.wrap instead.

    mesedi.Event, mesedi.Execution
        Dataclasses mirroring the backend's wire format. Useful when
        emitting events manually rather than via decorators.

    mesedi.EventType, mesedi.Status
        Enums for event_type and execution status. Match the Go
        constants in backend/internal/events/types.go exactly.
"""

from mesedi.client import MesediClient, configure, flush, get_client
from mesedi.events import (
    Event,
    EventType,
    Execution,
    Status,
    utcnow_rfc3339,
)
from mesedi.wrap import wrap

__version__ = "0.0.2"

__all__ = [
    "MesediClient",
    "Event",
    "EventType",
    "Execution",
    "Status",
    "configure",
    "flush",
    "get_client",
    "utcnow_rfc3339",
    "wrap",
    "__version__",
]
