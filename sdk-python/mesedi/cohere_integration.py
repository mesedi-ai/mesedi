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

Async coverage:

  - ``cohere.AsyncClient`` (v1) and ``cohere.AsyncClientV2`` (v2)
    are also patched by ``instrument_cohere()``. Mirrors the sync
    wrappers: same canonical error_class mapping, same retry_after
    extraction, same _maybe_emit_throttling_event auto-emit.

Still out of scope (filed under follow-ups):

  - Streaming responses (``chat_stream``) — patched in .
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
        async_client_v1_class: legacy ``cohere.AsyncClient`` class.
        async_client_v2_class: modern ``cohere.AsyncClientV2`` class.

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

    # : async-aware patching.
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

    # : also patch chat_stream on each available class. Each
    # patcher is idempotent (separate _stream_patched_classes set).
    if client_v1_class is not None:
        _patch_v1_stream(client_v1_class)
    if client_v2_class is not None:
        _patch_v2_stream(client_v2_class)
    if async_client_v1_class is not None:
        _patch_async_v1_stream(async_client_v1_class)
    if async_client_v2_class is not None:
        _patch_async_v2_stream(async_client_v2_class)

    # : also patch embed on each available class. Same embed
    # shape v1+v2 (texts=[str], model=, input_type=), so the patcher
    # is shared. Separate _embed_patched_classes set keeps embed
    # patching idempotent independent of chat/streaming patching.
    if client_v1_class is not None:
        _patch_embed_sync(client_v1_class)
    if client_v2_class is not None:
        _patch_embed_sync(client_v2_class)
    if async_client_v1_class is not None:
        _patch_embed_async(async_client_v1_class)
    if async_client_v2_class is not None:
        _patch_embed_async(async_client_v2_class)

    # sub-ship 2: also patch rerank on each available class.
    # Rerank shape is v1+v2 identical (query=, documents=[], model=).
    # Separate _rerank_patched_classes set keeps it idempotent
    # independent of chat/streaming/embed patching on the same class.
    if client_v1_class is not None:
        _patch_rerank_sync(client_v1_class)
    if client_v2_class is not None:
        _patch_rerank_sync(client_v2_class)
    if async_client_v1_class is not None:
        _patch_rerank_async(async_client_v1_class)
    if async_client_v2_class is not None:
        _patch_rerank_async(async_client_v2_class)

    return patched_any


def _patch_v1(cls: Type[Any]) -> None:
    """Wrap cls.chat — v1 ``message=...``/``chat_history=...`` shape.

    Safely no-ops when the class does not expose .chat (test fakes,
    customer subclasses that only do embeddings, etc.). Mirrors the
    hasattr posture already used by the streaming patchers.
    """
    if cls in _patched_classes:
        return
    original_chat = getattr(cls, "chat", None)
    if original_chat is None:
        return

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
            # exceptions so infrastructure_throttled
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
    """Wrap cls.chat — v2 ``messages=[...]`` shape (OpenAI-style).

    Same hasattr guard as _patch_v1.
    """
    if cls in _patched_classes:
        return
    original_chat = getattr(cls, "chat", None)
    if original_chat is None:
        return

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
            # exceptions; same rationale as the v1 chat
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
    """Async twin of _patch_v1 — wraps AsyncClient.chat.

    Same hasattr guard as the sync twin.
    """
    if cls in _patched_classes:
        return
    original_chat = getattr(cls, "chat", None)
    if original_chat is None:
        return

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
    """Async twin of _patch_v2 — wraps AsyncClientV2.chat.

    Same hasattr guard as the sync twin.
    """
    if cls in _patched_classes:
        return
    original_chat = getattr(cls, "chat", None)
    if original_chat is None:
        return

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
    surface: str = "chat",
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
            "surface": surface,
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
    surface: str = "chat",
) -> Event:
    failure_payload: Dict[str, Any] = {
        "provider": _PROVIDER,
        "surface": surface,
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


## ── streaming patching ────────────────────────────────────────


_stream_patched_classes: set = set()


def _patch_v1_stream(cls: Type[Any]) -> None:
    """Patch Cohere v1 Client.chat_stream — sync streaming generator."""
    if cls in _stream_patched_classes:
        return
    original_stream = getattr(cls, "chat_stream", None)
    if original_stream is None:
        return

    def patched_stream(self: Any, *args: Any, **kwargs: Any) -> Any:
        ctx = current_execution_context()
        if ctx is None:
            return original_stream(self, *args, **kwargs)
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
            inner = original_stream(self, *args, **kwargs)
        except BaseException as exc:
            duration_ms = int((time.perf_counter() - start) * 1000)
            _emit_cohere_stream_failure(
                client=client, ctx=ctx, event_id=event_id, sequence=sequence,
                duration_ms=duration_ms, model=model, system_text=system_text,
                user_message=user_message, exc=exc, endpoint="/v1/chat",
            )
            raise

        return _CohereStreamIteratorWrapper(
            inner=inner, ctx=ctx, client=client, event_id=event_id,
            sequence=sequence, model=model, system_text=system_text,
            user_message=user_message, start=start, endpoint="/v1/chat",
            accumulate_chunk=_accumulate_cohere_v1_chunk,
        )

    patched_stream.__name__ = getattr(original_stream, "__name__", "chat_stream")
    patched_stream.__doc__ = getattr(original_stream, "__doc__", None)
    cls.chat_stream = patched_stream  # type: ignore[assignment]
    _stream_patched_classes.add(cls)


def _patch_v2_stream(cls: Type[Any]) -> None:
    """Patch Cohere v2 ClientV2.chat_stream — sync streaming generator."""
    if cls in _stream_patched_classes:
        return
    original_stream = getattr(cls, "chat_stream", None)
    if original_stream is None:
        return

    def patched_stream(self: Any, *args: Any, **kwargs: Any) -> Any:
        ctx = current_execution_context()
        if ctx is None:
            return original_stream(self, *args, **kwargs)
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
            inner = original_stream(self, *args, **kwargs)
        except BaseException as exc:
            duration_ms = int((time.perf_counter() - start) * 1000)
            _emit_cohere_stream_failure(
                client=client, ctx=ctx, event_id=event_id, sequence=sequence,
                duration_ms=duration_ms, model=model, system_text=system_text,
                user_message=user_message, exc=exc, endpoint="/v2/chat",
            )
            raise

        return _CohereStreamIteratorWrapper(
            inner=inner, ctx=ctx, client=client, event_id=event_id,
            sequence=sequence, model=model, system_text=system_text,
            user_message=user_message, start=start, endpoint="/v2/chat",
            accumulate_chunk=_accumulate_cohere_v2_chunk,
        )

    patched_stream.__name__ = getattr(original_stream, "__name__", "chat_stream")
    patched_stream.__doc__ = getattr(original_stream, "__doc__", None)
    cls.chat_stream = patched_stream  # type: ignore[assignment]
    _stream_patched_classes.add(cls)


def _patch_async_v1_stream(cls: Type[Any]) -> None:
    """Patch Cohere v1 AsyncClient.chat_stream — async streaming generator."""
    if cls in _stream_patched_classes:
        return
    original_stream = getattr(cls, "chat_stream", None)
    if original_stream is None:
        return

    def patched_stream(self: Any, *args: Any, **kwargs: Any) -> Any:
        ctx = current_execution_context()
        if ctx is None:
            return original_stream(self, *args, **kwargs)
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
            inner = original_stream(self, *args, **kwargs)
        except BaseException as exc:
            duration_ms = int((time.perf_counter() - start) * 1000)
            _emit_cohere_stream_failure(
                client=client, ctx=ctx, event_id=event_id, sequence=sequence,
                duration_ms=duration_ms, model=model, system_text=system_text,
                user_message=user_message, exc=exc, endpoint="/v1/chat",
            )
            raise

        return _CohereAsyncStreamIteratorWrapper(
            inner=inner, ctx=ctx, client=client, event_id=event_id,
            sequence=sequence, model=model, system_text=system_text,
            user_message=user_message, start=start, endpoint="/v1/chat",
            accumulate_chunk=_accumulate_cohere_v1_chunk,
        )

    patched_stream.__name__ = getattr(original_stream, "__name__", "chat_stream")
    patched_stream.__doc__ = getattr(original_stream, "__doc__", None)
    cls.chat_stream = patched_stream  # type: ignore[assignment]
    _stream_patched_classes.add(cls)


def _patch_async_v2_stream(cls: Type[Any]) -> None:
    """Patch Cohere v2 AsyncClientV2.chat_stream — async streaming generator."""
    if cls in _stream_patched_classes:
        return
    original_stream = getattr(cls, "chat_stream", None)
    if original_stream is None:
        return

    def patched_stream(self: Any, *args: Any, **kwargs: Any) -> Any:
        ctx = current_execution_context()
        if ctx is None:
            return original_stream(self, *args, **kwargs)
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
            inner = original_stream(self, *args, **kwargs)
        except BaseException as exc:
            duration_ms = int((time.perf_counter() - start) * 1000)
            _emit_cohere_stream_failure(
                client=client, ctx=ctx, event_id=event_id, sequence=sequence,
                duration_ms=duration_ms, model=model, system_text=system_text,
                user_message=user_message, exc=exc, endpoint="/v2/chat",
            )
            raise

        return _CohereAsyncStreamIteratorWrapper(
            inner=inner, ctx=ctx, client=client, event_id=event_id,
            sequence=sequence, model=model, system_text=system_text,
            user_message=user_message, start=start, endpoint="/v2/chat",
            accumulate_chunk=_accumulate_cohere_v2_chunk,
        )

    patched_stream.__name__ = getattr(original_stream, "__name__", "chat_stream")
    patched_stream.__doc__ = getattr(original_stream, "__doc__", None)
    cls.chat_stream = patched_stream  # type: ignore[assignment]
    _stream_patched_classes.add(cls)


class _CohereStreamIteratorWrapper:
    """Wraps a Cohere chat_stream sync generator. Aggregates chunks
    via the provided accumulator callback; emits llm_call event on
    iteration completion (StopIteration). Mid-stream exceptions are
    caught in __next__ and re-raised after emission."""

    def __init__(
        self, *, inner: Any, ctx: Any, client: Any, event_id: str,
        sequence: int, model: str, system_text: str, user_message: str,
        start: float, endpoint: str, accumulate_chunk: Any,
    ) -> None:
        self._inner = inner
        self._ctx = ctx
        self._client = client
        self._event_id = event_id
        self._sequence = sequence
        self._model = model
        self._system_text = system_text
        self._user_message = user_message
        self._start = start
        self._endpoint = endpoint
        self._accumulate_chunk = accumulate_chunk
        self._state: Dict[str, Any] = {"text_parts": [], "input_tokens": 0, "output_tokens": 0}
        self._emitted = False

    def __getattr__(self, name: str) -> Any:
        return getattr(self._inner, name)

    def __iter__(self) -> Any:
        return self

    def __next__(self) -> Any:
        try:
            chunk = next(self._inner)
        except StopIteration:
            self._emit_success()
            raise
        except BaseException as exc:
            self._emit_failure(exc)
            raise
        try:
            self._accumulate_chunk(chunk, self._state)
        except Exception as acc_exc:
            logger.debug("mesedi: cohere stream chunk accumulate failed: %s", acc_exc)
        return chunk

    def _emit_success(self) -> None:
        if self._emitted:
            return
        self._emitted = True
        duration_ms = int((time.perf_counter() - self._start) * 1000)
        response_text = "".join(self._state["text_parts"])
        input_tokens = int(self._state.get("input_tokens", 0) or 0)
        output_tokens = int(self._state.get("output_tokens", 0) or 0)
        if self._ctx.budget_tracker is not None:
            self._ctx.budget_tracker.add_tokens(
                tokens_in=input_tokens, tokens_out=output_tokens,
            )
        self._client.submit_event(Event(
            event_id=self._event_id,
            execution_id=self._ctx.execution_id,
            event_type=EventType.LLM_CALL,
            sequence=self._sequence,
            timestamp=utcnow_rfc3339(),
            duration_ms=duration_ms,
            payload={
                "provider": _PROVIDER,
                "surface": "chat",
                "model": self._model,
                "system_prompt": _truncate(self._system_text, _MAX_SYSTEM),
                "user_message": _truncate(self._user_message, _MAX_USER_MSG),
                "response_text": _truncate(response_text, _MAX_RESPONSE),
                "status": "ok",
                "input_tokens": input_tokens,
                "output_tokens": output_tokens,
                "streaming": True,
            },
        ))

    def _emit_failure(self, exc: BaseException) -> None:
        if self._emitted:
            return
        self._emitted = True
        duration_ms = int((time.perf_counter() - self._start) * 1000)
        _emit_cohere_stream_failure(
            client=self._client, ctx=self._ctx, event_id=self._event_id,
            sequence=self._sequence, duration_ms=duration_ms,
            model=self._model, system_text=self._system_text,
            user_message=self._user_message, exc=exc, endpoint=self._endpoint,
        )


class _CohereAsyncStreamIteratorWrapper(_CohereStreamIteratorWrapper):
    """Async twin of _CohereStreamIteratorWrapper."""

    def __aiter__(self) -> Any:
        return self

    async def __anext__(self) -> Any:
        try:
            chunk = await self._inner.__anext__()
        except StopAsyncIteration:
            self._emit_success()
            raise
        except BaseException as exc:
            self._emit_failure(exc)
            raise
        try:
            self._accumulate_chunk(chunk, self._state)
        except Exception as acc_exc:
            logger.debug("mesedi: cohere async stream chunk accumulate failed: %s", acc_exc)
        return chunk


def _accumulate_cohere_v1_chunk(chunk: Any, state: Dict[str, Any]) -> None:
    """v1 event shape: event.event_type 'text-generation' with .text
    for incremental tokens, 'stream-end' with .response for finals."""
    et = getattr(chunk, "event_type", "") or ""
    if et == "text-generation":
        text = getattr(chunk, "text", None)
        if isinstance(text, str) and text:
            state["text_parts"].append(text)
    elif et == "stream-end":
        response = getattr(chunk, "response", None)
        if response is not None:
            try:
                meta = getattr(response, "meta", None) or getattr(response, "response", None)
                if meta is not None:
                    tokens = getattr(meta, "tokens", None) or getattr(meta, "billed_units", None)
                    if tokens is not None:
                        state["input_tokens"] = int(getattr(tokens, "input_tokens", 0) or 0)
                        state["output_tokens"] = int(getattr(tokens, "output_tokens", 0) or 0)
            except Exception:
                pass


def _accumulate_cohere_v2_chunk(chunk: Any, state: Dict[str, Any]) -> None:
    """v2 event shape: event.type 'content-delta' with .delta.message
    .content.text for incremental tokens, 'message-end' with .delta.usage."""
    t = getattr(chunk, "type", "") or ""
    delta = getattr(chunk, "delta", None)
    if t == "content-delta" and delta is not None:
        try:
            msg = getattr(delta, "message", None)
            if msg is not None:
                content = getattr(msg, "content", None)
                if content is not None:
                    text = getattr(content, "text", None)
                    if isinstance(text, str) and text:
                        state["text_parts"].append(text)
        except Exception:
            pass
    elif t == "message-end" and delta is not None:
        try:
            usage = getattr(delta, "usage", None)
            if usage is not None:
                tokens = getattr(usage, "tokens", None) or getattr(usage, "billed_units", None)
                if tokens is not None:
                    state["input_tokens"] = int(getattr(tokens, "input_tokens", 0) or 0)
                    state["output_tokens"] = int(getattr(tokens, "output_tokens", 0) or 0)
        except Exception:
            pass


def _emit_cohere_stream_failure(
    *, client: Any, ctx: Any, event_id: str, sequence: int, duration_ms: int,
    model: str, system_text: str, user_message: str, exc: BaseException,
    endpoint: str,
) -> None:
    """Shared failure-event emitter for request-time + mid-stream
    streaming exceptions."""
    failure_payload = {
        "provider": _PROVIDER,
        "surface": "chat",
        "model": model,
        "system_prompt": _truncate(system_text, _MAX_SYSTEM),
        "user_message": _truncate(user_message, _MAX_USER_MSG),
        "status": "failed",
        "error_class": classify_cohere_exception(exc),
        "exception_type": type(exc).__name__,
        "exception_message": _truncate(str(exc), _MAX_EXC_MSG),
        "streaming": True,
    }
    http_status = extract_http_status(exc)
    if http_status is not None:
        failure_payload["http_status"] = http_status
    retry_after = extract_retry_after(exc)
    if retry_after is not None:
        failure_payload["retry_after_seconds"] = retry_after
    client.submit_event(Event(
        event_id=event_id,
        execution_id=ctx.execution_id,
        event_type=EventType.LLM_CALL,
        sequence=sequence,
        timestamp=utcnow_rfc3339(),
        duration_ms=duration_ms,
        payload=failure_payload,
    ))
    _maybe_emit_throttling_event(
        provider=_PROVIDER,
        error_class=failure_payload["error_class"],
        http_status=http_status,
        retry_after_seconds=retry_after,
        endpoint=endpoint,
    )


## ── non-chat surfaces (embeddings) ─────────────────────────────


_embed_patched_classes: set = set()


def _extract_embed_input(kwargs: Dict[str, Any]) -> str:
    """Cohere embed accepts ``texts=[str, ...]`` (v1+v2) or ``inputs=``
    (v2 multi-modal). Stringify so user_message records the input."""
    texts = kwargs.get("texts") or kwargs.get("inputs") or []
    if isinstance(texts, str):
        return texts
    if isinstance(texts, list):
        if texts and isinstance(texts[0], str):
            return "\n".join(str(t) for t in texts)
        return f"<{len(texts)} embed input(s)>"
    return repr(texts)


def _extract_embed_response_tokens(response: Any) -> int:
    """Pull billed input tokens from EmbedResponse.meta.billed_units."""
    try:
        meta = getattr(response, "meta", None)
        if meta is not None:
            billed = getattr(meta, "billed_units", None)
            if billed is not None:
                return int(getattr(billed, "input_tokens", 0) or 0)
    except Exception as exc:
        logger.debug("mesedi: cohere embed usage extraction failed: %s", exc)
    return 0


def _patch_embed_sync(cls: Type[Any]) -> None:
    """Wrap cls.embed to emit surface='embeddings' llm_call events.

    Safely no-ops on a Cohere client class that does not expose
    .embed (older versions, customer subclasses that don't use
    embeddings, test fakes). Mirrors the chat_stream patcher's
    hasattr guard — instrument_cohere() must never crash because
    the customer's client class lacks a surface they don't use.
    """
    if cls in _embed_patched_classes:
        return
    original_embed = getattr(cls, "embed", None)
    if original_embed is None:
        return

    def patched_embed(self: Any, *args: Any, **kwargs: Any) -> Any:
        ctx = current_execution_context()
        if ctx is None:
            return original_embed(self, *args, **kwargs)
        ctx.check_budget()

        client = get_client()
        sequence = ctx.next_sequence()
        event_id = f"evt-{uuid.uuid4().hex[:12]}"
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.increment_steps()

        model = kwargs.get("model", "unknown")
        user_message = _extract_embed_input(kwargs)

        start = time.perf_counter()
        try:
            response = original_embed(self, *args, **kwargs)
        except BaseException as exc:
            duration_ms = int((time.perf_counter() - start) * 1000)
            client.submit_event(_build_failure_event(
                event_id=event_id,
                execution_id=ctx.execution_id,
                sequence=sequence,
                duration_ms=duration_ms,
                model=model,
                system_text="",
                user_message=user_message,
                exc=exc,
                surface="embeddings",
            ))
            _maybe_emit_throttling_event(
                provider=_PROVIDER,
                error_class=classify_cohere_exception(exc),
                http_status=extract_http_status(exc),
                retry_after_seconds=extract_retry_after(exc),
                endpoint="/v1/embed",
            )
            raise

        duration_ms = int((time.perf_counter() - start) * 1000)
        input_tokens = _extract_embed_response_tokens(response)
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.add_tokens(
                tokens_in=input_tokens, tokens_out=0,
            )
        client.submit_event(_success_event(
            event_id=event_id,
            execution_id=ctx.execution_id,
            sequence=sequence,
            duration_ms=duration_ms,
            model=model,
            system_text="",
            user_message=user_message,
            response_text="<embedding vectors>",
            input_tokens=input_tokens,
            output_tokens=0,
            surface="embeddings",
        ))
        return response

    patched_embed.__name__ = getattr(original_embed, "__name__", "embed")
    patched_embed.__doc__ = getattr(original_embed, "__doc__", None)
    cls.embed = patched_embed  # type: ignore[assignment]
    _embed_patched_classes.add(cls)


def _patch_embed_async(cls: Type[Any]) -> None:
    """Async twin of _patch_embed_sync — AsyncClient[V2].embed.

    Same hasattr guard as the sync twin.
    """
    if cls in _embed_patched_classes:
        return
    original_embed = getattr(cls, "embed", None)
    if original_embed is None:
        return

    async def patched_embed(self: Any, *args: Any, **kwargs: Any) -> Any:
        ctx = current_execution_context()
        if ctx is None:
            return await original_embed(self, *args, **kwargs)
        ctx.check_budget()

        client = get_client()
        sequence = ctx.next_sequence()
        event_id = f"evt-{uuid.uuid4().hex[:12]}"
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.increment_steps()

        model = kwargs.get("model", "unknown")
        user_message = _extract_embed_input(kwargs)

        start = time.perf_counter()
        try:
            response = await original_embed(self, *args, **kwargs)
        except BaseException as exc:
            duration_ms = int((time.perf_counter() - start) * 1000)
            client.submit_event(_build_failure_event(
                event_id=event_id,
                execution_id=ctx.execution_id,
                sequence=sequence,
                duration_ms=duration_ms,
                model=model,
                system_text="",
                user_message=user_message,
                exc=exc,
                surface="embeddings",
            ))
            _maybe_emit_throttling_event(
                provider=_PROVIDER,
                error_class=classify_cohere_exception(exc),
                http_status=extract_http_status(exc),
                retry_after_seconds=extract_retry_after(exc),
                endpoint="/v1/embed",
            )
            raise

        duration_ms = int((time.perf_counter() - start) * 1000)
        input_tokens = _extract_embed_response_tokens(response)
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.add_tokens(
                tokens_in=input_tokens, tokens_out=0,
            )
        client.submit_event(_success_event(
            event_id=event_id,
            execution_id=ctx.execution_id,
            sequence=sequence,
            duration_ms=duration_ms,
            model=model,
            system_text="",
            user_message=user_message,
            response_text="<embedding vectors>",
            input_tokens=input_tokens,
            output_tokens=0,
            surface="embeddings",
        ))
        return response

    patched_embed.__name__ = getattr(original_embed, "__name__", "embed")
    patched_embed.__doc__ = getattr(original_embed, "__doc__", None)
    cls.embed = patched_embed  # type: ignore[assignment]
    _embed_patched_classes.add(cls)


## ── sub-ship 2 — rerank surface ───────────────────────────────


_rerank_patched_classes: set = set()


def _extract_rerank_input(kwargs: Dict[str, Any]) -> str:
    """Cohere rerank kwargs: ``query=str`` + ``documents=[str, ...]``.
    Stringify both so DLP scanning still has signal; downstream
    _MAX_USER_MSG truncation caps the size."""
    query = kwargs.get("query", "")
    docs = kwargs.get("documents", []) or []
    if not isinstance(query, str):
        query = repr(query)
    if isinstance(docs, list) and docs and isinstance(docs[0], str):
        docs_preview = "\n".join(str(d) for d in docs)
        return f"query: {query}\n---\ndocs:\n{docs_preview}"
    return f"query: {query} (docs: {len(docs) if isinstance(docs, list) else 'n/a'})"


def _extract_rerank_response_tokens(response: Any) -> int:
    """Cohere rerank does not bill tokens — it bills search_units.
    Return 0 so cost computation routes through rerank-specific
    pricing when that lands (separate wave)."""
    return 0


def _extract_rerank_results_count(response: Any) -> int:
    try:
        results = getattr(response, "results", None)
        if isinstance(results, list):
            return len(results)
    except Exception:
        pass
    return 0


def _patch_rerank_sync(cls: Type[Any]) -> None:
    """Wrap cls.rerank to emit surface='rerank' llm_call events.

    Safely no-ops when the class does not expose .rerank.
    """
    if cls in _rerank_patched_classes:
        return
    original_rerank = getattr(cls, "rerank", None)
    if original_rerank is None:
        return

    def patched_rerank(self: Any, *args: Any, **kwargs: Any) -> Any:
        ctx = current_execution_context()
        if ctx is None:
            return original_rerank(self, *args, **kwargs)
        ctx.check_budget()

        client = get_client()
        sequence = ctx.next_sequence()
        event_id = f"evt-{uuid.uuid4().hex[:12]}"
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.increment_steps()

        model = kwargs.get("model", "unknown")
        user_message = _extract_rerank_input(kwargs)

        start = time.perf_counter()
        try:
            response = original_rerank(self, *args, **kwargs)
        except BaseException as exc:
            duration_ms = int((time.perf_counter() - start) * 1000)
            client.submit_event(_build_failure_event(
                event_id=event_id,
                execution_id=ctx.execution_id,
                sequence=sequence,
                duration_ms=duration_ms,
                model=model,
                system_text="",
                user_message=user_message,
                exc=exc,
                surface="rerank",
            ))
            _maybe_emit_throttling_event(
                provider=_PROVIDER,
                error_class=classify_cohere_exception(exc),
                http_status=extract_http_status(exc),
                retry_after_seconds=extract_retry_after(exc),
                endpoint="/v1/rerank",
            )
            raise

        duration_ms = int((time.perf_counter() - start) * 1000)
        n_results = _extract_rerank_results_count(response)
        client.submit_event(_success_event(
            event_id=event_id,
            execution_id=ctx.execution_id,
            sequence=sequence,
            duration_ms=duration_ms,
            model=model,
            system_text="",
            user_message=user_message,
            response_text=f"<rerank results: {n_results}>",
            input_tokens=0,
            output_tokens=0,
            surface="rerank",
        ))
        return response

    patched_rerank.__name__ = getattr(original_rerank, "__name__", "rerank")
    patched_rerank.__doc__ = getattr(original_rerank, "__doc__", None)
    cls.rerank = patched_rerank
    _rerank_patched_classes.add(cls)


def _patch_rerank_async(cls: Type[Any]) -> None:
    """Async twin of _patch_rerank_sync — AsyncClient[V2].rerank.

    Same hasattr guard as the sync twin.
    """
    if cls in _rerank_patched_classes:
        return
    original_rerank = getattr(cls, "rerank", None)
    if original_rerank is None:
        return

    async def patched_rerank(self: Any, *args: Any, **kwargs: Any) -> Any:
        ctx = current_execution_context()
        if ctx is None:
            return await original_rerank(self, *args, **kwargs)
        ctx.check_budget()

        client = get_client()
        sequence = ctx.next_sequence()
        event_id = f"evt-{uuid.uuid4().hex[:12]}"
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.increment_steps()

        model = kwargs.get("model", "unknown")
        user_message = _extract_rerank_input(kwargs)

        start = time.perf_counter()
        try:
            response = await original_rerank(self, *args, **kwargs)
        except BaseException as exc:
            duration_ms = int((time.perf_counter() - start) * 1000)
            client.submit_event(_build_failure_event(
                event_id=event_id,
                execution_id=ctx.execution_id,
                sequence=sequence,
                duration_ms=duration_ms,
                model=model,
                system_text="",
                user_message=user_message,
                exc=exc,
                surface="rerank",
            ))
            _maybe_emit_throttling_event(
                provider=_PROVIDER,
                error_class=classify_cohere_exception(exc),
                http_status=extract_http_status(exc),
                retry_after_seconds=extract_retry_after(exc),
                endpoint="/v1/rerank",
            )
            raise

        duration_ms = int((time.perf_counter() - start) * 1000)
        n_results = _extract_rerank_results_count(response)
        client.submit_event(_success_event(
            event_id=event_id,
            execution_id=ctx.execution_id,
            sequence=sequence,
            duration_ms=duration_ms,
            model=model,
            system_text="",
            user_message=user_message,
            response_text=f"<rerank results: {n_results}>",
            input_tokens=0,
            output_tokens=0,
            surface="rerank",
        ))
        return response

    patched_rerank.__name__ = getattr(original_rerank, "__name__", "rerank")
    patched_rerank.__doc__ = getattr(original_rerank, "__doc__", None)
    cls.rerank = patched_rerank
    _rerank_patched_classes.add(cls)
