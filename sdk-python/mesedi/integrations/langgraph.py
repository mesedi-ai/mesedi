"""LangGraph integration for Mesedi.

Usage (one-liner):

    import mesedi
    from mesedi.integrations.langgraph import instrument_langgraph
    from langgraph.graph import StateGraph

    graph = build_my_graph()              # returns a CompiledStateGraph
    graph = instrument_langgraph(graph)   # adds Mesedi telemetry

    @mesedi.wrap
    def run_agent(question: str) -> str:
        result = graph.invoke({"question": question})
        return result["answer"]

What the integration emits:

  - ``llm_call`` events for every LLM invocation inside the graph
  - ``tool_call`` events for every tool invocation
  - ``checkpoint`` events at every node entry. The node name lands
    on the checkpoint, and the canonical state is hashed so the
    semantic_loop detector can catch agents that revisit the
    same logical state across nodes.
  - ``agent_handoff`` events when the graph invokes a compiled
    sub-graph. The from_agent/to_agent labels are the parent and
    child node names; the handoff_kind is "delegate" (synchronous)
    or "spawn" (Send-style fan-out). Feeds cascading_failure
    and coordination_deadlock when the sub-graph runs as a
    nested @wrap.

Why this is a thin layer over the LangChain handler:

LangGraph builds on langchain-core. The callback interface
(``BaseCallbackHandler`` from ``langchain_core.callbacks``) is the
same one LangChain uses, and the LLM/tool dispatch goes through
the same dispatcher. We inherit from ``MesediCallbackHandler``
(``mesedi.integrations.langchain``) and add the LangGraph-specific
node-and-handoff translation on top.

Out of scope for v1:

  - Async streaming hooks (``astream``, ``astream_events``). The
    integration covers ``invoke`` and ``ainvoke`` for now; streaming
    parity is a v2 concern.
  - LangGraph's persistence layer (``Checkpointer``). Mesedi
    doesn't read or replace the persisted state; we emit telemetry
    parallel to it.
  - LangGraph ``interrupt()``. The HITL lifecycle helpers
    (``mesedi.pause_for_human``, ``mesedi.request_human_intervention``)
    cover the request/response capture cleanly already; bridging
    from interrupt() to those helpers must be done by the caller.
"""

from __future__ import annotations

import time
import uuid
from typing import Any, Dict, Optional

from mesedi._context import current_execution_context
from mesedi.client import get_client
from mesedi.events import Event, EventType, utcnow_rfc3339
from mesedi.integrations.langchain import MesediCallbackHandler


# How many characters of node-state dict repr we keep on a checkpoint.
# Wide enough to be useful for semantic_loop hashing; narrow enough
# to avoid blowing up event payload size on graphs with large state.
_MAX_NODE_STATE_REPR = 1000


class MesediLangGraphHandler(MesediCallbackHandler):
    """Extends :class:`MesediCallbackHandler` with LangGraph-specific
    behavior.

    On every node entry, emits a ``checkpoint`` event whose payload
    captures the node name plus a truncated repr of the current
    state. The semantic_loop detector hashes this state to
    catch agents that revisit the same logical state across multiple
    node visits.

    On every sub-graph invocation, emits an ``agent_handoff`` event
 so the topology graph can render the parent/child
    relationship and the cascading_failure +
    coordination_deadlock detectors can fire on the resulting
    cross-node patterns.

    Sub-graph detection: LangGraph marks compiled sub-graph
    invocations with the ``langgraph`` tag in the metadata. We watch
    for that tag on ``on_chain_start`` and emit a handoff with the
    parent node as ``from_agent`` and the sub-graph name as
    ``to_agent``.
    """

    def __init__(self) -> None:
        super().__init__()
        # run_id → (node_name, started_at). Used to pair node
        # start/end so we can compute node duration and emit it
        # alongside the checkpoint.
        self._node_starts: Dict[Any, tuple] = {}
        # run_id → (from_agent, to_agent). Used to pair sub-graph
        # start/end so we can compute handoff duration.
        self._handoff_starts: Dict[Any, tuple] = {}

    # ── Node-level chain events ─────────────────────────────────────

    def on_chain_start(
        self,
        serialized: Dict[str, Any],
        inputs: Dict[str, Any],
        *,
        run_id: Any,
        parent_run_id: Optional[Any] = None,
        tags: Optional[list] = None,
        metadata: Optional[Dict[str, Any]] = None,
        **kwargs: Any,
    ) -> None:
        """Fire on every chain entry. LangGraph nodes ARE chains, so
        this is also called per node. We use the metadata tag
        ``langgraph_node`` to distinguish node entries from generic
        LangChain chains.
        """
        node_name = self._extract_langgraph_node_name(serialized, tags, metadata)
        if node_name:
            self._node_starts[run_id] = (node_name, time.perf_counter())
            self._emit_checkpoint(
                node_name=node_name,
                state=inputs,
                node_event="enter",
            )

        # Sub-graph invocation detection. LangGraph compiles
        # sub-graphs into separate Runnable objects; when one is
        # invoked from another graph, the metadata includes the
        # sub-graph identifier.
        subgraph_name = self._extract_subgraph_name(serialized, tags, metadata)
        if subgraph_name:
            from_agent = self._most_recent_node() or "graph"
            self._handoff_starts[run_id] = (from_agent, subgraph_name)
            self._emit_handoff_request(
                from_agent=from_agent,
                to_agent=subgraph_name,
                task_summary=self._summarize_inputs(inputs),
            )

    def on_chain_end(
        self,
        outputs: Dict[str, Any],
        *,
        run_id: Any,
        parent_run_id: Optional[Any] = None,
        **kwargs: Any,
    ) -> None:
        """Pair with on_chain_start. We emit a checkpoint at the
        node EXIT too so detectors that look at state evolution
        (semantic_loop, drift) see both the entry and the result."""
        if run_id in self._node_starts:
            node_name, started_at = self._node_starts.pop(run_id)
            duration_ms = int((time.perf_counter() - started_at) * 1000)
            self._emit_checkpoint(
                node_name=node_name,
                state=outputs,
                node_event="exit",
                duration_ms=duration_ms,
            )

        # If this run_id was a sub-graph invocation, the handoff
        # completes here. We do not emit a separate completion
        # event; the agent_handoff payload already captures the
        # outbound delegation, and the sub-graph's nested execution
        # (if it ran as a @wrap'd run) carries its own terminal
        # status.
        self._handoff_starts.pop(run_id, None)

    def on_chain_error(
        self,
        error: BaseException,
        *,
        run_id: Any,
        parent_run_id: Optional[Any] = None,
        **kwargs: Any,
    ) -> None:
        """Drop in-flight node / handoff state on error. The
        upstream @wrap captures the crash signature; we just clean
        up our bookkeeping so we don't leak unbounded run_id state.
        """
        if run_id in self._node_starts:
            node_name, started_at = self._node_starts.pop(run_id)
            duration_ms = int((time.perf_counter() - started_at) * 1000)
            self._emit_checkpoint(
                node_name=node_name,
                state={"error_type": type(error).__name__},
                node_event="error",
                duration_ms=duration_ms,
            )
        self._handoff_starts.pop(run_id, None)

    # ── Helpers ─────────────────────────────────────────────────────

    @staticmethod
    def _extract_langgraph_node_name(
        serialized: Dict[str, Any],
        tags: Optional[list],
        metadata: Optional[Dict[str, Any]],
    ) -> Optional[str]:
        """LangGraph attaches the node name via metadata or tags.

        Conventions across versions:
          - ``metadata['langgraph_node']``: newest versions
          - ``tags`` includes ``langgraph:node:<name>``: older
          - ``metadata['node']``: some fork variants

        Returns the node name as a string or None if this chain
        entry is not a LangGraph node (e.g. an LCEL chain inside a
        node, which we deliberately ignore to avoid double-counting).
        """
        if isinstance(metadata, dict):
            name = metadata.get("langgraph_node") or metadata.get("node")
            if isinstance(name, str) and name:
                return name
        if isinstance(tags, list):
            for t in tags:
                if isinstance(t, str) and t.startswith("langgraph:node:"):
                    return t.split(":", 2)[2]
        return None

    @staticmethod
    def _extract_subgraph_name(
        serialized: Dict[str, Any],
        tags: Optional[list],
        metadata: Optional[Dict[str, Any]],
    ) -> Optional[str]:
        """Detect a sub-graph invocation. LangGraph marks compiled
        sub-graphs with metadata.langgraph_step + a non-None
        sub-graph id.
        """
        if isinstance(metadata, dict):
            sg = metadata.get("langgraph_subgraph") or metadata.get("subgraph")
            if isinstance(sg, str) and sg:
                return sg
        if isinstance(tags, list):
            for t in tags:
                if isinstance(t, str) and t.startswith("langgraph:subgraph:"):
                    return t.split(":", 2)[2]
        return None

    def _most_recent_node(self) -> Optional[str]:
        """Return the most recently entered node name. Used as the
        from_agent for sub-graph handoffs when we don't have an
        explicit parent context.
        """
        if not self._node_starts:
            return None
        # Insertion order is preserved in Python 3.7+ dicts.
        last_key = next(reversed(self._node_starts))
        return self._node_starts[last_key][0]

    @staticmethod
    def _summarize_inputs(inputs: Dict[str, Any]) -> str:
        """Build a short task summary from a node's inputs. Truncated
        to keep payload size bounded.
        """
        r = repr(inputs)
        if len(r) > _MAX_NODE_STATE_REPR:
            r = r[: _MAX_NODE_STATE_REPR - 3] + "..."
        return r

    @staticmethod
    def _emit_checkpoint(
        node_name: str,
        state: Any,
        node_event: str,
        duration_ms: int = 0,
    ) -> None:
        """Emit a Mesedi ``checkpoint`` event with the node name and
        a truncated state repr. Outside ``@wrap``: no-op.
        """
        ctx = current_execution_context()
        if ctx is None:
            return
        ctx.check_budget()
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.increment_steps()

        state_repr = repr(state) if state is not None else ""
        if len(state_repr) > _MAX_NODE_STATE_REPR:
            state_repr = state_repr[: _MAX_NODE_STATE_REPR - 3] + "..."

        payload: Dict[str, Any] = {
            "name": f"langgraph.{node_event}.{node_name}",
            "node": node_name,
            "node_event": node_event,
            "state_repr": state_repr,
        }
        if duration_ms:
            payload["duration_ms"] = int(duration_ms)

        client = get_client()
        client.submit_event(Event(
            event_id=f"evt-{uuid.uuid4().hex[:12]}",
            execution_id=ctx.execution_id,
            event_type=EventType.CHECKPOINT,
            sequence=ctx.next_sequence(),
            timestamp=utcnow_rfc3339(),
            duration_ms=duration_ms,
            payload=payload,
        ))

    @staticmethod
    def _emit_handoff_request(
        from_agent: str,
        to_agent: str,
        task_summary: str,
    ) -> None:
        """Emit an ``agent_handoff`` event when LangGraph invokes a
        sub-graph from one node to another. Mirrors the wire format
        of ``mesedi.emit_agent_handoff`` so the topology and
        cascading_failure detectors see it the same way.
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
            "handoff_kind": "delegate",
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


def instrument_langgraph(graph: Any) -> Any:
    """Attach Mesedi telemetry to a compiled LangGraph.

    Wraps the supplied ``CompiledStateGraph`` so every call to
    ``invoke``, ``ainvoke``, ``stream``, or ``astream`` automatically
    attaches a :class:`MesediLangGraphHandler` to the LangChain
    callback config. The wrap is non-destructive: it adds the
    handler to whatever callbacks the caller passed in via
    ``config={"callbacks": [...]}``, so existing instrumentation
    keeps working.

    Returns the same ``graph`` object with patched methods, so
    callers can re-assign in place::

        graph = StateGraph(...).compile()
        graph = instrument_langgraph(graph)
        # ... use graph normally; Mesedi telemetry is wired up.

    Outside a ``@mesedi.wrap`` execution context, the handler's
    emissions silently no-op, so it is safe to instrument the graph
    once at module load and let individual invocations decide
    whether they want to be observed.
    """
    handler = MesediLangGraphHandler()

    # Capture the original bound methods. We replace them with
    # thin wrappers that inject the handler into the config.
    _orig_invoke = getattr(graph, "invoke", None)
    _orig_ainvoke = getattr(graph, "ainvoke", None)
    _orig_stream = getattr(graph, "stream", None)
    _orig_astream = getattr(graph, "astream", None)

    def _ensure_handler_in_config(config: Optional[Dict[str, Any]]) -> Dict[str, Any]:
        cfg: Dict[str, Any] = dict(config or {})
        callbacks = cfg.get("callbacks") or []
        # Avoid double-attaching if the caller already passed one.
        for cb in callbacks:
            if isinstance(cb, MesediLangGraphHandler):
                return cfg
        cfg["callbacks"] = list(callbacks) + [handler]
        return cfg

    if _orig_invoke is not None:
        def invoke(input, config=None, **kwargs):
            return _orig_invoke(input, _ensure_handler_in_config(config), **kwargs)
        graph.invoke = invoke  # type: ignore[attr-defined]

    if _orig_ainvoke is not None:
        async def ainvoke(input, config=None, **kwargs):
            return await _orig_ainvoke(input, _ensure_handler_in_config(config), **kwargs)
        graph.ainvoke = ainvoke  # type: ignore[attr-defined]

    if _orig_stream is not None:
        def stream(input, config=None, **kwargs):
            yield from _orig_stream(input, _ensure_handler_in_config(config), **kwargs)
        graph.stream = stream  # type: ignore[attr-defined]

    if _orig_astream is not None:
        async def astream(input, config=None, **kwargs):
            async for item in _orig_astream(input, _ensure_handler_in_config(config), **kwargs):
                yield item
        graph.astream = astream  # type: ignore[attr-defined]

    return graph


__all__ = ["MesediLangGraphHandler", "instrument_langgraph"]
