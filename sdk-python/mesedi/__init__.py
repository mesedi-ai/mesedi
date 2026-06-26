"""
Mesedi SDK, Guardians for Autonomous AI.

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

    mesedi.instrument_openai()
        Patch the OpenAI SDK (chat.completions + Responses API) to
        auto-emit llm_call events.

    mesedi.instrument_cohere()
        Patch the Cohere SDK (Client + ClientV2 .chat) to auto-emit
        llm_call events.

    mesedi.instrument_gemini()
        Patch the google-generativeai SDK
        (GenerativeModel.generate_content) to auto-emit llm_call
        events.

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
        handlers don't swallow it. Normally caught by @wrap itself,
        so user code rarely needs to see it.

    mesedi.flush(timeout=5.0)
        Block until the background shipper drains.

    mesedi.MesediClient, mesedi.Event, mesedi.Execution,
    mesedi.EventType, mesedi.Status, building blocks for advanced use.
"""

from mesedi.anthropic_integration import instrument_anthropic
from mesedi.openai_integration import instrument_openai
from mesedi.cohere_integration import instrument_cohere
from mesedi.gemini_integration import instrument_gemini
from mesedi.ollama_integration import instrument_ollama
from mesedi.vertex_gemini_integration import instrument_vertex_gemini
from mesedi.client import MesediClient, configure, flush, get_client
from mesedi.events import (
    Event,
    EventType,
    Execution,
    Status,
    utcnow_rfc3339,
)
from mesedi.halt import Budget, MesediHalt
from mesedi.observe import (
    HumanInterventionHandle,
    checkpoint,
    emit_agent_handoff,
    emit_eval_score,
    emit_infrastructure_event,
    emit_llm_call,
    emit_mcp_call,
    emit_memory_operation,
    pause_for_human,
    request_human_intervention,
    resume_for_agent,
    validator_result,
)
from mesedi.tool import tool
from mesedi.wrap import wrap

__version__ = "0.5.0"

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
    "emit_agent_handoff",
    "emit_eval_score",
    "emit_infrastructure_event",
    "emit_llm_call",
    "emit_mcp_call",
    "emit_memory_operation",
    "flush",
    "get_client",
    "HumanInterventionHandle",
    "instrument_anthropic",
    "instrument_cohere",
    "instrument_gemini",
    "instrument_ollama",
    "instrument_openai",
    "instrument_vertex_gemini",
    "pause_for_human",
    "request_human_intervention",
    "resume_for_agent",
    "tool",
    "utcnow_rfc3339",
    "validator_result",
    "wrap",
    "__version__",
]
