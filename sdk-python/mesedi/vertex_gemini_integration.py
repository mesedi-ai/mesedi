"""
Vertex AI Gemini SDK monkey-patch — auto-emit llm_call events for
every ``GenerativeModel.generate_content`` (sync) and
``generate_content_async`` (async) call inside a ``@mesedi.wrap``
execution against the Vertex AI surface.

Enterprise Google customers use ``vertexai`` rather than
``google-generativeai`` because Vertex provides GCP-native auth
(service accounts, IAM), regional control, and the Enterprise-tier
SLA. The two SDKs ship different packages, different auth models,
and slightly different request/response shapes — so this lives in
its own integration with its own entry point.

Same provider tag (``provider="gemini"``) and same canonical
error_class map as ``instrument_gemini`` — a real Google outage
clusters into ONE provider_incident regardless of which Google
surface the customer was using. Customers running both surfaces
call ``instrument_gemini()`` and ``instrument_vertex_gemini()``;
each is idempotent and patches a different class.

Out of scope (filed as future follow-ups):

  - Streaming responses (``generate_content`` with ``stream=True``)
  - Embed surface — Vertex offers ``TextEmbeddingModel.get_embeddings``
    on a separate class hierarchy; tracked as future follow-up.

Dependency injection: ``instrument_vertex_gemini()`` accepts an
optional ``model_class`` parameter so this code path is testable
without installing the ``vertexai`` package.
"""

from __future__ import annotations

import logging
import time
import uuid
from typing import Any, Dict, Optional, Type

from mesedi._context import current_execution_context
from mesedi.client import get_client
from mesedi.errors import (
    classify_gemini_exception,
    extract_http_status,
    extract_retry_after,
)
from mesedi.events import Event, EventType, utcnow_rfc3339
from mesedi.observe import _maybe_emit_throttling_event

# Reuse the field extractors from the google-generativeai integration
# so Vertex + AI Studio behavior stays in lockstep. Request shapes
# are identical between the two surfaces (both accept Content lists);
# response shapes differ only in how ``usage_metadata`` is exposed,
# which our extractor already handles defensively.
from mesedi.gemini_integration import (
    _extract_user_message_from_contents,
    _stringify_gemini_content,
    _truncate,
)

_PROVIDER = "gemini"

logger = logging.getLogger("mesedi.vertex_gemini")

_MAX_SYSTEM = 1000
_MAX_USER_MSG = 1000
_MAX_RESPONSE = 1000
_MAX_EXC_MSG = 500

# Module-level idempotency registry keyed by class.
_patched_classes: set = set()


def instrument_vertex_gemini(
    model_class: Optional[Type[Any]] = None,
) -> bool:
    """Patch Vertex AI's GenerativeModel.generate_content (+ async)
    to emit llm_call events.

    Args:
        model_class: Class whose ``generate_content`` /
            ``generate_content_async`` methods to patch. When None
            (default), tries to import
            ``vertexai.generative_models.GenerativeModel``.

    Returns:
        True if at least one method (sync or async) was patched
        successfully OR was a no-op on a re-patch. False if neither
        vertexai is installed nor a class was provided.
    """
    if model_class is None:
        try:
            from vertexai.generative_models import GenerativeModel as _VGM
            model_class = _VGM
        except ImportError:
            logger.warning(
                "mesedi: vertexai package not importable; "
                "instrument_vertex_gemini() is a no-op. "
                "Install with `pip install google-cloud-aiplatform` "
                "to enable."
            )
            return False

    sync_ok = _patch_vertex_gemini_sync(model_class)
    async_ok = _patch_vertex_gemini_async(model_class)
    return sync_ok or async_ok


def _patch_vertex_gemini_sync(model_class: Type[Any]) -> bool:
    if model_class in _patched_classes:
        return True
    if not hasattr(model_class, "generate_content"):
        return False

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

        model = _resolve_model_name(self)
        system_text = _resolve_system_instruction(self)
        contents = args[0] if args else kwargs.get("contents")
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
            _maybe_emit_throttling_event(
                provider=_PROVIDER,
                error_class=classify_gemini_exception(exc),
                http_status=extract_http_status(exc),
                retry_after_seconds=extract_retry_after(exc),
                endpoint="/v1/projects/*/locations/*/publishers/google/models/*:generateContent",
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
                "surface": "chat",
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


def _patch_vertex_gemini_async(model_class: Type[Any]) -> bool:
    """Async twin — vertexai GenerativeModel.generate_content_async."""
    if not hasattr(model_class, "generate_content_async"):
        return False

    original_generate = model_class.generate_content_async

    async def patched_generate(self: Any, *args: Any, **kwargs: Any) -> Any:
        ctx = current_execution_context()
        if ctx is None:
            return await original_generate(self, *args, **kwargs)
        ctx.check_budget()

        client = get_client()
        sequence = ctx.next_sequence()
        event_id = f"evt-{uuid.uuid4().hex[:12]}"
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.increment_steps()

        model = _resolve_model_name(self)
        system_text = _resolve_system_instruction(self)
        contents = args[0] if args else kwargs.get("contents")
        user_message = _extract_user_message_from_contents(contents)

        start = time.perf_counter()
        try:
            response = await original_generate(self, *args, **kwargs)
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
            _maybe_emit_throttling_event(
                provider=_PROVIDER,
                error_class=classify_gemini_exception(exc),
                http_status=extract_http_status(exc),
                retry_after_seconds=extract_retry_after(exc),
                endpoint="/v1/projects/*/locations/*/publishers/google/models/*:generateContent",
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
                "surface": "chat",
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

    patched_generate.__name__ = getattr(
        original_generate, "__name__", "generate_content_async",
    )
    patched_generate.__doc__ = getattr(original_generate, "__doc__", None)
    model_class.generate_content_async = patched_generate  # type: ignore[assignment]
    _patched_classes.add(model_class)
    return True


def _resolve_model_name(self: Any) -> str:
    """Vertex GenerativeModel stores the model id on a few candidate
    attributes depending on SDK version. Walk them defensively."""
    for attr in ("_model_name", "model_name", "_model_id", "model"):
        v = getattr(self, attr, None)
        if isinstance(v, str) and v:
            return v
    return "unknown"


def _resolve_system_instruction(self: Any) -> str:
    """Pull the system_instruction the model was constructed with.
    Vertex exposes it on ``_system_instruction`` (private) and on
    ``system_instruction`` in newer versions."""
    for attr in ("_system_instruction", "system_instruction"):
        v = getattr(self, attr, None)
        if v is None:
            continue
        if isinstance(v, str):
            return v
        try:
            return _stringify_gemini_content(v)
        except Exception:
            continue
    return ""


def _extract_response_fields(response: Any) -> tuple:
    """Vertex response exposes ``text`` (string) and ``usage_metadata``
    with ``prompt_token_count`` + ``candidates_token_count``. Both
    accesses are defensive — unexpected shapes degrade to empty/zero."""
    response_text = ""
    input_tokens = 0
    output_tokens = 0
    try:
        text = getattr(response, "text", None)
        if isinstance(text, str):
            response_text = text
    except Exception as exc:
        logger.debug("mesedi: vertex gemini text extraction failed: %s", exc)

    try:
        usage = getattr(response, "usage_metadata", None)
        if usage is not None:
            input_tokens = int(getattr(usage, "prompt_token_count", 0) or 0)
            output_tokens = int(getattr(usage, "candidates_token_count", 0) or 0)
    except Exception as exc:
        logger.debug("mesedi: vertex gemini usage extraction failed: %s", exc)

    return response_text, input_tokens, output_tokens


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
        "surface": "chat",
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
