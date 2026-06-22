"""
Google Gemini SDK monkey-patch — auto-emit llm_call events for every
``GenerativeModel.generate_content`` call inside a ``@mesedi.wrap``
execution.

Mirrors the other provider integrations (anthropic, openai, cohere):
same fail-open semantics, same canonical error vocabulary, same
idempotency. Targets the ``google-generativeai`` package
(>=0.5; the dominant non-Vertex Gemini Python surface).

What gets captured:

  - ``provider`` = ``"gemini"``
  - ``model``, extracted from ``GenerativeModel.model_name``
  - ``system_prompt``, ``GenerativeModel.system_instruction`` (or a
    role:system-equivalent in the contents list), truncated
  - ``user_message``, the last role=user entry in the contents
    list — Gemini accepts either a string OR a list of message
    dicts/Parts; both shapes are handled.
  - ``response_text``, ``response.text`` (the canonical accessor)
  - ``input_tokens`` / ``output_tokens``, from
    ``response.usage_metadata.prompt_token_count`` /
    ``candidates_token_count``
  - ``status``, ``error_class``, ``http_status``, ``retry_after_seconds``
    on failure paths

Out of scope (filed as #271 follow-ups):

  - Async clients (``GenerativeModel.generate_content_async``)
  - Streaming (``stream=True`` / ``generate_content_stream``)
  - Vertex AI surface (``vertexai.generative_models.GenerativeModel``)
    — different package, different exception hierarchy
  - Chat sessions (``model.start_chat()``)

Dependency injection: ``instrument_gemini()`` accepts an optional
``model_class`` parameter so this code path is testable without
installing the google-generativeai package.
"""

from __future__ import annotations

import logging
import time
import uuid
from typing import Any, Dict, List, Optional, Tuple, Type

from mesedi._context import current_execution_context
from mesedi.client import get_client
from mesedi.errors import (
    classify_gemini_exception,
    extract_http_status,
    extract_retry_after,
)
from mesedi.events import Event, EventType, utcnow_rfc3339
from mesedi.observe import _maybe_emit_throttling_event

_PROVIDER = "gemini"

logger = logging.getLogger("mesedi.gemini")

_MAX_SYSTEM = 1000
_MAX_USER_MSG = 1000
_MAX_RESPONSE = 1000
_MAX_EXC_MSG = 500

_patched_classes: set = set()


def instrument_gemini(model_class: Optional[Type[Any]] = None) -> bool:
    """Patch GenerativeModel.generate_content to emit llm_call events.

    Args:
        model_class: Class whose ``generate_content`` method to patch.
            When None (default), tries to import
            ``google.generativeai.GenerativeModel``.

    Returns:
        True if patching succeeded (or was a no-op on a re-patch).
        False if neither google-generativeai is installed nor a
        class was provided.
    """
    if model_class is None:
        try:
            from google.generativeai import GenerativeModel as _GM
            model_class = _GM
        except ImportError:
            logger.warning(
                "mesedi: google-generativeai not importable; "
                "instrument_gemini() is a no-op. "
                "Install with `pip install google-generativeai` to enable."
            )
            return False

    if model_class in _patched_classes:
        return True

    original_generate = model_class.generate_content

    def patched_generate(self: Any, *args: Any, **kwargs: Any) -> Any:
        ctx = current_execution_context()
        if ctx is None:
            return original_generate(self, *args, **kwargs)
        ctx.check_budget()

        client = get_client()
        sequence = ctx.next_sequence()
        event_id = f"evt-{uuid.uuid4().hex[:12]}"
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.increment_steps()

        model = getattr(self, "model_name", None) or kwargs.get("model", "unknown")
        if not isinstance(model, str):
            model = str(model)
        # system_instruction is an attribute on GenerativeModel; not
        # all instances set it. ``getattr`` with default keeps the
        # extraction defensive.
        system_text = getattr(self, "_system_instruction", "") or getattr(
            self, "system_instruction", ""
        ) or ""
        if not isinstance(system_text, str):
            # Gemini sometimes stores system_instruction as a Content
            # object; stringify defensively.
            system_text = _stringify_gemini_content(system_text)

        # First positional or kwarg ``contents``: can be a str OR a
        # list of message dicts / Content objects.
        contents = args[0] if args else kwargs.get("contents", "")
        user_message = _extract_user_message_from_contents(contents)

        start = time.perf_counter()
        try:
            response = original_generate(self, *args, **kwargs)
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
            # Auto-emit infrastructure_event on throttling-class
            # exceptions (Wave 1.4) so infrastructure_throttled
            # isn't silently inactive for the default customer.
            _maybe_emit_throttling_event(
                provider=_PROVIDER,
                error_class=classify_gemini_exception(exc),
                http_status=extract_http_status(exc),
                retry_after_seconds=extract_retry_after(exc),
                endpoint="/v1/models/generateContent",
            )
            raise

        duration_ms = int((time.perf_counter() - start) * 1000)
        response_text, input_tokens, output_tokens = _extract_response_fields(response)
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

    patched_generate.__name__ = getattr(original_generate, "__name__", "generate_content")
    patched_generate.__doc__ = getattr(original_generate, "__doc__", None)
    model_class.generate_content = patched_generate  # type: ignore[assignment]
    _patched_classes.add(model_class)
    return True


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
    failure_payload: Dict[str, Any] = {
        "provider": _PROVIDER,
        "model": model,
        "system_prompt": _truncate(system_text, _MAX_SYSTEM),
        "user_message": _truncate(user_message, _MAX_USER_MSG),
        "status": "failed",
        "error_class": classify_gemini_exception(exc),
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


def _extract_user_message_from_contents(contents: Any) -> str:
    """Gemini ``contents`` is polymorphic: str OR list of (str | dict
    with ``role`` + ``parts`` | google.ai.Content objects). Walk for
    the LAST role=user entry; fall back to repr for unrecognized
    shapes."""
    if isinstance(contents, str):
        return contents
    if isinstance(contents, list):
        for item in reversed(contents):
            if isinstance(item, str):
                return item
            if isinstance(item, dict):
                if item.get("role") and item["role"] != "user":
                    continue
                parts = item.get("parts", [])
                if isinstance(parts, list):
                    text_parts: List[str] = []
                    for p in parts:
                        if isinstance(p, str):
                            text_parts.append(p)
                        elif isinstance(p, dict) and isinstance(p.get("text"), str):
                            text_parts.append(p["text"])
                    return "\n".join(text_parts)
                if isinstance(parts, str):
                    return parts
            # google.ai.generativelanguage.Content protobuf shape.
            role = getattr(item, "role", None)
            if role and role != "user":
                continue
            return _stringify_gemini_content(item)
        return ""
    return _stringify_gemini_content(contents)


def _stringify_gemini_content(content: Any) -> str:
    """Best-effort string from a Gemini Content-like object. Probes
    ``.text``, ``.parts[*].text``, then falls back to repr()."""
    if content is None:
        return ""
    text = getattr(content, "text", None)
    if isinstance(text, str):
        return text
    parts = getattr(content, "parts", None)
    if parts:
        bits: List[str] = []
        try:
            for p in parts:
                t = getattr(p, "text", None)
                if isinstance(t, str):
                    bits.append(t)
            if bits:
                return "\n".join(bits)
        except TypeError:
            pass
    return repr(content)


def _extract_response_fields(response: Any) -> Tuple[str, int, int]:
    """Pull (text, prompt_tokens, candidates_tokens) from a Gemini
    GenerateContentResponse. Defensive: any extraction failure
    degrades to empty/zero rather than crashing the wrapper."""
    response_text = ""
    input_tokens = 0
    output_tokens = 0
    try:
        text = getattr(response, "text", None)
        if isinstance(text, str):
            response_text = text
    except Exception as exc:
        logger.debug("mesedi: gemini text extraction failed: %s", exc)
    try:
        usage = getattr(response, "usage_metadata", None)
        if usage is not None:
            input_tokens = int(getattr(usage, "prompt_token_count", 0) or 0)
            output_tokens = int(
                getattr(usage, "candidates_token_count", 0) or 0
            )
    except Exception as exc:
        logger.debug("mesedi: gemini usage extraction failed: %s", exc)
    return response_text, input_tokens, output_tokens


def _truncate(s: str, max_len: int) -> str:
    if len(s) <= max_len:
        return s
    return s[: max_len - 3] + "..."
