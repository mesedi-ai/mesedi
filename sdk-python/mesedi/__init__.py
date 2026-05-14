"""
Mesedi SDK — Guardians for Autonomous AI.

Public API:

    mesedi.configure(api_key=..., base_url=...)
        Configure the module-level default client.

    @mesedi.wrap
        Decorator: records a function call as an agent execution.

    @mesedi.tool
        Decorator: records a tool_call event linked to the surrounding
        @wrap execution.

    mesedi.instrument_anthropic()
        Patch the Anthropic SDK to auto-emit llm_call events.

    mesedi.checkpoint(name, **metadata)
        Mark a notable point in agent execution.

    mesedi.validator_result(name, passed, message="", severity="error")
        Report a validator outcome.

    mesedi.flush(timeout=5.0)
        Block until the background shipper drains.

    mesedi.MesediClient, mesedi.Event, mesedi.Execution,
    mesedi.EventType, mesedi.Status — building blocks for advanced use.
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
from mesedi.observe import checkpoint, validator_result
from mesedi.tool import tool
from mesedi.wrap import wrap

__version__ = "0.0.5"

__all__ = [
    "MesediClient",
    "Event",
    "EventType",
    "Execution",
    "Status",
    "checkpoint",
    "configure",
    "flush",
    "get_client",
    "instrument_anthropic",
    "tool",
    "utcnow_rfc3339",
    "validator_result",
    "wrap",
    "__version__",
]
