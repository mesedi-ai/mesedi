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
from mesedi.observe import _maybe_emit_throttling_event
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
    async_completions_class: Optional[Type[Any]] = None,
    async_responses_class: Optional[Type[Any]] = None,
) -> bool:
    """Patch the OpenAI SDK's chat-completions and Responses APIs to
    emit llm_call events. Patches both sync + async surfaces (#271.h
    closes the async coverage gap that previously left AsyncOpenAI
    customers invisible to provider_incident).

    Args:
        completions_class: Sync class whose ``create`` method handles
            chat completions. Default: ``openai.resources.chat.completions.Completions``.
        responses_class: Sync class whose ``create`` method handles
            the Responses API. Default: ``openai.resources.responses.Responses``.
            Requires openai>=1.40; older installations skip silently.
        async_completions_class: Async class whose ``create`` method
            handles chat completions. Default:
            ``openai.resources.chat.completions.AsyncCompletions``.
        async_responses_class: Async class whose ``create`` method
            handles the Responses API. Default:
            ``openai.resources.responses.AsyncResponses``.

    Returns:
        True if at least one of the classes was successfully patched
        (or was a no-op because already patched). False if NONE
        could be located AND the openai package is not importable.
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

    # #271.h: async-aware patching. Mirrors the sync patchers but
    # targets the AsyncXxx classes and wraps with `async def`.
    if async_completions_class is None:
        try:
            from openai.resources.chat.completions import (
                AsyncCompletions as _AsyncCompletions,
            )
            async_completions_class = _AsyncCompletions
        except ImportError:
            logger.info(
                "mesedi: openai AsyncCompletions not importable; "
                "async chat.completions instrumentation skipped."
            )

    if async_completions_class is not None:
        _patch_async_chat_completions(async_completions_class)
        patched_any = True

    if async_responses_class is None:
        try:
            from openai.resources.responses import AsyncResponses as _AsyncResponses
            async_responses_class = _AsyncResponses
        except ImportError:
            logger.debug(
                "mesedi: openai AsyncResponses not importable; "
                "skipping async Responses API patch."
            )

    if async_responses_class is not None:
        _patch_async_responses(async_responses_class)
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
            # Auto-emit infrastructure_event on throttling-class
            # exceptions (Wave 1.4) so infrastructure_throttled isn't
            # silently inactive for the default customer.
            _maybe_emit_throttling_event(
                provider=_PROVIDER,
                error_class=classify_openai_exception(exc),
                http_status=extract_http_status(exc),
                retry_after_seconds=extract_retry_after(exc),
                endpoint="/v1/chat/completions",
            )
            raise

        # #271.i: stream=True → response is a Stream iterator, not a
        # completed ChatCompletion. Wrap so chunks pass through to the
        # customer while we aggregate response_text + tokens for the
        # final emission.
        if kwargs.get("stream") is True:
            return _OpenAIStreamIteratorWrapper(
                inner=response,
                ctx=ctx,
                client=client,
                event_id=event_id,
                sequence=sequence,
                model=model,
                system_text=system_text,
                user_message=user_message,
                start=start,
                endpoint="/v1/chat/completions",
                accumulate_chunk=_accumulate_chat_chunk,
            )

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
            # Auto-emit infrastructure_event on throttling-class
            # exceptions (Wave 1.4); same rationale as the chat
            # completions path above.
            _maybe_emit_throttling_event(
                provider=_PROVIDER,
                error_class=classify_openai_exception(exc),
                http_status=extract_http_status(exc),
                retry_after_seconds=extract_retry_after(exc),
                endpoint="/v1/responses",
            )
            raise

        # #271.i: stream=True → wrap iterator for chunk aggregation.
        if kwargs.get("stream") is True:
            return _OpenAIStreamIteratorWrapper(
                inner=response,
                ctx=ctx,
                client=client,
                event_id=event_id,
                sequence=sequence,
                model=model,
                system_text=system_text,
                user_message=user_message,
                start=start,
                endpoint="/v1/responses",
                accumulate_chunk=_accumulate_responses_chunk,
            )

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


def _patch_async_chat_completions(cls: Type[Any]) -> None:
    """Wrap cls.create (async) to emit chat-completions llm_call events.
    Mirrors _patch_chat_completions; only differences are `async def`
    + `await` on the underlying call. Same failure path, same throttling
    auto-emit, same canonical error classification."""
    if cls in _patched_classes:
        return

    original_create = cls.create

    async def patched_create(self: Any, *args: Any, **kwargs: Any) -> Any:
        ctx = current_execution_context()
        if ctx is None:
            return await original_create(self, *args, **kwargs)
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
            response = await original_create(self, *args, **kwargs)
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
                error_class=classify_openai_exception(exc),
                http_status=extract_http_status(exc),
                retry_after_seconds=extract_retry_after(exc),
                endpoint="/v1/chat/completions",
            )
            raise

        # #271.i: stream=True → wrap async iterator for chunk aggregation.
        if kwargs.get("stream") is True:
            return _OpenAIAsyncStreamIteratorWrapper(
                inner=response,
                ctx=ctx,
                client=client,
                event_id=event_id,
                sequence=sequence,
                model=model,
                system_text=system_text,
                user_message=user_message,
                start=start,
                endpoint="/v1/chat/completions",
                accumulate_chunk=_accumulate_chat_chunk,
            )

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


def _patch_async_responses(cls: Type[Any]) -> None:
    """Async twin of _patch_responses for the OpenAI Responses API."""
    if cls in _patched_classes:
        return

    original_create = cls.create

    async def patched_create(self: Any, *args: Any, **kwargs: Any) -> Any:
        ctx = current_execution_context()
        if ctx is None:
            return await original_create(self, *args, **kwargs)
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
        system_text = instructions or system_from_input

        start = time.perf_counter()
        try:
            response = await original_create(self, *args, **kwargs)
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
                error_class=classify_openai_exception(exc),
                http_status=extract_http_status(exc),
                retry_after_seconds=extract_retry_after(exc),
                endpoint="/v1/responses",
            )
            raise

        # #271.i: stream=True → wrap async iterator for chunk aggregation.
        if kwargs.get("stream") is True:
            return _OpenAIAsyncStreamIteratorWrapper(
                inner=response,
                ctx=ctx,
                client=client,
                event_id=event_id,
                sequence=sequence,
                model=model,
                system_text=system_text,
                user_message=user_message,
                start=start,
                endpoint="/v1/responses",
                accumulate_chunk=_accumulate_responses_chunk,
            )

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


## ── #271.i streaming patching ────────────────────────────────────────


class _OpenAIStreamIteratorWrapper:
    """Wraps an OpenAI sync Stream iterator so chunks pass through to
    the customer while we accumulate response_text + tokens for the
    final llm_call event emission.

    Customer iteration protocol preserved: __iter__ / __next__
    delegate to the inner Stream. On StopIteration we emit the
    success event; on any other exception we emit the failure event
    + fire the throttling auto-emit (mid-stream API errors).

    `accumulate_chunk` is provided per-surface (chat completions vs
    Responses API) so this one wrapper class covers both — chat
    chunks have `.choices[0].delta.content` + late `.usage`;
    Responses events carry text on different paths.
    """

    def __init__(
        self,
        *,
        inner: Any,
        ctx: Any,
        client: Any,
        event_id: str,
        sequence: int,
        model: str,
        system_text: str,
        user_message: str,
        start: float,
        endpoint: str,
        accumulate_chunk: Any,  # Callable[[chunk, state_dict], None]
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
        self._state = {"text_parts": [], "input_tokens": 0, "output_tokens": 0}
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
            logger.debug("mesedi: openai stream chunk accumulate failed: %s", acc_exc)
        return chunk

    # Allow `with stream as s:` context-manager usage (OpenAI Stream
    # objects support this for resource cleanup).
    def __enter__(self) -> Any:
        if hasattr(self._inner, "__enter__"):
            self._inner.__enter__()
        return self

    def __exit__(self, exc_type: Any, exc_val: Any, exc_tb: Any) -> Any:
        # If the customer used the with-block AND iterated to
        # completion, _emit_success already fired on StopIteration.
        # If they broke out early, no emission happens (matches
        # documented behavior — partial consumption = no aggregated
        # event). If exc_val is set, emit the failure event now.
        if exc_val is not None and not self._emitted:
            self._emit_failure(exc_val)
        if hasattr(self._inner, "__exit__"):
            return self._inner.__exit__(exc_type, exc_val, exc_tb)
        return None

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
        failure_payload = {
            "provider": _PROVIDER,
            "model": self._model,
            "system_prompt": _truncate(self._system_text, _MAX_SYSTEM),
            "user_message": _truncate(self._user_message, _MAX_USER_MSG),
            "status": "failed",
            "error_class": classify_openai_exception(exc),
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
        self._client.submit_event(Event(
            event_id=self._event_id,
            execution_id=self._ctx.execution_id,
            event_type=EventType.LLM_CALL,
            sequence=self._sequence,
            timestamp=utcnow_rfc3339(),
            duration_ms=duration_ms,
            payload=failure_payload,
        ))
        _maybe_emit_throttling_event(
            provider=_PROVIDER,
            error_class=failure_payload["error_class"],
            http_status=http_status,
            retry_after_seconds=retry_after,
            endpoint=self._endpoint,
        )


class _OpenAIAsyncStreamIteratorWrapper(_OpenAIStreamIteratorWrapper):
    """Async twin of _OpenAIStreamIteratorWrapper. Same accumulator
    logic; differs only in the iteration / context-manager protocol
    (async __aiter__/__anext__/__aenter__/__aexit__ vs sync versions).
    """

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
            logger.debug("mesedi: openai async stream chunk accumulate failed: %s", acc_exc)
        return chunk

    async def __aenter__(self) -> Any:
        if hasattr(self._inner, "__aenter__"):
            await self._inner.__aenter__()
        return self

    async def __aexit__(self, exc_type: Any, exc_val: Any, exc_tb: Any) -> Any:
        if exc_val is not None and not self._emitted:
            self._emit_failure(exc_val)
        if hasattr(self._inner, "__aexit__"):
            return await self._inner.__aexit__(exc_type, exc_val, exc_tb)
        return None


def _accumulate_chat_chunk(chunk: Any, state: Dict[str, Any]) -> None:
    """Pull content + usage from a ChatCompletionChunk into state.
    OpenAI chat chunks have .choices[0].delta.content for incremental
    text, and .usage on the LAST chunk (when the customer passes
    stream_options={"include_usage": True}) or None otherwise.
    """
    choices = getattr(chunk, "choices", None) or []
    if choices:
        delta = getattr(choices[0], "delta", None)
        if delta is not None:
            content = getattr(delta, "content", None)
            if isinstance(content, str) and content:
                state["text_parts"].append(content)
    usage = getattr(chunk, "usage", None)
    if usage is not None:
        try:
            state["input_tokens"] = int(getattr(usage, "prompt_tokens", 0) or 0)
            state["output_tokens"] = int(getattr(usage, "completion_tokens", 0) or 0)
        except Exception:
            pass


def _accumulate_responses_chunk(chunk: Any, state: Dict[str, Any]) -> None:
    """Pull content + usage from a Responses-API stream event.
    Responses API uses event-based streaming with multiple event
    types; the text-delta events carry incremental output_text and
    the response.completed event carries final usage.
    """
    event_type = getattr(chunk, "type", "") or ""
    # Text deltas appear on response.output_text.delta events.
    if event_type.endswith(".delta"):
        delta = getattr(chunk, "delta", None)
        if isinstance(delta, str) and delta:
            state["text_parts"].append(delta)
    # Final usage appears on the response.completed event.
    response_obj = getattr(chunk, "response", None)
    if response_obj is not None:
        usage = getattr(response_obj, "usage", None)
        if usage is not None:
            try:
                state["input_tokens"] = int(getattr(usage, "input_tokens", 0) or 0)
                state["output_tokens"] = int(getattr(usage, "output_tokens", 0) or 0)
            except Exception:
                pass
