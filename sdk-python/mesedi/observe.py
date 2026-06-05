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
from typing import Any, Dict

from mesedi._context import current_execution_context
from mesedi.client import get_client
from mesedi.events import Event, EventType, utcnow_rfc3339

# Truncation budget for validator messages. Validator messages are
# typically short ("schema mismatch at field X") but we don't want a
# pathological agent that pastes a 10MB JSON diff to blow up the
# events table.
_MAX_VALIDATOR_MSG = 500

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
