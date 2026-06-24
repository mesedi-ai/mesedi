"""
Direct-emission helpers for events that don't fit the decorator pattern.

@wrap and @tool wrap functions so observation is implicit at the
boundary. Checkpoints and validator results don't have a natural
"function" to wrap, they're markers inserted at points of interest
inside agent code, often inside the same function the @wrap decorator
already covers. For those, a plain function call is the right API:

    mesedi.checkpoint("after_retrieval", documents=5, used_cache=True)

    if not result:
        mesedi.validator_result(
            "non-empty-response",
            passed=False,
            message="LLM returned empty content",
            severity="error",
        )

Both helpers no-op when called outside an active @wrap execution
context, same fail-open pattern as @tool. This means a sandbox
script that calls checkpoint() at module load without setting up a
@wrap'd function won't crash; it just won't record anything.

Future slices may add a ``@validator`` decorator for the case where
the validator is a reusable function rather than an ad-hoc check, but
the function-call surface stays as the foundation either way.
"""

from __future__ import annotations

import uuid
from datetime import datetime, timezone
from typing import Any, Dict, Optional

from mesedi._context import current_execution_context
from mesedi.client import get_client
from mesedi.events import Event, EventType, utcnow_rfc3339

# Truncation budget for validator messages. Validator messages are
# typically short ("schema mismatch at field X") but we don't want a
# pathological agent that pastes a 10MB JSON diff to blow up the
# events table.
_MAX_VALIDATOR_MSG = 500
# Granular-signature wave: customer-supplied validator category gets
# concatenated into the failure_group signature backend-side as
# "<name>:<category>". 80 chars matches the backend's
# signaturePartMaxLen helper; longer values are truncated.
_MAX_VALIDATOR_CATEGORY = 80

# Truncation budgets for manually-emitted llm_call events. Match the
# values in anthropic_integration.py so wire-format payloads from the
# patched Anthropic SDK and from emit_llm_call() are byte-identical.
_MAX_LLM_SYSTEM = 1000
_MAX_LLM_USER_MSG = 1000
_MAX_LLM_RESPONSE = 1000


def checkpoint(name: str, **metadata: Any) -> None:
    """Emit a ``checkpoint`` event marking a notable point in execution.

    A checkpoint is a free-form marker: a name and arbitrary keyword
    metadata. Typical uses: "after_retrieval", "before_synthesis",
    "cache_hit", etc. Useful both for Phase-3+ detector hooks (drift,
    cost-velocity) and for ad-hoc debugging, replay UI in a later
    phase will render checkpoints as anchored markers on the
    execution timeline.

    Args:
        name: Short identifier for this checkpoint. Becomes the
            primary grouping key in the dashboard's checkpoint view.
        **metadata: Arbitrary additional context. JSON-serializable
            values only (strings, numbers, bools, lists, dicts).
            Non-serializable values would crash the shipper's
            json.dumps; defensive: callers should pre-serialize
            anything unusual.

    Outside @wrap: no-op. Drops the event silently, the caller still
    runs; nothing observed.
    """
    ctx = current_execution_context()
    if ctx is None:
        return

    # Halt-safe boundary: checkpoint is the canonical place for users
    # to insert their own "ok to halt here" markers. Budget check
    # runs first so a halt fires before the event is emitted.
    ctx.check_budget()
    if ctx.budget_tracker is not None:
        ctx.budget_tracker.increment_steps()

    client = get_client()
    client.submit_event(Event(
        event_id=f"evt-{uuid.uuid4().hex[:12]}",
        execution_id=ctx.execution_id,
        event_type=EventType.CHECKPOINT,
        sequence=ctx.next_sequence(),
        timestamp=utcnow_rfc3339(),
        payload={
            "name": name,
            "metadata": metadata,
        },
    ))


def validator_result(
    name: str,
    passed: bool,
    message: str = "",
    severity: str = "error",
    category: str = "",
) -> None:
    """Report a validator outcome as a ``validator_result`` event.

    Validators are checks the agent (or its framework) runs against
    intermediate or final outputs: schema conformance, factuality,
    relevance, safety. The result of each check, pass or fail ,
    becomes a discrete event so Phase-3 detection can spot patterns
    like "validator X has been failing 90% of the time on this
    model".

    Args:
        name: Validator identifier. Becomes the grouping key for
            validator-failure failure_groups in Phase 6.
        passed: True if the validator passed, False if it failed.
        message: Optional human-readable diagnostic. Truncated to
            500 chars.
        severity: "warning" | "error" | "critical". Hints to the
            backend how aggressively to surface a failing validator
            on the dashboard; the SDK doesn't enforce values today
            but the dashboard will color-code in Phase 6.
        category: Optional sub-classification of WHY the validator
            failed (e.g. "schema_mismatch", "hallucination",
            "missing_required_field"). When supplied, the backend
            concatenates it into the failure_group signature as
            "<name>:<category>" so a single ``quality_check``
            validator failing for 50 different reasons surfaces as
            50 clusters instead of one impossibly-coarse cluster.
            Truncated to 80 chars + control characters stripped
            backend-side. Backward-compat: empty category (the
            default) preserves the original "<name>"-only signature
            shape, so existing customers see zero behavior change
            until they opt in.

    Outside @wrap: no-op.
    """
    ctx = current_execution_context()
    if ctx is None:
        return

    if severity not in {"warning", "error", "critical"}:
        # Don't raise, the caller's agent shouldn't fail because of
        # an SDK-side validation. Coerce to the safest default.
        severity = "error"

    client = get_client()
    payload: Dict[str, Any] = {
        "name": name,
        "passed": passed,
        "severity": severity,
    }
    if message:
        payload["message"] = message[:_MAX_VALIDATOR_MSG]
    if category:
        # Truncate client-side so the wire payload stays bounded;
        # backend re-sanitizes (strips control chars + re-caps) as
        # defense-in-depth.
        payload["category"] = category[:_MAX_VALIDATOR_CATEGORY]

    client.submit_event(Event(
        event_id=f"evt-{uuid.uuid4().hex[:12]}",
        execution_id=ctx.execution_id,
        event_type=EventType.VALIDATOR_RESULT,
        sequence=ctx.next_sequence(),
        timestamp=utcnow_rfc3339(),
        payload=payload,
    ))


def emit_llm_call(
    model: str,
    user_message: str,
    system_prompt: str = "",
    response_text: str = "",
    input_tokens: int = 0,
    output_tokens: int = 0,
    duration_ms: int = 0,
    status: str = "ok",
) -> None:
    """Emit an ``llm_call`` event with the same wire format the
    Anthropic patch produces.

    This is the manual escape hatch for LLM providers Mesedi doesn't
    auto-instrument (OpenAI, Google, Mistral, Together, local models,
    mocked calls in dogfood scripts, etc.). Call it after each model
    invocation with the same fields the patched Anthropic create
    method would have captured automatically:

        mesedi.emit_llm_call(
            model="gpt-4o",
            user_message=user_prompt,
            system_prompt=system_prompt,
            response_text=completion_text,
            input_tokens=usage.prompt_tokens,
            output_tokens=usage.completion_tokens,
            duration_ms=int((time.perf_counter() - start) * 1000),
        )

    Drift / similar-call / identical-call / cost-velocity / prompt-
    injection detectors all read from the resulting event payload, so
    a manually-emitted llm_call event is detector-complete the same
    way an auto-instrumented one is.

    Halt-safe: this function is a safe halt boundary. Budget check
    runs first (just like ``@tool``, ``checkpoint()``, and the patched
    Anthropic create), so a halt fires here before the event is
    persisted.

    Outside @wrap: no-op. Mirrors the fail-open pattern of every
    other observe-layer primitive.

    Args:
        model: The model identifier (e.g. "gpt-4o",
            "claude-haiku-4-5-20251001"). Captured verbatim into the
            event's ``payload.model`` for drift detection and cost
            attribution.
        user_message: The user-role prompt. Truncated to 1000 chars
            to match the Anthropic patch's truncation budget.
        system_prompt: The system-role prompt. Truncated to 1000 chars.
        response_text: The model's response. Truncated to 1000 chars.
        input_tokens: Token count for the prompt; used by cost-velocity.
        output_tokens: Token count for the response; used by
            cost-velocity.
        duration_ms: Wall-clock duration of the LLM call in ms. Pass 0
            if not measured.
        status: "ok" if the call returned cleanly, "failed" otherwise.
            Failed calls still record their model name (which still
            feeds drift) but don't contribute response_text/token data.
    """
    ctx = current_execution_context()
    if ctx is None:
        return

    # Halt-safe boundary: same pattern as the Anthropic patch.
    ctx.check_budget()
    if ctx.budget_tracker is not None:
        ctx.budget_tracker.increment_steps()
        if input_tokens > 0 or output_tokens > 0:
            ctx.budget_tracker.add_tokens(tokens_in=input_tokens, tokens_out=output_tokens)

    client = get_client()
    payload: Dict[str, Any] = {
        "model": model,
        "system_prompt": (system_prompt or "")[:_MAX_LLM_SYSTEM],
        "user_message": (user_message or "")[:_MAX_LLM_USER_MSG],
        "status": status,
    }
    if status == "ok":
        payload["response_text"] = (response_text or "")[:_MAX_LLM_RESPONSE]
        payload["input_tokens"] = int(input_tokens)
        payload["output_tokens"] = int(output_tokens)

    client.submit_event(Event(
        event_id=f"evt-{uuid.uuid4().hex[:12]}",
        execution_id=ctx.execution_id,
        event_type=EventType.LLM_CALL,
        sequence=ctx.next_sequence(),
        timestamp=utcnow_rfc3339(),
        duration_ms=duration_ms,
        payload=payload,
    ))


# Reasons recognized by ThrottlingSignature on the backend. Keep in
# sync with backend/internal/store/sqlite.go::ThrottlingSignature.
_INFRA_REASONS = frozenset({"rate_limit", "circuit_breaker", "quota_exhausted"})


# Canonical error_class values that signal per-tenant throttling — used
# by the instrument_* modules to decide whether to auto-emit an
# infrastructure_event alongside their failed llm_call event so the
# infrastructure_throttled detector picks up the pattern. Deliberately
# narrow: RATE_LIMITED and QUOTA_EXHAUSTED are unambiguous throttling
# signals. Anthropic's OverloadedError (HTTP 529) is NOT here because
# the canonical error class set conflates it with SERVICE_UNAVAILABLE
# (which also catches network outages and 5xx — provider_incident
# territory, not throttling). A future wave that splits
# SERVICE_UNAVAILABLE into separate classes (e.g. PROVIDER_OVERLOADED
# vs PROVIDER_DOWN) can extend this set; for now we err toward
# false-negatives over false-positives.
_THROTTLING_ERROR_CLASSES = frozenset({
    "rate_limited",
    "quota_exhausted",
})

_REASON_BY_ERROR_CLASS = {
    "rate_limited": "rate_limit",
    "quota_exhausted": "quota_exhausted",
}


def _maybe_emit_throttling_event(
    provider: str,
    error_class: str,
    http_status: Optional[int] = None,
    retry_after_seconds: Optional[float] = None,
    endpoint: str = "",
    quota_dimension: str = "",
) -> None:
    """If the canonical error_class is a throttling signal, emit an
    infrastructure_event alongside the failed llm_call so the
    backend's infrastructure_throttled detector picks up the
    per-tenant backpressure pattern. No-op for non-throttling
    errors. Internal helper used by the four instrument_* modules
    (anthropic, openai, cohere, gemini) so the throttling-class
    filter and reason-mapping live in one place.
    """
    if error_class not in _THROTTLING_ERROR_CLASSES:
        return
    reason = _REASON_BY_ERROR_CLASS.get(error_class, error_class)
    retry_after_ms = 0
    if retry_after_seconds is not None and retry_after_seconds > 0:
        retry_after_ms = int(float(retry_after_seconds) * 1000)
    emit_infrastructure_event(
        reason=reason,
        provider=provider,
        endpoint=endpoint,
        status_code=int(http_status) if http_status is not None else 0,
        retry_after_ms=retry_after_ms,
        quota_dimension=quota_dimension,
    )


def emit_infrastructure_event(
    reason: str,
    provider: str = "",
    endpoint: str = "",
    status_code: int = 0,
    retry_after_ms: int = 0,
    quota_remaining: int = 0,
    quota_limit: int = 0,
    quota_dimension: str = "",
    backoff_applied_ms: int = 0,
    circuit_state: str = "",
) -> None:
    """Emit an ``infrastructure_event`` for transport-plane backpressure.

    Used when the SDK observes an HTTP 429, hits a provider quota
    header, trips a local circuit breaker, or otherwise gets a signal
    from the network plane that the agent's logic is fine but the
    underlying infrastructure is pushing back. The Mesedi backend's
    ``infrastructure_throttled`` detector consumes these events and
    clusters them by (reason, provider, quota_dimension) so SRE teams
    get a single page per affected provider instead of one page per
    request.

    Typical caller pattern (synchronous retry-loop):

        try:
            resp = httpx.post(endpoint, json=body, timeout=30)
            resp.raise_for_status()
        except httpx.HTTPStatusError as exc:
            if exc.response.status_code == 429:
                retry_after = int(
                    exc.response.headers.get("retry-after-ms", 0)
                )
                mesedi.emit_infrastructure_event(
                    reason="rate_limit",
                    provider="anthropic",
                    endpoint="/v1/messages",
                    status_code=429,
                    retry_after_ms=retry_after,
                    quota_dimension="tokens_per_minute",
                    quota_remaining=int(
                        exc.response.headers.get("x-ratelimit-remaining", 0)
                    ),
                )
                raise

    Halt-safe: the budget check runs before the event is emitted, so a
    halt request issued mid-throttling still fires immediately. The
    SDK does NOT automatically retry on its own; emitting this event
    is purely observational. The caller's retry policy (httpx, openai
    SDK, langchain, etc.) is the source of truth for whether to wait
    and try again.

    Outside @wrap: no-op. Mirrors the fail-open pattern of every other
    observe-layer primitive.

    Args:
        reason: One of "rate_limit", "circuit_breaker",
            "quota_exhausted". Unknown values are passed through to the
            backend, which clusters them as "<reason>:<provider>" so
            you still get useful grouping for SDK-side reasons the
            detector doesn't yet know about.
        provider: Short provider identifier ("anthropic", "openai",
            "google", etc.). Used as the primary signature dimension.
            Empty string is allowed but degrades grouping to
            "rate_limit:unknown" style.
        endpoint: The provider URL path that triggered the event.
            Captured for the expanded payload view but not used in the
            signature (so per-endpoint variations cluster together).
        status_code: HTTP status returned (429, 503, etc.). Captured
            for the expanded view.
        retry_after_ms: Server-suggested backoff window in
            milliseconds, parsed from the Retry-After header.
        quota_remaining: Calls / tokens still available on this quota,
            from x-ratelimit-remaining or similar.
        quota_limit: Maximum on this quota, from x-ratelimit-limit or
            similar.
        quota_dimension: Which quota dimension breached. Use stable
            string identifiers ("tokens_per_minute",
            "requests_per_second", "tokens_per_day", etc.) so signature
            grouping stays consistent.
        backoff_applied_ms: How long the caller actually waited before
            retrying. Useful for sizing future quota requests.
        circuit_state: When reason="circuit_breaker", one of
            "open" (request blocked), "half_open" (probe in flight),
            "closed" (recovered). Empty defaults to "open" on the
            backend signature side.
    """
    ctx = current_execution_context()
    if ctx is None:
        return

    ctx.check_budget()
    if ctx.budget_tracker is not None:
        ctx.budget_tracker.increment_steps()

    payload: Dict[str, Any] = {"event_type": reason}
    if provider:
        payload["provider"] = provider
    if endpoint:
        payload["endpoint"] = endpoint
    if status_code:
        payload["status_code"] = int(status_code)
    if retry_after_ms:
        payload["retry_after_ms"] = int(retry_after_ms)
    if quota_remaining:
        payload["quota_remaining"] = int(quota_remaining)
    if quota_limit:
        payload["quota_limit"] = int(quota_limit)
    if quota_dimension:
        payload["quota_dimension"] = quota_dimension
    if backoff_applied_ms:
        payload["backoff_applied_ms"] = int(backoff_applied_ms)
    if circuit_state:
        payload["circuit_state"] = circuit_state

    client = get_client()
    client.submit_event(Event(
        event_id=f"evt-{uuid.uuid4().hex[:12]}",
        execution_id=ctx.execution_id,
        event_type=EventType.INFRASTRUCTURE_EVENT,
        sequence=ctx.next_sequence(),
        timestamp=utcnow_rfc3339(),
        payload=payload,
    ))


def emit_mcp_call(
    server_name: str,
    method: str,
    server_url: str = "",
    arguments: Any = None,
    return_value: Any = None,
    latency_ms: int = 0,
    error: str = "",
    error_class: str = "",
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


def emit_eval_score(
    evaluator_id: str,
    metric_type: str,
    score: float,
    passed: bool,
    threshold: float = 0.0,
    higher_is_better: bool = True,
    reason: str = "",
    confidence: float = 0.0,
) -> None:
    """Emit an ``eval_score`` event recording one external evaluator
    verdict.

    Use this when you run Ragas, Promptfoo, Vectara HHEM, a custom
    LLM-judge, or any other evaluator against an execution's output
    and want Mesedi to track the score over time. Mesedi #14
    (grounding_failure) aggregates these events across executions
    to fire alerts when scores trend below threshold.

    Typical caller pattern (Ragas faithfulness)::

        from ragas.metrics import faithfulness
        score = faithfulness.score(question, answer, contexts)
        mesedi.emit_eval_score(
            evaluator_id="ragas/faithfulness",
            metric_type="faithfulness",
            score=score,
            passed=score >= 0.7,
            threshold=0.7,
            higher_is_better=True,
        )

    Outside @wrap: no-op.

    Args:
        evaluator_id: Stable identifier for the evaluator
            ("ragas/faithfulness", "vectara-hhem/v1", "custom:my-judge").
            Used as the primary aggregation key.
        metric_type: What the score measures ("faithfulness",
            "relevance", "hallucination_rate", "answer_correctness", ...).
            Free-form; the dashboard groups by it.
        score: The numeric score the evaluator produced.
        passed: The evaluator's own pass/fail verdict.
        threshold: Optional cutoff value the evaluator used.
        higher_is_better: True for faithfulness/relevance/correctness;
            False for inverse metrics like hallucination_rate.
        reason: Optional explanation the evaluator returned.
        confidence: Optional [0, 1] self-confidence the evaluator
            reports about its score.
    """
    ctx = current_execution_context()
    if ctx is None:
        return

    ctx.check_budget()
    if ctx.budget_tracker is not None:
        ctx.budget_tracker.increment_steps()

    payload: Dict[str, Any] = {
        "evaluator_id": evaluator_id,
        "metric_type": metric_type,
        "score": float(score),
        "passed": bool(passed),
        "higher_is_better": bool(higher_is_better),
    }
    if threshold:
        payload["threshold"] = float(threshold)
    if reason:
        payload["reason"] = reason
    if confidence:
        payload["confidence"] = float(confidence)

    client = get_client()
    client.submit_event(Event(
        event_id=f"evt-{uuid.uuid4().hex[:12]}",
        execution_id=ctx.execution_id,
        event_type=EventType.EVAL_SCORE,
        sequence=ctx.next_sequence(),
        timestamp=utcnow_rfc3339(),
        payload=payload,
    ))


def emit_memory_operation(
    operation: str,
    store_type: str = "",
    store_name: str = "",
    query: str = "",
    document_count: int = 0,
    token_count: int = 0,
    top_score: float = 0.0,
    latency_ms: int = 0,
    cache_hit: bool = False,
    error: str = "",
    error_class: str = "",
) -> None:
    """Emit a ``memory_operation`` event for one external memory
    store read / write / search.

    Use this when your agent reads from or writes to a vector
    database (Pinecone, Weaviate, pgvector, Qdrant), a key-value
    cache, or any other backing store that holds agent state outside
    the model context. The dashboard renders memory ops in a distinct
    chip so cost / latency attribution can break down per store.

    Outside @wrap: no-op.
    """
    ctx = current_execution_context()
    if ctx is None:
        return

    ctx.check_budget()
    if ctx.budget_tracker is not None:
        ctx.budget_tracker.increment_steps()

    payload: Dict[str, Any] = {"operation": operation}
    if store_type:
        payload["store_type"] = store_type
    if store_name:
        payload["store_name"] = store_name
    if query:
        payload["query"] = query[:1000]
    if document_count:
        payload["document_count"] = int(document_count)
    if token_count:
        payload["token_count"] = int(token_count)
    if top_score:
        payload["top_score"] = float(top_score)
    if latency_ms:
        payload["latency_ms"] = int(latency_ms)
    if cache_hit:
        payload["cache_hit"] = True
    if error:
        payload["error"] = error
    if error_class:
        payload["error_class"] = error_class

    client = get_client()
    client.submit_event(Event(
        event_id=f"evt-{uuid.uuid4().hex[:12]}",
        execution_id=ctx.execution_id,
        event_type=EventType.MEMORY_OPERATION,
        sequence=ctx.next_sequence(),
        timestamp=utcnow_rfc3339(),
        duration_ms=latency_ms,
        payload=payload,
    ))


def emit_agent_handoff(
    from_agent: Optional[str] = None,
    to_agent: str = "",
    handoff_kind: str = "",
    task_summary: str = "",
    child_execution_id: str = "",
    latency_ms: int = 0,
    error: str = "",
    error_class: str = "",
) -> None:
    """Emit an ``agent_handoff`` event marking that the current agent
    delegated work to another agent.

    Mesedi #11. Use this at the moment one agent invokes another:
    a supervisor calling a worker, a planner calling an executor, a
    role-based router dispatching to a sub-agent, or any other
    inter-agent handoff. The dashboard joins this event back to the
    topology graph (#10) so that the cascading_failure detector
    (#12) can correlate a handoff with a child execution that
    crashed shortly afterwards.

    Common values for ``handoff_kind``:

    * ``"delegate"`` — one-shot, expects a return value
    * ``"spawn"`` — fire-and-forget background sub-agent
    * ``"transfer"`` — control transferred (no return)
    * ``"consult"`` — short Q&A, return text only

    ``child_execution_id`` is optional at emit-time. If the SDK has
    already opened the nested ``@wrap`` for the target agent, pass
    its ``execution_id`` so the backend can join the handoff
    directly. If not, the topology graph still links the two via
    ``parent_execution_id``; the handoff event remains useful for
    surfacing the cross-agent intent.

    Resolution of ``from_agent`` (Wave 1.2.b ergonomics):

      1. Explicit ``from_agent=...`` argument wins if supplied.
      2. Otherwise, the ``agent_name`` set on the active
         ``@mesedi.wrap(agent_name=...)`` decoration is used.
      3. If neither is supplied while inside ``@wrap``, this function
         raises ``ValueError`` rather than silently emitting an
         ``unknown`` source agent — polluting the topology graph
         would degrade ``cascading_failure`` / ``coordination_deadlock``
         clustering at scale.

    Outside ``@wrap``: no-op (matches the existing fail-open contract).
    """
    ctx = current_execution_context()
    if ctx is None:
        return

    # Resolve the effective source agent. Explicit beats context;
    # missing both inside @wrap is a caller-side logic bug we surface
    # loudly so the customer fixes it in dev rather than polluting the
    # topology graph in prod.
    effective_from = from_agent if from_agent is not None else ctx.agent_name
    if effective_from is None:
        raise ValueError(
            "emit_agent_handoff: no source agent identity available. "
            "Either pass from_agent=... explicitly, or decorate the "
            "calling @mesedi.wrap with agent_name='...'."
        )

    if not to_agent:
        raise ValueError(
            "emit_agent_handoff: to_agent is required (cannot be empty)."
        )

    ctx.check_budget()
    if ctx.budget_tracker is not None:
        ctx.budget_tracker.increment_steps()

    payload: Dict[str, Any] = {
        "from_agent": effective_from,
        "to_agent": to_agent,
    }
    if handoff_kind:
        payload["handoff_kind"] = handoff_kind
    if task_summary:
        payload["task_summary"] = task_summary[:1000]
    if child_execution_id:
        payload["child_execution_id"] = child_execution_id
    if latency_ms:
        payload["latency_ms"] = int(latency_ms)
    if error:
        payload["error"] = error
    if error_class:
        payload["error_class"] = error_class

    client = get_client()
    client.submit_event(Event(
        event_id=f"evt-{uuid.uuid4().hex[:12]}",
        execution_id=ctx.execution_id,
        event_type=EventType.AGENT_HANDOFF,
        sequence=ctx.next_sequence(),
        timestamp=utcnow_rfc3339(),
        duration_ms=latency_ms,
        payload=payload,
    ))


def pause_for_human() -> None:
    """Synchronously transition the current execution to
    ``awaiting_human`` (Mesedi #18).

    Call this from inside a ``@wrap``'d function at the moment the
    agent reaches a decision that requires a human in the loop. The
    backend records the pause timestamp, increments pause_count, and
    starts the HITL clock. Subsequent calls to ``resume_for_agent()``
    add the elapsed time to total_paused_ms so a long HITL wait does
    not falsely trip the agent's time budget.

    The host application is responsible for actually blocking the
    agent until the human responds (typically the @wrap'd function
    body itself blocks on a queue / websocket / database row). This
    helper only updates Mesedi's lifecycle state; it does not block.

    Outside ``@wrap``: no-op.

    Important: this is a SYNCHRONOUS HTTP call rather than the
    asynchronous shipper path the events take. The pause/resume
    transitions must be committed before the host application blocks
    or releases the agent, so async-only delivery would race against
    the human responding.
    """
    ctx = current_execution_context()
    if ctx is None:
        return
    client = get_client()
    # Drain pending shipper traffic first so the POST /executions
    # that opened this @wrap has landed on the backend. Without
    # this, the synchronous PATCH races the async POST and returns
    # 404 when pause is called immediately after the agent starts.
    client.flush(timeout=5.0)
    r = client._http.patch(  # noqa: SLF001
        f"/executions/{ctx.execution_id}",
        json={"status": "awaiting_human"},
    )
    r.raise_for_status()


def resume_for_agent() -> None:
    """Synchronously transition the current execution from
    ``awaiting_human`` back to ``started`` (Mesedi #18).

    Call this from the host application's HITL response handler the
    moment the human's decision lands and the agent is about to be
    unblocked. The backend computes the wait duration and adds it to
    total_paused_ms so the time-budget detector reads only the
    agent's actual working time.

    Outside ``@wrap``: no-op.
    """
    ctx = current_execution_context()
    if ctx is None:
        return
    client = get_client()
    r = client._http.patch(  # noqa: SLF001
        f"/executions/{ctx.execution_id}",
        json={"status": "started"},
    )
    r.raise_for_status()


class HumanInterventionHandle:
    """Handle to an in-flight HITL request (Mesedi #19).

    Returned by :func:`request_human_intervention`. Stash this in
    your host application (database row, queue payload, websocket
    session, whatever) until the human responds. Then call
    :meth:`complete` with the response payload.

    The handle is JSON-serializable via the ``to_dict()`` /
    ``from_dict()`` helpers so it can survive a round-trip through
    Redis, Kafka, or a SQL row without losing the correlation data
    needed to attribute the response back to the original ask.
    """

    def __init__(
        self,
        *,
        execution_id: str,
        request_id: str,
        question: str,
        sla_seconds: int,
        requested_at: str,
        metadata: Optional[Dict[str, Any]] = None,
    ) -> None:
        self.execution_id = execution_id
        self.request_id = request_id
        self.question = question
        self.sla_seconds = sla_seconds
        self.requested_at = requested_at
        self.metadata = metadata or {}

    def to_dict(self) -> Dict[str, Any]:
        """Serialize the handle so the host application can persist
        it across the human-response wait. Pair with
        ``from_dict()`` on the receiving side."""
        return {
            "execution_id": self.execution_id,
            "request_id": self.request_id,
            "question": self.question,
            "sla_seconds": self.sla_seconds,
            "requested_at": self.requested_at,
            "metadata": dict(self.metadata),
        }

    @classmethod
    def from_dict(cls, data: Dict[str, Any]) -> "HumanInterventionHandle":
        return cls(
            execution_id=data["execution_id"],
            request_id=data["request_id"],
            question=data["question"],
            sla_seconds=int(data.get("sla_seconds", 0)),
            requested_at=data["requested_at"],
            metadata=data.get("metadata"),
        )

    def complete(
        self,
        response_kind: str,
        response_payload: Optional[Dict[str, Any]] = None,
        decided_by: str = "",
    ) -> None:
        """Mark the HITL request complete and resume the execution.

        Emits the ``human_intervention`` event with the full
        ask/answer payload, then synchronously transitions the
        execution from ``awaiting_human`` back to ``started`` so
        the agent code can continue.

        Well-known ``response_kind`` values: ``"approved"``,
        ``"rejected"``, ``"edited"``, ``"timeout"``,
        ``"cancelled"``. Custom strings are accepted but downstream
        HITL detectors (#20, #21) only recognize the five
        well-known values.
        """
        decided_at_dt = datetime.now(timezone.utc)
        decided_at = decided_at_dt.isoformat().replace("+00:00", "Z")
        # Compute wait duration from requested_at -> decided_at.
        # Parse requested_at; it should be an ISO8601 'Z' string
        # this SDK produced earlier.
        try:
            requested_dt = datetime.fromisoformat(
                self.requested_at.replace("Z", "+00:00")
            )
            wait_duration_ms = int(
                (decided_at_dt - requested_dt).total_seconds() * 1000
            )
            if wait_duration_ms < 0:
                wait_duration_ms = 0
        except (ValueError, TypeError):
            wait_duration_ms = 0

        payload: Dict[str, Any] = {
            "request_id": self.request_id,
            "question": self.question,
            "requested_at": self.requested_at,
            "response_kind": response_kind,
            "decided_at": decided_at,
            "wait_duration_ms": wait_duration_ms,
        }
        if self.sla_seconds > 0:
            payload["sla_seconds"] = int(self.sla_seconds)
        if response_payload:
            payload["response_payload"] = response_payload
        if decided_by:
            payload["decided_by"] = decided_by
        if self.metadata:
            payload["metadata"] = dict(self.metadata)

        client = get_client()
        # Emit the event via the synchronous send_events path so it
        # lands BEFORE the resume PATCH. This is important so a
        # subsequent flush-on-shutdown does not lose the
        # intervention event if the resume succeeds but the shipper
        # has not yet flushed.
        client.send_events([Event(
            event_id=f"evt-{uuid.uuid4().hex[:12]}",
            execution_id=self.execution_id,
            event_type=EventType.HUMAN_INTERVENTION,
            sequence=0,  # ordering re-derived at the backend
            timestamp=decided_at,
            duration_ms=wait_duration_ms,
            payload=payload,
        )])
        # Resume the execution. The PATCH only succeeds if the
        # execution is currently awaiting_human; if a concurrent
        # timeout already terminated the run, this raises an
        # error the host application can catch.
        r = client._http.patch(  # noqa: SLF001
            f"/executions/{self.execution_id}",
            json={"status": "started"},
        )
        r.raise_for_status()


def request_human_intervention(
    question: str,
    sla_seconds: int = 0,
    metadata: Optional[Dict[str, Any]] = None,
) -> Optional[HumanInterventionHandle]:
    """Pause the current execution and return a handle (Mesedi #19).

    Synchronously transitions the execution into ``awaiting_human``
    and returns a :class:`HumanInterventionHandle` carrying the
    correlation data needed to complete the cycle later. The host
    application is responsible for actually waiting on the human
    response (queue / websocket / DB poll) and then calling
    ``handle.complete(response_kind, response_payload=...)`` when
    the answer arrives.

    Outside ``@wrap``: no-op, returns None.

    ``sla_seconds`` is optional metadata describing the customer's
    own SLA expectation. The hitl_timeout detector (#20) reads
    this to fire when the actual wait exceeds the configured SLA.
    Customers without an explicit SLA can omit it; #20 will then
    use a project-level default.
    """
    ctx = current_execution_context()
    if ctx is None:
        return None
    request_id = f"hitl-{uuid.uuid4().hex[:12]}"
    requested_at = utcnow_rfc3339()
    # Pause the execution lifecycle first; the handle exists only
    # after the lifecycle transition lands.
    client = get_client()
    # Drain pending shipper traffic so the POST /executions for
    # this @wrap has landed. Without this, calling
    # request_human_intervention immediately after the agent
    # starts races the async POST and returns 404.
    client.flush(timeout=5.0)
    r = client._http.patch(  # noqa: SLF001
        f"/executions/{ctx.execution_id}",
        json={"status": "awaiting_human"},
    )
    r.raise_for_status()
    return HumanInterventionHandle(
        execution_id=ctx.execution_id,
        request_id=request_id,
        question=question,
        sla_seconds=int(sla_seconds),
        requested_at=requested_at,
        metadata=metadata,
    )
