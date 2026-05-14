"""
Anthropic SDK monkey-patch — auto-emit llm_call events for every
``Messages.create`` call inside a ``@mesedi.wrap`` execution.

Activation is **opt-in**: call ``mesedi.instrument_anthropic()`` once at
process startup. This matches the Datadog / Sentry / OpenTelemetry
pattern — observability instrumentation should be explicit, not magical.

What gets captured per call:

  - ``model`` (e.g. "claude-opus-4-6")
  - ``system_prompt`` — truncated to 1000 chars
  - ``user_message`` — the LAST user-role message in the conversation,
    truncated to 1000 chars
  - ``response_text`` — concatenated text-block content from the
    response, truncated to 1000 chars
  - ``input_tokens`` / ``output_tokens`` — from response.usage
  - ``duration_ms`` — wall-clock time of the API call
  - ``status`` — "ok" if the call returned, "failed" if it raised
  - ``exception_type`` / ``exception_message`` — on failure

Truncation budget is intentionally bounded (1000 chars per text field)
so the events table doesn't bloat from agents that paste whole web
pages into prompts. PII redaction is a separate, configurable layer
that lands in a future sub-slice.

Out of scope for this sub-slice:

  - ``AsyncAnthropic.messages.create`` (async client) — patched in the
    async-support sub-slice
  - ``Messages.stream()`` / streaming responses — patched separately
  - Anthropic tools / tool_use response blocks — handled by @mesedi.tool
    at the agent layer, not at the LLM-call layer

Patching is idempotent: calling ``instrument_anthropic()`` twice has no
additional effect.

Dependency injection: ``instrument_anthropic()`` accepts an optional
``messages_class`` parameter so this code path is testable without
installing the actual ``anthropic`` package. Pass any class that has a
``create`` method to patch — the sandbox test does this with a fake
class to verify the patching logic end-to-end.
"""

from __future__ import annotations

import logging
import time
import uuid
from typing import Any, List, Optional, Type

from mesedi._context import current_execution_context
from mesedi.client import get_client
from mesedi.events import Event, EventType, utcnow_rfc3339

logger = logging.getLogger("mesedi.anthropic")

# Truncation budgets. Tunable in a future slice if we want a redaction-
# aware path; today these are constants because the surface is small.
_MAX_SYSTEM = 1000
_MAX_USER_MSG = 1000
_MAX_RESPONSE = 1000
_MAX_EXC_MSG = 500

# Module-level "already patched" flag. The flag is keyed by the class
# object so that injecting different fake classes for testing doesn't
# falsely trip the idempotency check.
_patched_classes: set = set()


def instrument_anthropic(messages_class: Optional[Type[Any]] = None) -> bool:
    """Patch the Anthropic SDK's ``Messages.create`` to emit llm_call events.

    Args:
        messages_class: The class whose ``create`` method should be
            patched. When ``None`` (the default), tries to import
            ``anthropic.resources.messages.Messages``. Passing an
            explicit class is intended for testing — production callers
            should leave this as None and let the function auto-locate
            the real Anthropic class.

    Returns:
        True if patching succeeded (or was a no-op because the class is
        already patched). False if ``anthropic`` is not installed and no
        ``messages_class`` was provided.
    """
    if messages_class is None:
        try:
            from anthropic.resources.messages import Messages as _Messages
            messages_class = _Messages
        except ImportError:
            logger.warning(
                "mesedi: anthropic package not importable; "
                "instrument_anthropic() is a no-op. "
                "Install with `pip install anthropic` to enable."
            )
            return False

    if messages_class in _patched_classes:
        return True

    original_create = messages_class.create

    def patched_create(self: Any, *args: Any, **kwargs: Any) -> Any:
        ctx = current_execution_context()
        if ctx is None:
            # No active execution — run unobserved. Same fail-open
            # pattern as @tool and @wrap.
            return original_create(self, *args, **kwargs)

        client = get_client()
        sequence = ctx.next_sequence()
        event_id = f"evt-{uuid.uuid4().hex[:12]}"

        model = kwargs.get("model", "unknown")
        system_raw = kwargs.get("system", "")
        messages = kwargs.get("messages", [])
        user_message = _extract_last_user_message(messages)

        # System can be a string OR a list of content blocks in the
        # Anthropic SDK; normalize to a string for the payload.
        system_text = system_raw if isinstance(system_raw, str) else str(system_raw)

        start = time.perf_counter()
        try:
            response = original_create(self, *args, **kwargs)
        except BaseException as exc:
            duration_ms = int((time.perf_counter() - start) * 1000)
            client.submit_event(Event(
                event_id=event_id,
                execution_id=ctx.execution_id,
                event_type=EventType.LLM_CALL,
                sequence=sequence,
                timestamp=utcnow_rfc3339(),
                duration_ms=duration_ms,
                payload={
                    "model": model,
                    "system_prompt": _truncate(system_text, _MAX_SYSTEM),
                    "user_message": _truncate(user_message, _MAX_USER_MSG),
                    "status": "failed",
                    "exception_type": type(exc).__name__,
                    "exception_message": _truncate(str(exc), _MAX_EXC_MSG),
                },
            ))
            raise

        duration_ms = int((time.perf_counter() - start) * 1000)
        response_text, input_tokens, output_tokens = _extract_response_fields(response)

        client.submit_event(Event(
            event_id=event_id,
            execution_id=ctx.execution_id,
            event_type=EventType.LLM_CALL,
            sequence=sequence,
            timestamp=utcnow_rfc3339(),
            duration_ms=duration_ms,
            payload={
                "model": model,
                "system_prompt": _truncate(system_text, _MAX_SYSTEM),
                "user_message": _truncate(user_message, _MAX_USER_MSG),
                "response_text": _truncate(response_text, _MAX_RESPONSE),
                "status": "ok",
                "input_tokens": input_tokens,
                "output_tokens": output_tokens,
            },
        ))
        return response

    # Preserve the original name + docstring on the wrapper so
    # introspection (help(), repr()) still shows useful info.
    patched_create.__name__ = getattr(original_create, "__name__", "create")
    patched_create.__doc__ = getattr(original_create, "__doc__", None)

    messages_class.create = patched_create  # type: ignore[assignment]
    _patched_classes.add(messages_class)
    return True


def _extract_last_user_message(messages: List[Any]) -> str:
    """Pull the most recent user message text from an Anthropic-style messages list.

    The Anthropic API accepts both plain strings and content-block lists
    for each message. Handle both shapes; fall back to repr() on
    anything else so the event still has something useful to display
    even if the format is unexpected.
    """
    for msg in reversed(messages):
        if not isinstance(msg, dict):
            continue
        if msg.get("role") != "user":
            continue
        content = msg.get("content", "")
        if isinstance(content, str):
            return content
        if isinstance(content, list):
            text_parts: List[str] = []
            for block in content:
                if isinstance(block, dict) and block.get("type") == "text":
                    text_parts.append(str(block.get("text", "")))
            return "\n".join(text_parts)
        return repr(content)
    return ""


def _extract_response_fields(response: Any) -> tuple:
    """Extract (response_text, input_tokens, output_tokens) from an Anthropic Message.

    Defensive: a future-version Anthropic response could change shape.
    Failures in extraction degrade to empty/zero rather than crashing
    the wrapping function.
    """
    response_text = ""
    input_tokens = 0
    output_tokens = 0

    try:
        content = getattr(response, "content", None)
        if content:
            parts: List[str] = []
            for block in content:
                text = getattr(block, "text", None)
                if isinstance(text, str):
                    parts.append(text)
            response_text = "\n".join(parts)
    except Exception as exc:
        logger.debug("mesedi: response.content extraction failed: %s", exc)

    try:
        usage = getattr(response, "usage", None)
        if usage is not None:
            input_tokens = int(getattr(usage, "input_tokens", 0) or 0)
            output_tokens = int(getattr(usage, "output_tokens", 0) or 0)
    except Exception as exc:
        logger.debug("mesedi: response.usage extraction failed: %s", exc)

    return response_text, input_tokens, output_tokens


def _truncate(s: str, max_len: int) -> str:
    """Truncate a string with an ellipsis marker if it exceeds max_len."""
    if len(s) <= max_len:
        return s
    return s[: max_len - 3] + "..."
