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
# Async patching uses a separate sentinel because Gemini's async
# surface is a different METHOD on the SAME class (generate_content
# vs generate_content_async) rather than a different class like
# Anthropic's AsyncMessages. A single `_patched_classes` set would
# falsely block async patching when sync had already run.
_async_patched_classes: set = set()


def instrument_gemini(model_class: Optional[Type[Any]] = None) -> bool:
    """Patch GenerativeModel.generate_content (sync) AND
    generate_content_async (async, #271.h) to emit llm_call events.

    Args:
        model_class: Class whose ``generate_content`` /
            ``generate_content_async`` methods to patch. When None
            (default), tries to import
            ``google.generativeai.GenerativeModel``.

    Returns:
        True if at least one of the sync/async surfaces was patched
        (or was a no-op on a re-patch). False if neither
        google-generativeai is installed nor a class was provided.
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

    sync_ok = _patch_gemini_sync(model_class)
    async_ok = _patch_gemini_async(model_class)

    # #271.j: also patch module-level embed_content (+ async) when
    # google.generativeai is importable. Returns True silently when
    # neither attribute exists on the module (very old SDK versions).
    embed_ok = instrument_gemini_embed()

    return sync_ok or async_ok or embed_ok


def _patch_gemini_sync(model_class: Type[Any]) -> bool:
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

        # #271.i: stream=True → wrap iterator for chunk aggregation.
        if kwargs.get("stream") is True:
            return _GeminiStreamIteratorWrapper(
                inner=response, ctx=ctx, client=client, event_id=event_id,
                sequence=sequence, model=model, system_text=system_text,
                user_message=user_message, start=start,
            )

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


def _patch_gemini_async(model_class: Type[Any]) -> bool:
    """Patch GenerativeModel.generate_content_async — #271.h.

    Mirrors _patch_gemini_sync exactly except for `async def` +
    `await`. Best-effort: if the installed google-generativeai
    version doesn't ship generate_content_async, skip silently.
    """
    if model_class in _async_patched_classes:
        return True

    original_generate_async = getattr(model_class, "generate_content_async", None)
    if original_generate_async is None:
        logger.debug(
            "mesedi: GenerativeModel.generate_content_async not present; "
            "async Gemini instrumentation skipped."
        )
        return False

    async def patched_generate_async(self: Any, *args: Any, **kwargs: Any) -> Any:
        ctx = current_execution_context()
        if ctx is None:
            return await original_generate_async(self, *args, **kwargs)
        ctx.check_budget()

        client = get_client()
        sequence = ctx.next_sequence()
        event_id = f"evt-{uuid.uuid4().hex[:12]}"
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.increment_steps()

        model = getattr(self, "model_name", None) or kwargs.get("model", "unknown")
        if not isinstance(model, str):
            model = str(model)
        system_text = getattr(self, "_system_instruction", "") or getattr(
            self, "system_instruction", ""
        ) or ""
        if not isinstance(system_text, str):
            system_text = _stringify_gemini_content(system_text)

        contents = args[0] if args else kwargs.get("contents", "")
        user_message = _extract_user_message_from_contents(contents)

        start = time.perf_counter()
        try:
            response = await original_generate_async(self, *args, **kwargs)
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
                endpoint="/v1/models/generateContent",
            )
            raise

        # #271.i: stream=True → wrap async iterator for chunk aggregation.
        if kwargs.get("stream") is True:
            return _GeminiAsyncStreamIteratorWrapper(
                inner=response, ctx=ctx, client=client, event_id=event_id,
                sequence=sequence, model=model, system_text=system_text,
                user_message=user_message, start=start,
            )

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

    patched_generate_async.__name__ = getattr(
        original_generate_async, "__name__", "generate_content_async"
    )
    patched_generate_async.__doc__ = getattr(original_generate_async, "__doc__", None)
    model_class.generate_content_async = patched_generate_async  # type: ignore[assignment]
    _async_patched_classes.add(model_class)
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
    surface: str = "chat",
) -> Event:
    failure_payload: Dict[str, Any] = {
        "provider": _PROVIDER,
        "surface": surface,
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


## ── #271.i streaming patching ────────────────────────────────────────


def _accumulate_gemini_chunk(chunk: Any, state: Dict[str, Any]) -> None:
    """Pull incremental text + final usage from a Gemini stream chunk.

    Gemini streaming chunks each carry .text (incremental output) and
    the LAST chunk carries .usage_metadata with prompt_token_count +
    candidates_token_count for the full call.
    """
    try:
        text = getattr(chunk, "text", None)
        if isinstance(text, str) and text:
            state["text_parts"].append(text)
    except Exception:
        # Some chunk types (e.g. safety-blocked) raise on .text;
        # defensive skip so accumulation continues for subsequent
        # chunks.
        pass
    usage = getattr(chunk, "usage_metadata", None)
    if usage is not None:
        try:
            state["input_tokens"] = int(getattr(usage, "prompt_token_count", 0) or 0)
            state["output_tokens"] = int(getattr(usage, "candidates_token_count", 0) or 0)
        except Exception:
            pass


class _GeminiStreamIteratorWrapper:
    """Wraps a Gemini sync stream iterator. Aggregates chunks via
    _accumulate_gemini_chunk; emits llm_call event on iteration
    completion (StopIteration). Mid-stream exceptions caught in
    __next__ and re-raised after emission."""

    def __init__(
        self, *, inner: Any, ctx: Any, client: Any, event_id: str,
        sequence: int, model: str, system_text: str, user_message: str,
        start: float,
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
            _accumulate_gemini_chunk(chunk, self._state)
        except Exception as acc_exc:
            logger.debug("mesedi: gemini stream chunk accumulate failed: %s", acc_exc)
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
        _emit_gemini_stream_failure(
            client=self._client, ctx=self._ctx, event_id=self._event_id,
            sequence=self._sequence, duration_ms=duration_ms,
            model=self._model, system_text=self._system_text,
            user_message=self._user_message, exc=exc,
        )


class _GeminiAsyncStreamIteratorWrapper(_GeminiStreamIteratorWrapper):
    """Async twin of _GeminiStreamIteratorWrapper."""

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
            _accumulate_gemini_chunk(chunk, self._state)
        except Exception as acc_exc:
            logger.debug("mesedi: gemini async stream chunk accumulate failed: %s", acc_exc)
        return chunk


def _emit_gemini_stream_failure(
    *, client: Any, ctx: Any, event_id: str, sequence: int,
    duration_ms: int, model: str, system_text: str, user_message: str,
    exc: BaseException,
) -> None:
    """Shared failure-event emitter for streaming exceptions."""
    failure_payload = {
        "provider": _PROVIDER,
        "surface": "chat",
        "model": model,
        "system_prompt": _truncate(system_text, _MAX_SYSTEM),
        "user_message": _truncate(user_message, _MAX_USER_MSG),
        "status": "failed",
        "error_class": classify_gemini_exception(exc),
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
        endpoint="/v1/models/generateContent",
    )


## ── #271.j non-chat surfaces (embed_content) ──────────────────────────


# Module-level patching state — Gemini's embed_content lives on the
# google.generativeai module directly (unlike chat which is a method
# on GenerativeModel). Two booleans keep sync vs async re-patching
# idempotent independent of each other.
_embed_sync_patched = False
_embed_async_patched = False


def _extract_embed_content_input(content: Any) -> str:
    """Gemini embed_content ``content`` is str | google.ai.Content |
    list[...]. Stringify for the user_message field; truncated
    downstream by _MAX_USER_MSG."""
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        if content and isinstance(content[0], str):
            return "\n".join(str(c) for c in content)
        return f"<{len(content)} embed input(s)>"
    return _stringify_gemini_content(content)


def _extract_embed_response_tokens(response: Any) -> int:
    """Gemini embed_content returns dict-like {'embedding': [...]}
    without usage metadata. Token count not available — return 0."""
    return 0


def instrument_gemini_embed(genai_module: Optional[Any] = None) -> bool:
    """Patch ``google.generativeai.embed_content`` +
    ``embed_content_async`` (when available) to emit
    surface='embeddings' llm_call events.

    Args:
        genai_module: Override module to patch. When None, imports
            ``google.generativeai``.

    Returns:
        True if at least one of the embed surfaces was patched.
    """
    global _embed_sync_patched, _embed_async_patched
    if genai_module is None:
        try:
            import google.generativeai as _genai
            genai_module = _genai
        except ImportError:
            logger.debug(
                "mesedi: google-generativeai not importable; "
                "skipping embed_content patch."
            )
            return False

    patched_any = False

    if not _embed_sync_patched and hasattr(genai_module, "embed_content"):
        original_embed = genai_module.embed_content

        def patched_embed(*args: Any, **kwargs: Any) -> Any:
            ctx = current_execution_context()
            if ctx is None:
                return original_embed(*args, **kwargs)
            ctx.check_budget()
            client = get_client()
            sequence = ctx.next_sequence()
            event_id = f"evt-{uuid.uuid4().hex[:12]}"
            if ctx.budget_tracker is not None:
                ctx.budget_tracker.increment_steps()

            model = kwargs.get("model", "unknown")
            content = kwargs.get("content")
            if content is None and args:
                # Positional: embed_content(model, content). Best-effort.
                content = args[1] if len(args) >= 2 else None
            user_message = _extract_embed_content_input(content)

            start = time.perf_counter()
            try:
                response = original_embed(*args, **kwargs)
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
                    error_class=classify_gemini_exception(exc),
                    http_status=extract_http_status(exc),
                    retry_after_seconds=extract_retry_after(exc),
                    endpoint="/v1/models/embedContent",
                )
                raise

            duration_ms = int((time.perf_counter() - start) * 1000)
            client.submit_event(Event(
                event_id=event_id,
                execution_id=ctx.execution_id,
                event_type=EventType.LLM_CALL,
                sequence=sequence,
                timestamp=utcnow_rfc3339(),
                duration_ms=duration_ms,
                payload={
                    "provider": _PROVIDER,
                    "surface": "embeddings",
                    "model": model,
                    "user_message": _truncate(user_message, _MAX_USER_MSG),
                    "response_text": "<embedding vectors>",
                    "status": "ok",
                    "input_tokens": 0,
                    "output_tokens": 0,
                },
            ))
            return response

        patched_embed.__name__ = "embed_content"
        patched_embed.__doc__ = getattr(original_embed, "__doc__", None)
        genai_module.embed_content = patched_embed
        _embed_sync_patched = True
        patched_any = True

    if not _embed_async_patched and hasattr(genai_module, "embed_content_async"):
        original_embed_async = genai_module.embed_content_async

        async def patched_embed_async(*args: Any, **kwargs: Any) -> Any:
            ctx = current_execution_context()
            if ctx is None:
                return await original_embed_async(*args, **kwargs)
            ctx.check_budget()
            client = get_client()
            sequence = ctx.next_sequence()
            event_id = f"evt-{uuid.uuid4().hex[:12]}"
            if ctx.budget_tracker is not None:
                ctx.budget_tracker.increment_steps()

            model = kwargs.get("model", "unknown")
            content = kwargs.get("content")
            if content is None and args:
                content = args[1] if len(args) >= 2 else None
            user_message = _extract_embed_content_input(content)

            start = time.perf_counter()
            try:
                response = await original_embed_async(*args, **kwargs)
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
                    error_class=classify_gemini_exception(exc),
                    http_status=extract_http_status(exc),
                    retry_after_seconds=extract_retry_after(exc),
                    endpoint="/v1/models/embedContent",
                )
                raise

            duration_ms = int((time.perf_counter() - start) * 1000)
            client.submit_event(Event(
                event_id=event_id,
                execution_id=ctx.execution_id,
                event_type=EventType.LLM_CALL,
                sequence=sequence,
                timestamp=utcnow_rfc3339(),
                duration_ms=duration_ms,
                payload={
                    "provider": _PROVIDER,
                    "surface": "embeddings",
                    "model": model,
                    "user_message": _truncate(user_message, _MAX_USER_MSG),
                    "response_text": "<embedding vectors>",
                    "status": "ok",
                    "input_tokens": 0,
                    "output_tokens": 0,
                },
            ))
            return response

        patched_embed_async.__name__ = "embed_content_async"
        patched_embed_async.__doc__ = getattr(original_embed_async, "__doc__", None)
        genai_module.embed_content_async = patched_embed_async
        _embed_async_patched = True
        patched_any = True

    return patched_any
