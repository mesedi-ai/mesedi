"""
OpenAI SDK monkey-patch — auto-emit llm_call events for every
``Completions.create`` and ``Responses.create`` call inside a
``@mesedi.wrap`` execution.

Activation is **opt-in**: call ``mesedi.instrument_openai()`` once at
process startup. Mirrors ``mesedi.instrument_anthropic()`` — same
fail-open semantics, same canonical error vocabulary, same idempotency
guarantee.

What gets captured per call:

  - ``provider`` = ``"openai"`` (stable, lowercase identifier; used
    by the backend's provider_incident detector to cluster cross-
    tenant signals by (provider, error_class))
  - ``model`` (e.g. "gpt-4o", "gpt-4o-mini", "o1-preview")
  - ``system_prompt``, the FIRST ``role="system"`` message content,
    truncated to 1000 chars (OpenAI puts system messages inside the
    ``messages`` array, unlike Anthropic's separate ``system=``
    parameter)
  - ``user_message``, the LAST ``role="user"`` message content,
    truncated to 1000 chars
  - ``response_text``, ``choices[0].message.content`` for chat
    completions OR ``output_text`` for the Responses API, truncated
    to 1000 chars
  - ``input_tokens`` / ``output_tokens``, from ``response.usage`` —
    OpenAI names these ``prompt_tokens`` / ``completion_tokens``;
    we translate to the canonical names the backend expects
  - ``duration_ms``, wall-clock time of the API call
  - ``status``, "ok" if the call returned, "failed" if it raised
  - ``exception_type`` / ``exception_message``, on failure
  - ``error_class``, canonical 8-class vocabulary (see
    :mod:`mesedi.errors`) — classify_openai_exception handles the
    OpenAI quirk that RateLimitError covers BOTH true rate-limiting
    AND insufficient_quota; the body is probed to distinguish them
  - ``http_status``, on failure when the exception exposes it
  - ``retry_after_seconds``, on failure when the Retry-After header
    or exception attribute is present

What this module patches:

  - ``openai.resources.chat.completions.Completions.create`` —
    the dominant chat-completions surface
  - ``openai.resources.responses.Responses.create`` — the newer
    Responses API surface (added in openai>=1.40)

Out of scope (filed as follow-ups):

  - Async clients (``AsyncOpenAI``). Same gap exists for
    ``instrument_anthropic``; a coordinated async-support sweep
    closes both at once.
  - Streaming responses (``stream=True``). Both Anthropic and
    OpenAI streaming need a separate observer that drains chunks
    without buffering the whole response in memory.
  - Embeddings / image generation / audio. The provider_incident
    detector is most valuable on chat-completions; other endpoints
    can be added as customer demand surfaces.

Patching is idempotent per class object.

Dependency injection: ``instrument_openai()`` accepts optional
``completions_class`` + ``responses_class`` parameters so this code
path is testable without installing the actual ``openai`` package.
Production callers leave both as ``None`` and let the function auto-
locate the real classes.
"""

from __future__ import annotations

import logging
import time
import uuid
from typing import Any, Dict, List, Optional, Tuple, Type

from mesedi._context import current_execution_context
from mesedi.client import get_client
from mesedi.errors import (
    classify_openai_exception,
    extract_http_status,
    extract_retry_after,
)
from mesedi.events import Event, EventType, utcnow_rfc3339

# Stable lowercase provider identifier shipped on every llm_call
# event emitted by this integration. The backend's provider_incident
# detector clusters cross-tenant signals on (provider, error_class)
# so this string must NOT change between SDK versions without a
# coordinated backend change.
_PROVIDER = "openai"

logger = logging.getLogger("mesedi.openai")

# Truncation budgets. Matches anthropic_integration so payload sizes
# stay comparable across providers.
_MAX_SYSTEM = 1000
_MAX_USER_MSG = 1000
_MAX_RESPONSE = 1000
_MAX_EXC_MSG = 500

# Module-level "already patched" registry keyed by class object so
# distinct fake classes injected in tests don't falsely trip the
# idempotency check.
_patched_classes: set = set()


def instrument_openai(
    completions_class: Optional[Type[Any]] = None,
    responses_class: Optional[Type[Any]] = None,
) -> bool:
    """Patch the OpenAI SDK's chat-completions and Responses APIs to
    emit llm_call events.

    Args:
        completions_class: Class whose ``create`` method handles chat
            completions. When ``None`` (default), tries to import
            ``openai.resources.chat.completions.Completions``.
        responses_class: Class whose ``create`` method handles the
            Responses API. When ``None``, tries to import
            ``openai.resources.responses.Responses``. The Responses
            API was added in openai>=1.40; older installations skip
            this patch (logged at debug level, not warning, because
            it's a clean no-op for the older surface).

    Returns:
        True if at least one of the classes was successfully patched
        (including the case where it was already patched on a prior
        call). False if neither could be located AND the openai
        package is not importable.
    """
    patched_any = False

    if completions_class is None:
        try:
            from openai.resources.chat.completions import (
                Completions as _Completions,
            )
            completions_class = _Completions
        except ImportError:
            logger.warning(
                "mesedi: openai package not importable; "
                "instrument_openai() chat.completions patch is a no-op. "
                "Install with `pip install openai` to enable."
            )

    if completions_class is not None:
        _patch_chat_completions(completions_class)
        patched_any = True

    if responses_class is None:
        try:
            from openai.resources.responses import Responses as _Responses
            responses_class = _Responses
        except ImportError:
            # Responses API requires openai>=1.40. Older installations
            # just don't get this surface patched, which is correct
            # (there's nothing to patch).
            logger.debug(
                "mesedi: openai.resources.responses not importable; "
                "skipping Responses API patch (requires openai>=1.40)."
            )

    if responses_class is not None:
        _patch_responses(responses_class)
        patched_any = True

    return patched_any


def _patch_chat_completions(cls: Type[Any]) -> None:
    """Wrap cls.create to emit chat-completions llm_call events."""
    if cls in _patched_classes:
        return

    original_create = cls.create

    def patched_create(self: Any, *args: Any, **kwargs: Any) -> Any:
        ctx = current_execution_context()
        if ctx is None:
            return original_create(self, *args, **kwargs)
        ctx.check_budget()

        client = get_client()
        sequence = ctx.next_sequence()
        event_id = f"evt-{uuid.uuid4().hex[:12]}"
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.increment_steps()

        model = kwargs.get("model", "unknown")
        messages = kwargs.get("messages", []) or []
        system_text = _extract_first_system_message(messages)
        user_message = _extract_last_user_message(messages)

        start = time.perf_counter()
        try:
            response = original_create(self, *args, **kwargs)
        except BaseException as exc:
            duration_ms = int((time.perf_counter() - start) * 1000)
            client.submit_event(_build_failure_event(
                event_id=event_id,
                execution_id=ctx.execution_id,
                sequence=sequence,
                duration_ms=duration_ms,
                model=model,
                system_text=system_text,
                user_message=user_message,
                exc=exc,
            ))
            raise

        duration_ms = int((time.perf_counter() - start) * 1000)
        response_text, input_tokens, output_tokens = (
            _extract_chat_response_fields(response)
        )
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.add_tokens(
                tokens_in=input_tokens, tokens_out=output_tokens,
            )
        client.submit_event(Event(
            event_id=event_id,
            execution_id=ctx.execution_id,
            event_type=EventType.LLM_CALL,
            sequence=sequence,
            timestamp=utcnow_rfc3339(),
            duration_ms=duration_ms,
            payload={
                "provider": _PROVIDER,
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

    patched_create.__name__ = getattr(original_create, "__name__", "create")
    patched_create.__doc__ = getattr(original_create, "__doc__", None)
    cls.create = patched_create  # type: ignore[assignment]
    _patched_classes.add(cls)


def _patch_responses(cls: Type[Any]) -> None:
    """Wrap cls.create to emit Responses-API llm_call events.

    Responses API has a different request/response shape than chat
    completions:

      request: input=str OR input=[{"role":..., "content":...}, ...]
      response: response.output_text (string), response.usage.input_tokens,
                response.usage.output_tokens
    """
    if cls in _patched_classes:
        return

    original_create = cls.create

    def patched_create(self: Any, *args: Any, **kwargs: Any) -> Any:
        ctx = current_execution_context()
        if ctx is None:
            return original_create(self, *args, **kwargs)
        ctx.check_budget()

        client = get_client()
        sequence = ctx.next_sequence()
        event_id = f"evt-{uuid.uuid4().hex[:12]}"
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.increment_steps()

        model = kwargs.get("model", "unknown")
        instructions = kwargs.get("instructions", "") or ""
        input_field = kwargs.get("input", "")
        user_message, system_from_input = _extract_responses_input(input_field)
        # `instructions` is the Responses API equivalent of system
        # prompt; if both are present, the explicit `instructions`
        # parameter wins because it's the canonical system slot.
        system_text = instructions or system_from_input

        start = time.perf_counter()
        try:
            response = original_create(self, *args, **kwargs)
        except BaseException as exc:
            duration_ms = int((time.perf_counter() - start) * 1000)
            client.submit_event(_build_failure_event(
                event_id=event_id,
                execution_id=ctx.execution_id,
                sequence=sequence,
                duration_ms=duration_ms,
                model=model,
                system_text=system_text,
                user_message=user_message,
                exc=exc,
            ))
            raise

        duration_ms = int((time.perf_counter() - start) * 1000)
        response_text, input_tokens, output_tokens = (
            _extract_responses_response_fields(response)
        )
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.add_tokens(
                tokens_in=input_tokens, tokens_out=output_tokens,
            )
        client.submit_event(Event(
            event_id=event_id,
            execution_id=ctx.execution_id,
            event_type=EventType.LLM_CALL,
            sequence=sequence,
            timestamp=utcnow_rfc3339(),
            duration_ms=duration_ms,
            payload={
                "provider": _PROVIDER,
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

    patched_create.__name__ = getattr(original_create, "__name__", "create")
    patched_create.__doc__ = getattr(original_create, "__doc__", None)
    cls.create = patched_create  # type: ignore[assignment]
    _patched_classes.add(cls)


def _build_failure_event(
    *,
    event_id: str,
    execution_id: str,
    sequence: int,
    duration_ms: int,
    model: str,
    system_text: str,
    user_message: str,
    exc: BaseException,
) -> Event:
    """Construct the shared failure-path llm_call event from an OpenAI
    exception. Same shape as anthropic_integration's failure payload —
    backend detectors fingerprint on the canonical fields, not on
    per-provider quirks.
    """
    failure_payload: Dict[str, Any] = {
        "provider": _PROVIDER,
        "model": model,
        "system_prompt": _truncate(system_text, _MAX_SYSTEM),
        "user_message": _truncate(user_message, _MAX_USER_MSG),
        "status": "failed",
        "error_class": classify_openai_exception(exc),
        "exception_type": type(exc).__name__,
        "exception_message": _truncate(str(exc), _MAX_EXC_MSG),
    }
    http_status = extract_http_status(exc)
    if http_status is not None:
        failure_payload["http_status"] = http_status
    retry_after = extract_retry_after(exc)
    if retry_after is not None:
        failure_payload["retry_after_seconds"] = retry_after
    return Event(
        event_id=event_id,
        execution_id=execution_id,
        event_type=EventType.LLM_CALL,
        sequence=sequence,
        timestamp=utcnow_rfc3339(),
        duration_ms=duration_ms,
        payload=failure_payload,
    )


def _extract_first_system_message(messages: List[Any]) -> str:
    """Pull the FIRST role=system message's content from an
    OpenAI-style messages list. OpenAI puts the system prompt inside
    the messages array (no separate `system=` parameter like
    Anthropic). Returns empty string when no system message is
    present.
    """
    for msg in messages:
        if not isinstance(msg, dict):
            continue
        if msg.get("role") != "system":
            continue
        content = msg.get("content", "")
        if isinstance(content, str):
            return content
        if isinstance(content, list):
            # Multi-part system content (e.g. text + image). Stitch
            # text parts together; ignore non-text blocks.
            return "\n".join(
                str(b.get("text", ""))
                for b in content
                if isinstance(b, dict) and b.get("type") == "text"
            )
        return repr(content)
    return ""


def _extract_last_user_message(messages: List[Any]) -> str:
    """Pull the MOST RECENT role=user message's content. Walks
    backwards so multi-turn conversations report the latest prompt
    (matching Anthropic instrument).
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
            return "\n".join(
                str(b.get("text", ""))
                for b in content
                if isinstance(b, dict) and b.get("type") == "text"
            )
        return repr(content)
    return ""


def _extract_responses_input(
    input_field: Any,
) -> Tuple[str, str]:
    """For the Responses API: extract (user_message, system_text)
    from the polymorphic ``input`` parameter.

    The Responses API accepts:
      - A plain string (most common one-shot use): treated as the
        entire user message; no system text.
      - A list of {"role": ..., "content": ...} dicts: walked the
        same way chat-completions messages are.

    Returns (user_message, system_text). Either may be empty.
    """
    if isinstance(input_field, str):
        return input_field, ""
    if isinstance(input_field, list):
        user_message = _extract_last_user_message(input_field)
        system_text = _extract_first_system_message(input_field)
        return user_message, system_text
    # Unexpected shape — fall back to repr so SOMETHING gets recorded.
    return repr(input_field), ""


def _extract_chat_response_fields(response: Any) -> Tuple[str, int, int]:
    """Extract (response_text, input_tokens, output_tokens) from an
    OpenAI ChatCompletion response. Defensive: any extraction failure
    degrades to empty/zero rather than crashing the wrapped call.

    OpenAI token field names are ``prompt_tokens`` / ``completion_tokens``;
    we translate to the canonical ``input_tokens`` / ``output_tokens``
    the backend expects.
    """
    response_text = ""
    input_tokens = 0
    output_tokens = 0

    try:
        choices = getattr(response, "choices", None)
        if choices:
            first = choices[0]
            message = getattr(first, "message", None)
            if message is not None:
                text = getattr(message, "content", None)
                if isinstance(text, str):
                    response_text = text
    except Exception as exc:
        logger.debug("mesedi: openai chat response extraction failed: %s", exc)

    try:
        usage = getattr(response, "usage", None)
        if usage is not None:
            input_tokens = int(getattr(usage, "prompt_tokens", 0) or 0)
            output_tokens = int(getattr(usage, "completion_tokens", 0) or 0)
    except Exception as exc:
        logger.debug("mesedi: openai chat usage extraction failed: %s", exc)

    return response_text, input_tokens, output_tokens


def _extract_responses_response_fields(response: Any) -> Tuple[str, int, int]:
    """Extract (response_text, input_tokens, output_tokens) from an
    OpenAI Responses API response.

    Responses API uses ``output_text`` (concatenated convenience
    accessor) AND ``output`` (the full block list). Prefer
    ``output_text`` since it's the SDK's own canonical string view;
    fall back to assembling from output blocks if absent.

    Token field names on the Responses API are ``input_tokens`` /
    ``output_tokens`` — already canonical, no translation needed.
    """
    response_text = ""
    input_tokens = 0
    output_tokens = 0

    try:
        text = getattr(response, "output_text", None)
        if isinstance(text, str):
            response_text = text
        else:
            # Older SDK shapes or test fakes may expose `output` only.
            output = getattr(response, "output", None)
            if output:
                parts: List[str] = []
                for block in output:
                    content_blocks = getattr(block, "content", None) or []
                    for cb in content_blocks:
                        t = getattr(cb, "text", None)
                        if isinstance(t, str):
                            parts.append(t)
                response_text = "\n".join(parts)
    except Exception as exc:
        logger.debug("mesedi: openai responses extraction failed: %s", exc)

    try:
        usage = getattr(response, "usage", None)
        if usage is not None:
            input_tokens = int(getattr(usage, "input_tokens", 0) or 0)
            output_tokens = int(getattr(usage, "output_tokens", 0) or 0)
    except Exception as exc:
        logger.debug("mesedi: openai responses usage extraction failed: %s", exc)

    return response_text, input_tokens, output_tokens


def _truncate(s: str, max_len: int) -> str:
    if len(s) <= max_len:
        return s
    return s[: max_len - 3] + "..."
