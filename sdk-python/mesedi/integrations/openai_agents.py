"""OpenAI Agents SDK integration for Mesedi (Mesedi #28).

The OpenAI Agents SDK ships a clean ``RunHooks`` interface that
exposes per-agent / per-tool / per-handoff lifecycle callbacks. We
implement those hooks and translate them into Mesedi events, so
customers running on the OpenAI Agents SDK get topology,
agent_handoff, semantic_loop, and the other Mesedi detectors for
free without rewriting their agent code.

Usage:

    import mesedi
    from agents import Agent, Runner
    from mesedi.integrations.openai_agents import MesediRunHooks

    triage = Agent(name="triage", instructions="...", handoffs=[router])
    router = Agent(name="router",  instructions="...", handoffs=[planner, summarizer])
    planner = Agent(name="planner", instructions="...", tools=[search, fetch])
    summarizer = Agent(name="summarizer", instructions="...")

    @mesedi.wrap
    async def run_user_request(question: str) -> str:
        result = await Runner.run(
            triage,
            question,
            hooks=MesediRunHooks(),
        )
        return result.final_output

What the hooks emit:

  - ``checkpoint`` events at every agent start AND end. The agent
    name and a truncated input/output repr land on the checkpoint.
    Feeds the semantic_loop detector (#6) which hashes canonical
    state.
  - ``agent_handoff`` events on every handoff between agents. The
    from_agent and to_agent come straight from the hook arguments,
    handoff_kind defaults to ``"transfer"`` (the OAI Agents SDK
    handoff semantics: control transfers to the new agent).
    Feeds cascading_failure (#12) and coordination_deadlock (#13).
  - ``tool_call`` events for every tool invocation, mirroring
    ``@mesedi.tool`` wire format.

What the hooks do NOT emit yet (out of scope for v1):

  - ``llm_call`` events. The OAI Agents SDK dispatches model calls
    through its own runner; per-LLM-call hooks are not exposed in
    the public RunHooks surface as of the version this integration
    was written against. For Anthropic-backed runs, the existing
    ``mesedi.instrument_anthropic()`` patch covers the LLM-call
    surface; for OpenAI-backed runs, the same pattern is on the
    roadmap. The drift, identical-call, similar-call, and
    cost-velocity detectors that rely on llm_call events therefore
    do not fire on OAI-Agents-only deployments yet; the topology,
    handoff-based, and tool-based detectors all do.
  - Streaming hooks (``Runner.run_streamed``). Same shape; a v2
    iteration will add streaming-aware emission.
"""

from __future__ import annotations

import time
import uuid
from typing import Any, Dict, Optional

from mesedi._context import current_execution_context
from mesedi.client import get_client
from mesedi.events import Event, EventType, utcnow_rfc3339


# Lazy import. The OAI Agents SDK is an optional runtime dependency;
# importing this module does not require ``agents`` to be installed.
# Customers who actually use the hooks will have ``agents`` in their
# environment; tests stub the base class so the handler can be
# defined and exercised even when the package is missing.
_AGENTS_AVAILABLE = False
try:
    from agents import RunHooks  # type: ignore  # noqa: F401
    _AGENTS_AVAILABLE = True
except ImportError:
    # Provide a stub so the class can be defined and tested even
    # when the OAI Agents SDK is not installed. The customer's
    # code path would import ``agents`` itself, making
    # _AGENTS_AVAILABLE=True at module load.
    class RunHooks:  # type: ignore[no-redef]
        """Stub used when the OpenAI Agents SDK is not installed."""

        async def on_agent_start(self, context: Any, agent: Any) -> None:
            ...

        async def on_agent_end(self, context: Any, agent: Any, output: Any) -> None:
            ...

        async def on_handoff(
            self,
            context: Any,
            from_agent: Any,
            to_agent: Any,
        ) -> None:
            ...

        async def on_tool_start(self, context: Any, agent: Any, tool: Any) -> None:
            ...

        async def on_tool_end(
            self,
            context: Any,
            agent: Any,
            tool: Any,
            result: Any,
        ) -> None:
            ...


# Truncation budgets, kept consistent with the LangGraph integration
# and with the rest of the Mesedi observe layer.
_MAX_STATE_REPR = 1000
_MAX_TOOL_INPUT_REPR = 200
_MAX_TOOL_OUTPUT_REPR = 500


class MesediRunHooks(RunHooks):
    """OAI Agents SDK ``RunHooks`` implementation that emits Mesedi
    events.

    Pass an instance to ``Runner.run(..., hooks=MesediRunHooks())``.
    All emissions happen in the sync side of the SDK (the Mesedi
    shipper batches them asynchronously), so the OAI Agents
    SDK's async runner is never blocked.

    Outside a ``@mesedi.wrap`` execution, all emissions silently
    no-op (matching the rest of Mesedi's observe layer).
    """

    def __init__(self) -> None:
        super().__init__()
        # tool_call_id → start time (perf_counter seconds). Used to
        # pair on_tool_start with on_tool_end so we can compute
        # duration. Keyed by id(tool) which is reasonably stable
        # for the lifetime of one Runner.run() call.
        self._tool_starts: Dict[Any, float] = {}

    # ── Agent lifecycle ─────────────────────────────────────────────

    async def on_agent_start(self, context: Any, agent: Any) -> None:
        """Fire on every agent invocation. Emits a ``checkpoint``
        event whose payload captures the agent name and a truncated
        repr of the runner context. The semantic_loop detector (#6)
        hashes this state to catch agents that revisit the same
        logical state across multiple invocations.
        """
        agent_name = _extract_agent_name(agent)
        _emit_checkpoint(
            agent_name=agent_name,
            state=_extract_context_state(context),
            agent_event="enter",
        )

    async def on_agent_end(
        self,
        context: Any,
        agent: Any,
        output: Any,
    ) -> None:
        """Pair with on_agent_start. We emit a checkpoint at the
        agent EXIT too so detectors that look at state evolution
        see both the entry and the result.
        """
        agent_name = _extract_agent_name(agent)
        _emit_checkpoint(
            agent_name=agent_name,
            state=_summarize(output),
            agent_event="exit",
        )

    # ── Handoffs ────────────────────────────────────────────────────

    async def on_handoff(
        self,
        context: Any,
        from_agent: Any,
        to_agent: Any,
    ) -> None:
        """Fire on every handoff between agents. Emits an
        ``agent_handoff`` event so the topology graph (#10) renders
        the parent/child relationship and the cascading_failure
        (#12) + coordination_deadlock (#13) detectors fire on the
        resulting cross-agent patterns.

        Defaults handoff_kind to ``"transfer"`` because the OAI
        Agents SDK handoff semantics transfer control to the new
        agent (the parent does not stay active waiting for a return).
        Customers running a custom handoff pattern can subclass and
        override.
        """
        _emit_handoff(
            from_agent=_extract_agent_name(from_agent),
            to_agent=_extract_agent_name(to_agent),
            handoff_kind="transfer",
            task_summary=_summarize(_extract_context_state(context)),
        )

    # ── Tools ───────────────────────────────────────────────────────

    async def on_tool_start(
        self,
        context: Any,
        agent: Any,
        tool: Any,
    ) -> None:
        """Record the start time for pairing with on_tool_end."""
        self._tool_starts[id(tool)] = time.perf_counter()

    async def on_tool_end(
        self,
        context: Any,
        agent: Any,
        tool: Any,
        result: Any,
    ) -> None:
        """Emit a ``tool_call`` event with the paired duration.

        If on_tool_start was not observed (the hook missed the
        start, or the tool object id changed mid-flight) we still
        emit the event with duration_ms=0 rather than dropping it,
        because the tool_failures detector benefits from knowing
        the call happened at all.
        """
        started = self._tool_starts.pop(id(tool), None)
        duration_ms = 0
        if started is not None:
            duration_ms = int((time.perf_counter() - started) * 1000)
        _emit_tool_call(
            tool_name=_extract_tool_name(tool),
            arguments=_summarize_tool_input(tool),
            return_value=_summarize_tool_output(result),
            duration_ms=duration_ms,
            status="ok",
        )


# ── Helpers ─────────────────────────────────────────────────────────


def _extract_agent_name(agent: Any) -> str:
    """OAI Agents Agent objects expose .name as the canonical
    identifier. Fall back to class name when the attribute is
    missing (e.g. a custom Agent subclass).
    """
    if agent is None:
        return "unknown"
    name = getattr(agent, "name", None)
    if isinstance(name, str) and name:
        return name
    return agent.__class__.__name__


def _extract_tool_name(tool: Any) -> str:
    """OAI Agents tool objects expose .name; some custom tool
    types use .__name__ (function tools wrapped via the decorator).
    """
    if tool is None:
        return "unknown"
    for attr in ("name", "__name__"):
        v = getattr(tool, attr, None)
        if isinstance(v, str) and v:
            return v
    return tool.__class__.__name__


def _extract_context_state(context: Any) -> Any:
    """Pull a stable, serializable view of the runner context.

    OAI Agents SDK wraps the customer's context type in a
    ``RunContextWrapper``; the customer's real context object is
    typically on ``.context``. We grab that and fall back to the
    wrapper itself if the attribute is missing.
    """
    if context is None:
        return None
    inner = getattr(context, "context", None)
    if inner is not None:
        return inner
    return context


def _summarize(obj: Any) -> str:
    """Repr-and-truncate. Used for checkpoint state and tool args/
    return values. Stays inside Mesedi's payload size budget
    regardless of what the customer's agent passes around.
    """
    if obj is None:
        return ""
    try:
        r = repr(obj)
    except Exception:
        r = "<unrepr>"
    if len(r) > _MAX_STATE_REPR:
        r = r[: _MAX_STATE_REPR - 3] + "..."
    return r


def _summarize_tool_input(tool: Any) -> str:
    """Best-effort extraction of the tool input arguments. OAI
    Agents tool wrappers attach the parsed arguments to the tool
    instance in some variants; in others they are not surfaced to
    the hooks at all. Falls back to the tool name when there's
    nothing to show.
    """
    for attr in ("arguments", "input", "params"):
        v = getattr(tool, attr, None)
        if v is not None:
            return _summarize(v)
    return _summarize(_extract_tool_name(tool))


def _summarize_tool_output(result: Any) -> str:
    """Best-effort extraction of the tool return value."""
    if result is None:
        return ""
    output = getattr(result, "output", None)
    if output is not None:
        return _summarize(output)
    return _summarize(result)


def _emit_checkpoint(
    agent_name: str,
    state: Any,
    agent_event: str,
) -> None:
    """Emit a Mesedi ``checkpoint`` event with the agent name and a
    truncated state repr. Outside ``@wrap``: no-op.
    """
    ctx = current_execution_context()
    if ctx is None:
        return
    ctx.check_budget()
    if ctx.budget_tracker is not None:
        ctx.budget_tracker.increment_steps()

    state_repr = _summarize(state)
    payload: Dict[str, Any] = {
        "name": f"openai_agents.{agent_event}.{agent_name}",
        "agent": agent_name,
        "agent_event": agent_event,
        "state_repr": state_repr,
    }
    client = get_client()
    client.submit_event(Event(
        event_id=f"evt-{uuid.uuid4().hex[:12]}",
        execution_id=ctx.execution_id,
        event_type=EventType.CHECKPOINT,
        sequence=ctx.next_sequence(),
        timestamp=utcnow_rfc3339(),
        payload=payload,
    ))


def _emit_handoff(
    from_agent: str,
    to_agent: str,
    handoff_kind: str,
    task_summary: str,
) -> None:
    """Emit an ``agent_handoff`` event matching the wire format of
    ``mesedi.emit_agent_handoff`` so the topology + cascading_failure
    + coordination_deadlock detectors see it the same way.
    """
    ctx = current_execution_context()
    if ctx is None:
        return
    ctx.check_budget()
    if ctx.budget_tracker is not None:
        ctx.budget_tracker.increment_steps()

    payload: Dict[str, Any] = {
        "from_agent": from_agent,
        "to_agent": to_agent,
        "handoff_kind": handoff_kind,
        "task_summary": task_summary,
    }
    client = get_client()
    client.submit_event(Event(
        event_id=f"evt-{uuid.uuid4().hex[:12]}",
        execution_id=ctx.execution_id,
        event_type=EventType.AGENT_HANDOFF,
        sequence=ctx.next_sequence(),
        timestamp=utcnow_rfc3339(),
        payload=payload,
    ))


def _emit_tool_call(
    tool_name: str,
    arguments: str,
    return_value: str,
    duration_ms: int,
    status: str,
) -> None:
    """Emit a Mesedi ``tool_call`` event matching the wire format
    of the LangGraph adapter and ``@mesedi.tool``.
    """
    ctx = current_execution_context()
    if ctx is None:
        return
    ctx.check_budget()
    if ctx.budget_tracker is not None:
        ctx.budget_tracker.increment_steps()

    payload: Dict[str, Any] = {
        "tool_name": tool_name,
        "arguments": arguments[:_MAX_TOOL_INPUT_REPR],
        "return_value": return_value[:_MAX_TOOL_OUTPUT_REPR],
        "latency_ms": int(duration_ms),
        "status": status,
    }
    client = get_client()
    client.submit_event(Event(
        event_id=f"evt-{uuid.uuid4().hex[:12]}",
        execution_id=ctx.execution_id,
        event_type=EventType.TOOL_CALL,
        sequence=ctx.next_sequence(),
        timestamp=utcnow_rfc3339(),
        duration_ms=int(duration_ms),
        payload=payload,
    ))


__all__ = ["MesediRunHooks"]
