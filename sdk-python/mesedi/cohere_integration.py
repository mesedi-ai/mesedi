"""
Cohere SDK monkey-patch — auto-emit llm_call events for every chat
call (sync ``Client.chat`` v1 + ``ClientV2.chat`` v5+) inside a
``@mesedi.wrap`` execution.

Mirrors :mod:`mesedi.anthropic_integration` and
:mod:`mesedi.openai_integration` in fail-open semantics, canonical
error vocabulary, idempotency. Cohere has two SDK surfaces that we
patch:

  - ``cohere.Client`` (v1, ``message="..."`` + ``chat_history=[...]``)
  - ``cohere.ClientV2`` (v5+, ``messages=[{"role":..., "content":...}]``)

Both write the same canonical llm_call payload (``provider="cohere"``)
so the backend doesn't need to know which API surface fired.

Async coverage (#271.h):

  - ``cohere.AsyncClient`` (v1) and ``cohere.AsyncClientV2`` (v2)
    are also patched by ``instrument_cohere()``. Mirrors the sync
    wrappers: same canonical error_class mapping, same retry_after
    extraction, same _maybe_emit_throttling_event auto-emit.

Still out of scope (filed under #271 follow-ups):

  - Streaming responses (``chat_stream``) — patched in #271.i.
  - RAG endpoints (``rerank``, ``embed``) — provider_incident is
    most valuable on chat surfaces

Dependency injection: ``instrument_cohere()`` accepts optional
``client_v1_class`` + ``client_v2_class`` parameters so this code
path is testable without installing the cohere package.
"""

from __future__ import annotations

import logging
import time
import uuid
from typing import Any, Dict, List, Optional, Tuple, Type

from mesedi._context import current_execution_context
from mesedi.client import get_client
from mesedi.errors import (
    classify_cohere_exception,
    extract_http_status,
    extract_retry_after,
)
from mesedi.events import Event, EventType, utcnow_rfc3339
from mesedi.observe import _maybe_emit_throttling_event

_PROVIDER = "cohere"

logger = logging.getLogger("mesedi.cohere")

_MAX_SYSTEM = 1000
_MAX_USER_MSG = 1000
_MAX_RESPONSE = 1000
_MAX_EXC_MSG = 500

_patched_classes: set = set()


def instrument_cohere(
    client_v1_class: Optional[Type[Any]] = None,
    client_v2_class: Optional[Type[Any]] = None,
    async_client_v1_class: Optional[Type[Any]] = None,
    async_client_v2_class: Optional[Type[Any]] = None,
) -> bool:
    """Patch Cohere SDK chat methods (sync + async) to emit llm_call events.

    Args:
        client_v1_class: legacy ``cohere.Client`` class.
        client_v2_class: modern ``cohere.ClientV2`` class.
        async_client_v1_class: legacy ``cohere.AsyncClient`` class (#271.h).
        async_client_v2_class: modern ``cohere.AsyncClientV2`` class (#271.h).

    Returns True if at least one surface patched. False only if none
    could be located AND cohere isn't installed.
    """
    patched_any = False

    if client_v1_class is None:
        try:
            from cohere import Client as _ClientV1
            client_v1_class = _ClientV1
        except ImportError:
            logger.debug(
                "mesedi: cohere.Client not importable; skipping v1 patch."
            )
        except AttributeError:
            logger.debug("mesedi: cohere.Client not found on cohere module.")

    if client_v2_class is None:
        try:
            from cohere import ClientV2 as _ClientV2
            client_v2_class = _ClientV2
        except ImportError:
            logger.warning(
                "mesedi: cohere package not importable; "
                "instrument_cohere() is a no-op. "
                "Install with `pip install cohere` to enable."
            )
            return False
        except AttributeError:
            logger.debug(
                "mesedi: cohere.ClientV2 not found on cohere module "
                "(requires cohere>=5.0)."
            )

    if client_v1_class is not None:
        _patch_v1(client_v1_class)
        patched_any = True
    if client_v2_class is not None:
        _patch_v2(client_v2_class)
        patched_any = True

    # #271.h: async-aware patching.
    if async_client_v1_class is None:
        try:
            from cohere import AsyncClient as _AsyncClientV1
            async_client_v1_class = _AsyncClientV1
        except (ImportError, AttributeError):
            logger.debug(
                "mesedi: cohere.AsyncClient not importable; skipping async v1 patch."
            )

    if async_client_v2_class is None:
        try:
            from cohere import AsyncClientV2 as _AsyncClientV2
            async_client_v2_class = _AsyncClientV2
        except (ImportError, AttributeError):
            logger.debug(
                "mesedi: cohere.AsyncClientV2 not importable; skipping async v2 patch."
            )

    if async_client_v1_class is not None:
        _patch_async_v1(async_client_v1_class)
        patched_any = True
    if async_client_v2_class is not None:
        _patch_async_v2(async_client_v2_class)
        patched_any = True

    return patched_any


def _patch_v1(cls: Type[Any]) -> None:
    """Wrap cls.chat — v1 ``message=...``/``chat_history=...`` shape."""
    if cls in _patched_classes:
        return
    original_chat = cls.chat

    def patched_chat(self: Any, *args: Any, **kwargs: Any) -> Any:
        ctx = current_execution_context()
        if ctx is None:
            return original_chat(self, *args, **kwargs)
        ctx.check_budget()

        client = get_client()
        sequence = ctx.next_sequence()
        event_id = f"evt-{uuid.uuid4().hex[:12]}"
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.increment_steps()

        model = kwargs.get("model", "unknown")
        # v1 sends a single message= string + optional chat_history.
        user_message = kwargs.get("message", "") or ""
        if not isinstance(user_message, str):
            user_message = str(user_message)
        # v1 has no separate system slot; the preamble= parameter is
        # the closest equivalent.
        system_text = kwargs.get("preamble", "") or ""
        if not isinstance(system_text, str):
            system_text = str(system_text)

        start = time.perf_counter()
        try:
            response = original_chat(self, *args, **kwargs)
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
                error_class=classify_cohere_exception(exc),
                http_status=extract_http_status(exc),
                retry_after_seconds=extract_retry_after(exc),
                endpoint="/v1/chat",
            )
            raise

        duration_ms = int((time.perf_counter() - start) * 1000)
        response_text, input_tokens, output_tokens = _extract_v1_fields(response)
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.add_tokens(
                tokens_in=input_tokens, tokens_out=output_tokens,
            )
        client.submit_event(_success_event(
            event_id=event_id,
            execution_id=ctx.execution_id,
            sequence=sequence,
            duration_ms=duration_ms,
            model=model,
            system_text=system_text,
            user_message=user_message,
            response_text=response_text,
            input_tokens=input_tokens,
            output_tokens=output_tokens,
        ))
        return response

    patched_chat.__name__ = getattr(original_chat, "__name__", "chat")
    patched_chat.__doc__ = getattr(original_chat, "__doc__", None)
    cls.chat = patched_chat  # type: ignore[assignment]
    _patched_classes.add(cls)


def _patch_v2(cls: Type[Any]) -> None:
    """Wrap cls.chat — v2 ``messages=[...]`` shape (OpenAI-style)."""
    if cls in _patched_classes:
        return
    original_chat = cls.chat

    def patched_chat(self: Any, *args: Any, **kwargs: Any) -> Any:
        ctx = current_execution_context()
        if ctx is None:
            return original_chat(self, *args, **kwargs)
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
            response = original_chat(self, *args, **kwargs)
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
            # exceptions (Wave 1.4); same rationale as the v1 chat
            # path above.
            _maybe_emit_throttling_event(
                provider=_PROVIDER,
                error_class=classify_cohere_exception(exc),
                http_status=extract_http_status(exc),
                retry_after_seconds=extract_retry_after(exc),
                endpoint="/v2/chat",
            )
            raise

        duration_ms = int((time.perf_counter() - start) * 1000)
        response_text, input_tokens, output_tokens = _extract_v2_fields(response)
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.add_tokens(
                tokens_in=input_tokens, tokens_out=output_tokens,
            )
        client.submit_event(_success_event(
            event_id=event_id,
            execution_id=ctx.execution_id,
            sequence=sequence,
            duration_ms=duration_ms,
            model=model,
            system_text=system_text,
            user_message=user_message,
            response_text=response_text,
            input_tokens=input_tokens,
            output_tokens=output_tokens,
        ))
        return response

    patched_chat.__name__ = getattr(original_chat, "__name__", "chat")
    patched_chat.__doc__ = getattr(original_chat, "__doc__", None)
    cls.chat = patched_chat  # type: ignore[assignment]
    _patched_classes.add(cls)


def _patch_async_v1(cls: Type[Any]) -> None:
    """Async twin of _patch_v1 — wraps AsyncClient.chat (#271.h)."""
    if cls in _patched_classes:
        return
    original_chat = cls.chat

    async def patched_chat(self: Any, *args: Any, **kwargs: Any) -> Any:
        ctx = current_execution_context()
        if ctx is None:
            return await original_chat(self, *args, **kwargs)
        ctx.check_budget()

        client = get_client()
        sequence = ctx.next_sequence()
        event_id = f"evt-{uuid.uuid4().hex[:12]}"
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.increment_steps()

        model = kwargs.get("model", "unknown")
        user_message = kwargs.get("message", "") or ""
        if not isinstance(user_message, str):
            user_message = str(user_message)
        system_text = kwargs.get("preamble", "") or ""
        if not isinstance(system_text, str):
            system_text = str(system_text)

        start = time.perf_counter()
        try:
            response = await original_chat(self, *args, **kwargs)
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
                error_class=classify_cohere_exception(exc),
                http_status=extract_http_status(exc),
                retry_after_seconds=extract_retry_after(exc),
                endpoint="/v1/chat",
            )
            raise

        duration_ms = int((time.perf_counter() - start) * 1000)
        response_text, input_tokens, output_tokens = _extract_v1_fields(response)
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.add_tokens(
                tokens_in=input_tokens, tokens_out=output_tokens,
            )
        client.submit_event(_success_event(
            event_id=event_id,
            execution_id=ctx.execution_id,
            sequence=sequence,
            duration_ms=duration_ms,
            model=model,
            system_text=system_text,
            user_message=user_message,
            response_text=response_text,
            input_tokens=input_tokens,
            output_tokens=output_tokens,
        ))
        return response

    patched_chat.__name__ = getattr(original_chat, "__name__", "chat")
    patched_chat.__doc__ = getattr(original_chat, "__doc__", None)
    cls.chat = patched_chat  # type: ignore[assignment]
    _patched_classes.add(cls)


def _patch_async_v2(cls: Type[Any]) -> None:
    """Async twin of _patch_v2 — wraps AsyncClientV2.chat (#271.h)."""
    if cls in _patched_classes:
        return
    original_chat = cls.chat

    async def patched_chat(self: Any, *args: Any, **kwargs: Any) -> Any:
        ctx = current_execution_context()
        if ctx is None:
            return await original_chat(self, *args, **kwargs)
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
            response = await original_chat(self, *args, **kwargs)
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
                error_class=classify_cohere_exception(exc),
                http_status=extract_http_status(exc),
                retry_after_seconds=extract_retry_after(exc),
                endpoint="/v2/chat",
            )
            raise

        duration_ms = int((time.perf_counter() - start) * 1000)
        response_text, input_tokens, output_tokens = _extract_v2_fields(response)
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.add_tokens(
                tokens_in=input_tokens, tokens_out=output_tokens,
            )
        client.submit_event(_success_event(
            event_id=event_id,
            execution_id=ctx.execution_id,
            sequence=sequence,
            duration_ms=duration_ms,
            model=model,
            system_text=system_text,
            user_message=user_message,
            response_text=response_text,
            input_tokens=input_tokens,
            output_tokens=output_tokens,
        ))
        return response

    patched_chat.__name__ = getattr(original_chat, "__name__", "chat")
    patched_chat.__doc__ = getattr(original_chat, "__doc__", None)
    cls.chat = patched_chat  # type: ignore[assignment]
    _patched_classes.add(cls)


def _success_event(
    *,
    event_id: str,
    execution_id: str,
    sequence: int,
    duration_ms: int,
    model: str,
    system_text: str,
    user_message: str,
    response_text: str,
    input_tokens: int,
    output_tokens: int,
) -> Event:
    return Event(
        event_id=event_id,
        execution_id=execution_id,
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
    )


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
        "error_class": classify_cohere_exception(exc),
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
    for msg in messages:
        if not isinstance(msg, dict):
            continue
        if msg.get("role") != "system":
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


def _extract_last_user_message(messages: List[Any]) -> str:
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


def _extract_v1_fields(response: Any) -> Tuple[str, int, int]:
    """Cohere v1 response: ``response.text`` for the full reply,
    ``response.meta.tokens.input_tokens / output_tokens`` for usage.
    Older SDK versions may use ``response.token_count`` instead;
    both shapes are probed."""
    response_text = ""
    input_tokens = 0
    output_tokens = 0
    try:
        text = getattr(response, "text", None)
        if isinstance(text, str):
            response_text = text
    except Exception as exc:
        logger.debug("mesedi: cohere v1 text extraction failed: %s", exc)
    try:
        meta = getattr(response, "meta", None)
        if meta is not None:
            tokens = getattr(meta, "tokens", None) or getattr(meta, "billed_units", None)
            if tokens is not None:
                input_tokens = int(getattr(tokens, "input_tokens", 0) or 0)
                output_tokens = int(getattr(tokens, "output_tokens", 0) or 0)
    except Exception as exc:
        logger.debug("mesedi: cohere v1 usage extraction failed: %s", exc)
    return response_text, input_tokens, output_tokens


def _extract_v2_fields(response: Any) -> Tuple[str, int, int]:
    """Cohere v2 response: ``response.message.content[0].text`` for
    the reply, ``response.usage.tokens.input_tokens/output_tokens``
    for usage."""
    response_text = ""
    input_tokens = 0
    output_tokens = 0
    try:
        message = getattr(response, "message", None)
        if message is not None:
            content = getattr(message, "content", None) or []
            parts: List[str] = []
            for block in content:
                t = getattr(block, "text", None)
                if isinstance(t, str):
                    parts.append(t)
            response_text = "\n".join(parts)
    except Exception as exc:
        logger.debug("mesedi: cohere v2 message extraction failed: %s", exc)
    try:
        usage = getattr(response, "usage", None)
        if usage is not None:
            tokens = getattr(usage, "tokens", None) or getattr(usage, "billed_units", None)
            if tokens is not None:
                input_tokens = int(getattr(tokens, "input_tokens", 0) or 0)
                output_tokens = int(getattr(tokens, "output_tokens", 0) or 0)
    except Exception as exc:
        logger.debug("mesedi: cohere v2 usage extraction failed: %s", exc)
    return response_text, input_tokens, output_tokens


def _truncate(s: str, max_len: int) -> str:
    if len(s) <= max_len:
        return s
    return s[: max_len - 3] + "..."
