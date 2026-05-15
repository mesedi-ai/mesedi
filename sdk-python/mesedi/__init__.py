"""
Mesedi SDK — Guardians for Autonomous AI.

Public API:

    mesedi.configure(api_key=..., base_url=...)
        Configure the module-level default client.

    @mesedi.wrap                              # bare form
    @mesedi.wrap(budget=Budget(...))          # with hard-halt budget
        Decorator: records a function call as an agent execution.
        Optional `budget` enforces wall-clock / step / token limits
        at safe boundaries; on exceedance, raises MesediHalt which
        @wrap catches and converts to status=halted.

    @mesedi.tool
        Decorator: records a tool_call event linked to the
        surrounding @mesedi.wrap execution.

    mesedi.instrument_anthropic()
        Patch the Anthropic SDK to auto-emit llm_call events.

    mesedi.checkpoint(name, **metadata)
        Mark a notable point in agent execution.

    mesedi.validator_result(name, passed, message="", severity="error")
        Report a validator outcome.

    mesedi.Budget(max_wall_clock_seconds=..., max_steps=...,
                  max_tokens_in=..., max_tokens_out=...)
        Hard-halt policy. Pass to @wrap to enforce local budgets.

    mesedi.MesediHalt
        Exception class raised when a budget is exceeded. Inherits
        BaseException (not Exception) so broad `except Exception:`
        handlers don't swallow it. Normally caught by @wrap itself
        — user code rarely needs to see it.

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
from mesedi.halt import Budget, MesediHalt
from mesedi.observe import checkpoint, emit_llm_call, validator_result
from mesedi.tool import tool
from mesedi.wrap import wrap

__version__ = "0.0.8"

__all__ = [
    "Budget",
    "MesediClient",
    "MesediHalt",
    "Event",
    "EventType",
    "Execution",
    "Status",
    "checkpoint",
    "configure",
    "emit_llm_call",
    "flush",
    "get_client",
    "instrument_anthropic",
    "tool",
    "utcnow_rfc3339",
    "validator_result",
    "wrap",
    "__version__",
]
