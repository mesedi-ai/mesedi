"""CrewAI integration — auto-instrument a Crew with Mesedi telemetry.

Usage:

    import mesedi
    from mesedi.integrations.crewai import instrument_crew

    @mesedi.wrap
    def run_my_crew(question: str) -> str:
        crew = build_crew()       # however you construct yours
        instrument_crew(crew)     # one-liner — see below for what this does
        result = crew.kickoff(inputs={"question": question})
        return str(result)

Design:

CrewAI builds on LangChain for the LLM layer — every Agent owns a
LangChain ChatModel as its ``llm`` attribute. That means the
LangChain ``MesediCallbackHandler`` we already ship gets us full
LLM-call and tool-call visibility "for free" if we just attach it to
each agent's LLM. ``instrument_crew`` does exactly that.

On top of LLM-level telemetry, CrewAI exposes two crew-level
callbacks that surface CrewAI's own semantics:

  step_callback(step)    — fires for each ReAct-style agent step
                           (AgentAction when the agent chooses a
                           tool, AgentFinish when it stops). Emits
                           ``checkpoint`` events tagged
                           ``crewai.agent_action`` /
                           ``crewai.agent_finish`` so the dashboard
                           timeline shows the agent's reasoning
                           rhythm in addition to the underlying LLM
                           calls.
  task_callback(output)  — fires when a Task completes. Emits a
                           ``crewai.task_completed`` checkpoint.

These checkpoint events complement, not duplicate, the LangChain
handler's LLM/tool telemetry: detectors continue to read from the
llm_call / tool_call event types, while the dashboard timeline
gains structural markers showing where each CrewAI task and agent
step happened.

``instrument_crew`` is idempotent: calling it twice on the same
crew won't double-register the handler or callbacks. Safe to call
each kickoff if your Crew is recreated per request.

Out of scope for this slice:
  - Multiple CrewAI versions that have renamed agents → agents_list
    or step_callback → on_step. We try the canonical attribute names
    used in CrewAI 0.30+; if a future major version moves them, the
    instrumentation silently no-ops on those attributes (rather than
    raising) and the surface still works at the LangChain layer.
  - Hierarchical / manager-agent Crews — the manager's LLM is also
    attached because it shows up in ``crew.agents``. If a future
    version stores the manager separately, we'd need to walk
    additional attributes; we'll add that when a customer reports it.
"""

from __future__ import annotations

from typing import Any, Optional

from mesedi.integrations.langchain import MesediCallbackHandler
from mesedi.observe import checkpoint


def instrument_crew(
    crew: Any,
    handler: Optional[MesediCallbackHandler] = None,
) -> MesediCallbackHandler:
    """Attach Mesedi telemetry to a CrewAI ``Crew`` instance.

    Performs three operations, each idempotent:

    1. Constructs (or accepts) a ``MesediCallbackHandler`` and appends
       it to each agent's LLM's ``callbacks`` list. This captures every
       LLM call and every tool invocation that flows through that LLM,
       matching the exact wire format the LangChain adapter produces
       elsewhere.
    2. Sets the crew's ``step_callback`` to a Mesedi step-callback that
       emits a ``checkpoint`` event per agent step. Only set if the
       crew didn't already have a step_callback (we never overwrite
       customer-supplied callbacks).
    3. Sets the crew's ``task_callback`` to a Mesedi task-callback that
       emits a ``checkpoint`` event per completed task. Same
       non-overwrite policy.

    Returns the handler so the caller can attach it elsewhere if
    needed (e.g. to a separate ChatModel not owned by an agent).

    Outside a ``@mesedi.wrap`` execution, the emitted events silently
    no-op — same fail-open pattern as every other Mesedi primitive.
    """
    if handler is None:
        handler = MesediCallbackHandler()

    # 1. Attach the LangChain handler to each agent's LLM.
    agents = getattr(crew, "agents", None) or []
    for agent in agents:
        _attach_handler_to_llm(agent, handler)

    # 2. Set step_callback if not already set.
    if not getattr(crew, "step_callback", None):
        try:
            setattr(crew, "step_callback", mesedi_step_callback)
        except Exception:
            # Some CrewAI versions use Pydantic models that reject
            # attribute assignment after instantiation; degrade
            # gracefully — LLM-level telemetry still works.
            pass

    # 3. Set task_callback if not already set.
    if not getattr(crew, "task_callback", None):
        try:
            setattr(crew, "task_callback", mesedi_task_callback)
        except Exception:
            pass

    return handler


def _attach_handler_to_llm(agent: Any, handler: MesediCallbackHandler) -> None:
    """Append ``handler`` to ``agent.llm.callbacks``.

    Handles three callback-storage patterns seen across LangChain
    versions:
      - ``llm.callbacks`` is None / missing → assign a fresh list
      - ``llm.callbacks`` is a list → append (if not already there)
      - ``llm.callbacks`` is a CallbackManager (an object with
        ``.add_handler`` or ``.handlers``) → register via that
        interface

    Idempotent: a handler already attached is not re-attached.
    """
    llm = getattr(agent, "llm", None)
    if llm is None:
        return

    existing = getattr(llm, "callbacks", None)

    # Pattern 3: BaseCallbackManager-like object.
    if existing is not None and hasattr(existing, "handlers"):
        handlers_list = getattr(existing, "handlers", None)
        if isinstance(handlers_list, list) and handler not in handlers_list:
            if hasattr(existing, "add_handler"):
                try:
                    existing.add_handler(handler)
                    return
                except Exception:
                    pass
            try:
                handlers_list.append(handler)
            except Exception:
                pass
        return

    # Pattern 2: plain list.
    if isinstance(existing, list):
        if handler not in existing:
            try:
                existing.append(handler)
            except Exception:
                pass
        return

    # Pattern 1: nothing there — assign a new list.
    try:
        setattr(llm, "callbacks", [handler])
    except Exception:
        # Frozen Pydantic model or similar — give up silently. LLM
        # observability via this Agent will be missing but the
        # CrewAI-level step/task callbacks still work.
        pass


def mesedi_step_callback(step: Any) -> None:
    """CrewAI step_callback that emits a ``checkpoint`` event per step.

    The ``step`` argument is either an ``AgentAction`` (the agent
    chose a tool to invoke) or an ``AgentFinish`` (the agent has
    decided it's done). Both classes are duck-typed below — we don't
    import them from CrewAI so this module is import-safe in
    environments where crewai is not installed (e.g. test runs).

    The emitted checkpoint is a structural marker only. LLM-call and
    tool-call telemetry come from the LangChain handler that
    ``instrument_crew`` also attached. Detectors continue to consume
    those event types; this checkpoint surfaces CrewAI's reasoning
    rhythm in the dashboard timeline.

    Outside ``@mesedi.wrap``: silently no-ops (matches the contract
    of every other Mesedi primitive).
    """
    # Distinguish AgentAction from AgentFinish by attribute shape.
    # AgentAction has tool + tool_input. AgentFinish has return_values.
    if hasattr(step, "tool") and hasattr(step, "tool_input"):
        tool_name = _safe_str(getattr(step, "tool", "")) or "unknown"
        tool_input = _safe_str(getattr(step, "tool_input", ""))[:200]
        checkpoint(
            "crewai.agent_action",
            tool=tool_name,
            tool_input=tool_input,
        )
    elif hasattr(step, "return_values") or hasattr(step, "output"):
        # AgentFinish — final answer. We don't try to capture
        # return_values content here because it can be large; the
        # underlying LangChain handler's llm_call event already
        # records the response text.
        checkpoint("crewai.agent_finish")
    else:
        # Unknown step shape — record a generic checkpoint so the
        # timeline doesn't have an unexplained gap.
        checkpoint("crewai.agent_step", step_type=type(step).__name__)


def mesedi_task_callback(task_output: Any) -> None:
    """CrewAI task_callback that emits a ``checkpoint`` per task completion.

    The ``task_output`` argument is a ``TaskOutput`` (or similar shape
    across CrewAI versions). We extract a short identifier for the
    task and the agent so the dashboard timeline shows which task
    just finished.

    Outside ``@mesedi.wrap``: silently no-ops.
    """
    description = (
        _safe_str(getattr(task_output, "description", ""))
        or _safe_str(getattr(task_output, "name", ""))
    )
    agent = (
        _safe_str(getattr(task_output, "agent", ""))
        or _safe_str(getattr(task_output, "agent_name", ""))
    )
    metadata: dict = {}
    if description:
        metadata["task"] = description[:200]
    if agent:
        metadata["agent"] = agent[:100]
    checkpoint("crewai.task_completed", **metadata)


def _safe_str(value: Any) -> str:
    """Stringify ``value`` defensively. Avoids raising on
    objects whose ``__str__`` is broken (rare but seen in wild
    pydantic-based libraries)."""
    if value is None:
        return ""
    if isinstance(value, str):
        return value
    try:
        return str(value)
    except Exception:
        return ""
