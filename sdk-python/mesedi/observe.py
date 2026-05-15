"""
Direct-emission helpers for events that don't fit the decorator pattern.

@wrap and @tool wrap functions so observation is implicit at the
boundary. Checkpoints and validator results don't have a natural
"function" to wrap — they're markers inserted at points of interest
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
context — same fail-open pattern as @tool. This means a sandbox
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


def checkpoint(name: str, **metadata: Any) -> None:
    """Emit a ``checkpoint`` event marking a notable point in execution.

    A checkpoint is a free-form marker: a name and arbitrary keyword
    metadata. Typical uses: "after_retrieval", "before_synthesis",
    "cache_hit", etc. Useful both for Phase-3+ detector hooks (drift,
    cost-velocity) and for ad-hoc debugging — replay UI in a later
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

    Outside @wrap: no-op. Drops the event silently — the caller still
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
    relevance, safety. The result of each check — pass or fail —
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
        # Don't raise — the caller's agent shouldn't fail because of
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
