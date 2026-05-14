"""
Mesedi SDK — Guardians for Autonomous AI.

Public API:

    mesedi.configure(api_key=..., base_url=...)
        Configure the module-level default client. Reads MESEDI_API_KEY
        and MESEDI_BASE_URL from the environment as fallbacks.

    @mesedi.wrap
        Decorator that records a function call as an agent execution.
        Submits start/complete/crash to the async shipper thread; pushes
        an execution context that @tool and instrumented LLM calls read.

    @mesedi.tool
        Decorator that records a function call as a tool_call event,
        attached to the surrounding @mesedi.wrap execution.

    mesedi.instrument_anthropic()
        Patch the Anthropic SDK's Messages.create to emit llm_call
        events automatically. Opt-in; call once at process startup.

    mesedi.flush(timeout=5.0)
        Block until the background shipper has drained all events.

    mesedi.MesediClient
        Explicit client for advanced usage.

    mesedi.Event, mesedi.Execution
        Dataclasses mirroring the backend's wire format.

    mesedi.EventType, mesedi.Status
        Enums for event_type and execution status.
"""

from mesedi.anthropic_integration import instrument_anthropic
from mesedi.client import MesediClient, configure, flush, get_client
from mesedi.events import (
    Event,
    EventType,
    Execution,
    Status,
    utcnow_rfc3339,
)
from mesedi.tool import tool
from mesedi.wrap import wrap

__version__ = "0.0.4"

__all__ = [
    "MesediClient",
    "Event",
    "EventType",
    "Execution",
    "Status",
    "configure",
    "flush",
    "get_client",
    "instrument_anthropic",
    "tool",
    "utcnow_rfc3339",
    "wrap",
    "__version__",
]
