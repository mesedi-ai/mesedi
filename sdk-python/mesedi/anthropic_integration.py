"""
Anthropic SDK monkey-patch, auto-emit llm_call events for every
``Messages.create`` call inside a ``@mesedi.wrap`` execution.

Activation is **opt-in**: call ``mesedi.instrument_anthropic()`` once at
process startup. This matches the Datadog / Sentry / OpenTelemetry
pattern, observability instrumentation should be explicit, not magical.

What gets captured per call:

  - ``model`` (e.g. "claude-opus-4-6")
  - ``system_prompt``, truncated to 1000 chars
  - ``user_message``, the LAST user-role message in the conversation,
    truncated to 1000 chars
  - ``response_text``, concatenated text-block content from the
    response, truncated to 1000 chars
  - ``input_tokens`` / ``output_tokens``, from response.usage
  - ``duration_ms``, wall-clock time of the API call
  - ``status``, "ok" if the call returned, "failed" if it raised
  - ``exception_type`` / ``exception_message``, on failure

Truncation budget is intentionally bounded (1000 chars per text field)
so the events table doesn't bloat from agents that paste whole web
pages into prompts. PII redaction is a separate, configurable layer
that lands in a future sub-slice.

Async coverage (#271.h):

  - ``AsyncAnthropic.messages.create`` (async client) is also patched
    by ``instrument_anthropic()``. The async wrapper mirrors the sync
    one — same captured fields, same canonical error classification,
    same throttling auto-emit. Customers using ``AsyncAnthropic`` get
    full provider_incident + infrastructure_throttled coverage.

Streaming coverage (#271.i):

  - ``Messages.stream()`` and ``AsyncMessages.stream()`` are also
    patched. The wrapper returns a proxy MessageStreamManager that
    delegates iteration to the original stream while injecting an
    llm_call event emission at stream close (via the inner manager's
    ``get_final_message()`` helper). Customer's iteration protocol
    preserved — ``with client.messages.stream(...) as stream: for
    event in stream:`` works unchanged. Mid-stream exceptions are
    captured via ``exc_val`` on __exit__/__aexit__.

Still out of scope for this slice:

  - Anthropic tools / tool_use response blocks, handled by @mesedi.tool
    at the agent layer, not at the LLM-call layer

Patching is idempotent: calling ``instrument_anthropic()`` twice has no
additional effect.

Dependency injection: ``instrument_anthropic()`` accepts an optional
``messages_class`` parameter so this code path is testable without
installing the actual ``anthropic`` package. Pass any class that has a
``create`` method to patch, the sandbox test does this with a fake
class to verify the patching logic end-to-end.
"""

from __future__ import annotations

import logging
import time
import uuid
from typing import Any, List, Optional, Type

from mesedi._context import current_execution_context
from mesedi.client import get_client
from mesedi.errors import (
    classify_anthropic_exception,
    extract_http_status,
    extract_retry_after,
)
from mesedi.events import Event, EventType, utcnow_rfc3339
from mesedi.observe import _maybe_emit_throttling_event

# Stable lowercase provider identifier shipped on every llm_call
# event emitted by this integration. Backend detectors (e.g.
# provider_incident) cluster cross-tenant signals on (provider,
# error_class), so this string must NOT change between SDK versions
# without a coordinated backend change.
_PROVIDER = "anthropic"

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


def instrument_anthropic(
    messages_class: Optional[Type[Any]] = None,
    async_messages_class: Optional[Type[Any]] = None,
) -> bool:
    """Patch the Anthropic SDK to emit llm_call events on both sync + async paths.

    Args:
        messages_class: The sync class whose ``create`` method should
            be patched. When ``None`` (the default), tries to import
            ``anthropic.resources.messages.Messages``. Passing an
            explicit class is intended for testing.
        async_messages_class: The async class whose ``create`` method
            should be patched. When ``None`` (the default), tries to
            import ``anthropic.resources.messages.AsyncMessages``. An
            ImportError on the async class logs + skips async patching;
            the sync path is unaffected. (Older Anthropic SDK versions
            without the async client are still partially supported via
            the sync path.)

    Returns:
        True if at least one of the sync/async paths was successfully
        patched (or was a no-op because the class is already patched).
        False if neither path could be patched (e.g. anthropic is not
        installed at all).
    """
    sync_ok = _instrument_sync_anthropic(messages_class)
    async_ok = _instrument_async_anthropic(async_messages_class)
    # #271.i: also patch the .stream() methods on both Messages
    # classes if available. These are streaming surfaces that bypass
    # the create() patches entirely. The patchers locate the
    # already-resolved Messages / AsyncMessages classes via the
    # _patched_classes set; we re-resolve here so this function is
    # idempotent even if .stream() patching runs after create()
    # patching on the same class.
    _patch_anthropic_sync_stream(messages_class)
    _patch_anthropic_async_stream(async_messages_class)
    return sync_ok or async_ok


def _instrument_sync_anthropic(messages_class: Optional[Type[Any]]) -> bool:
    """Patch the sync Messages.create path. Extracted from
    instrument_anthropic so the sync + async paths can be patched
    independently and either one can no-op cleanly if its target class
    isn't importable in the installed Anthropic SDK version."""
    if messages_class is None:
        try:
            from anthropic.resources.messages import Messages as _Messages
            messages_class = _Messages
        except ImportError:
            logger.warning(
                "mesedi: anthropic package not importable; "
                "instrument_anthropic() sync path is a no-op. "
                "Install with `pip install anthropic` to enable."
            )
            return False

    if messages_class in _patched_classes:
        return True

    original_create = messages_class.create

    def patched_create(self: Any, *args: Any, **kwargs: Any) -> Any:
        ctx = current_execution_context()
        if ctx is None:
            # No active execution, run unobserved. Same fail-open
            # pattern as @tool and @wrap.
            return original_create(self, *args, **kwargs)

        # Halt-safe boundary: budget check runs BEFORE the LLM call,
        # so a halt fires at this checkpoint rather than mid-API-call.
        ctx.check_budget()

        client = get_client()
        sequence = ctx.next_sequence()
        event_id = f"evt-{uuid.uuid4().hex[:12]}"
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.increment_steps()

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
            # Build the failure payload using the canonical
            # cross-provider vocabulary in mesedi.errors. provider
            # lets the backend group multi-provider signals;
            # error_class maps the native exception to one of eight
            # canonical buckets; http_status is included when the
            # exception exposes it (Anthropic APIStatusError
            # subclasses do, connection / timeout errors don't).
            failure_payload = {
                "provider": _PROVIDER,
                "model": model,
                "system_prompt": _truncate(system_text, _MAX_SYSTEM),
                "user_message": _truncate(user_message, _MAX_USER_MSG),
                "status": "failed",
                "error_class": classify_anthropic_exception(exc),
                "exception_type": type(exc).__name__,
                "exception_message": _truncate(str(exc), _MAX_EXC_MSG),
            }
            http_status = extract_http_status(exc)
            if http_status is not None:
                failure_payload["http_status"] = http_status
            # Provider-recommended back-off window. When present, the
            # backend surfaces it on the provider_incident failure
            # group so the customer's dashboard shows "back off N
            # seconds" alongside the incident itself.
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
            # Auto-emit a matching infrastructure_event when the
            # canonical error_class signals per-tenant throttling
            # (rate_limited or quota_exhausted). Different detector
            # path from the failed llm_call above: provider_incident
            # reads the llm_call; infrastructure_throttled reads the
            # infrastructure_event. Without this auto-emit the
            # infrastructure_throttled detector is silently inactive
            # for the default customer (Wave 1.4 audit gap closure).
            _maybe_emit_throttling_event(
                provider=_PROVIDER,
                error_class=failure_payload["error_class"],
                http_status=http_status,
                retry_after_seconds=retry_after,
                endpoint="/v1/messages",
            )
            raise

        duration_ms = int((time.perf_counter() - start) * 1000)
        response_text, input_tokens, output_tokens = _extract_response_fields(response)
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.add_tokens(
                tokens_in=input_tokens,
                tokens_out=output_tokens,
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

    # Preserve the original name + docstring on the wrapper so
    # introspection (help(), repr()) still shows useful info.
    patched_create.__name__ = getattr(original_create, "__name__", "create")
    patched_create.__doc__ = getattr(original_create, "__doc__", None)

    messages_class.create = patched_create  # type: ignore[assignment]
    _patched_classes.add(messages_class)
    return True


def _instrument_async_anthropic(async_messages_class: Optional[Type[Any]]) -> bool:
    """Patch AsyncAnthropic.messages.create — #271.h.

    Mirrors the sync wrapper's event-emit logic line-for-line; the
    only differences are (a) ``await`` on the underlying call and (b)
    ``async def`` on the wrapper. Same canonical error_class mapping,
    same retry_after extraction, same _maybe_emit_throttling_event
    auto-emit on rate_limited / quota_exhausted.

    Best-effort: an ImportError on AsyncMessages (older Anthropic SDK
    versions that don't ship it) logs at INFO + returns False; the
    sync path remains patched independently.
    """
    if async_messages_class is None:
        try:
            from anthropic.resources.messages import AsyncMessages as _AsyncMessages
            async_messages_class = _AsyncMessages
        except ImportError:
            logger.info(
                "mesedi: anthropic.resources.messages.AsyncMessages not importable; "
                "async client instrumentation skipped. Sync path is unaffected."
            )
            return False

    if async_messages_class in _patched_classes:
        return True

    original_acreate = async_messages_class.create

    async def patched_acreate(self: Any, *args: Any, **kwargs: Any) -> Any:
        ctx = current_execution_context()
        if ctx is None:
            return await original_acreate(self, *args, **kwargs)

        # Halt-safe boundary — same posture as sync wrapper.
        ctx.check_budget()

        client = get_client()
        sequence = ctx.next_sequence()
        event_id = f"evt-{uuid.uuid4().hex[:12]}"
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.increment_steps()

        model = kwargs.get("model", "unknown")
        system_raw = kwargs.get("system", "")
        messages = kwargs.get("messages", [])
        user_message = _extract_last_user_message(messages)
        system_text = system_raw if isinstance(system_raw, str) else str(system_raw)

        start = time.perf_counter()
        try:
            response = await original_acreate(self, *args, **kwargs)
        except BaseException as exc:
            duration_ms = int((time.perf_counter() - start) * 1000)
            failure_payload = {
                "provider": _PROVIDER,
                "model": model,
                "system_prompt": _truncate(system_text, _MAX_SYSTEM),
                "user_message": _truncate(user_message, _MAX_USER_MSG),
                "status": "failed",
                "error_class": classify_anthropic_exception(exc),
                "exception_type": type(exc).__name__,
                "exception_message": _truncate(str(exc), _MAX_EXC_MSG),
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
                endpoint="/v1/messages",
            )
            raise

        duration_ms = int((time.perf_counter() - start) * 1000)
        response_text, input_tokens, output_tokens = _extract_response_fields(response)
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.add_tokens(
                tokens_in=input_tokens,
                tokens_out=output_tokens,
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

    patched_acreate.__name__ = getattr(original_acreate, "__name__", "create")
    patched_acreate.__doc__ = getattr(original_acreate, "__doc__", None)

    async_messages_class.create = patched_acreate  # type: ignore[assignment]
    _patched_classes.add(async_messages_class)
    return True


## ── #271.i streaming patching ──────────────────────────────────────

# Streaming uses a separate idempotency sentinel because .stream() is
# a different METHOD on the same Messages / AsyncMessages classes
# than .create() (which is tracked in _patched_classes). A single set
# would falsely block stream patching when create patching had
# already added the class.
_stream_patched_classes: set = set()


def _patch_anthropic_sync_stream(messages_class: Optional[Type[Any]]) -> bool:
    """Patch Messages.stream() — sync streaming context manager."""
    if messages_class is None:
        try:
            from anthropic.resources.messages import Messages as _Messages
            messages_class = _Messages
        except ImportError:
            return False
    if messages_class in _stream_patched_classes:
        return True
    original_stream = getattr(messages_class, "stream", None)
    if original_stream is None:
        return False

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
        system_raw = kwargs.get("system", "")
        messages = kwargs.get("messages", [])
        user_message = _extract_last_user_message(messages)
        system_text = system_raw if isinstance(system_raw, str) else str(system_raw)

        start = time.perf_counter()
        try:
            manager = original_stream(self, *args, **kwargs)
        except BaseException as exc:
            # Request-time failure — no manager to wrap, no chunks
            # to aggregate. Emit failed event + throttling event +
            # re-raise, same as the non-streaming .create() failure
            # path.
            duration_ms = int((time.perf_counter() - start) * 1000)
            _emit_anthropic_stream_failure(
                client=client, ctx=ctx, event_id=event_id, sequence=sequence,
                duration_ms=duration_ms, model=model, system_text=system_text,
                user_message=user_message, exc=exc, async_mode=False,
            )
            raise

        return _AnthropicStreamManagerWrapper(
            inner=manager,
            ctx=ctx,
            client=client,
            event_id=event_id,
            sequence=sequence,
            model=model,
            system_text=system_text,
            user_message=user_message,
            start=start,
            async_mode=False,
        )

    patched_stream.__name__ = getattr(original_stream, "__name__", "stream")
    patched_stream.__doc__ = getattr(original_stream, "__doc__", None)
    messages_class.stream = patched_stream  # type: ignore[assignment]
    _stream_patched_classes.add(messages_class)
    return True


def _patch_anthropic_async_stream(async_messages_class: Optional[Type[Any]]) -> bool:
    """Patch AsyncMessages.stream() — async streaming context manager."""
    if async_messages_class is None:
        try:
            from anthropic.resources.messages import AsyncMessages as _AsyncMessages
            async_messages_class = _AsyncMessages
        except ImportError:
            return False
    if async_messages_class in _stream_patched_classes:
        return True
    original_stream = getattr(async_messages_class, "stream", None)
    if original_stream is None:
        return False

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
        system_raw = kwargs.get("system", "")
        messages = kwargs.get("messages", [])
        user_message = _extract_last_user_message(messages)
        system_text = system_raw if isinstance(system_raw, str) else str(system_raw)

        start = time.perf_counter()
        try:
            manager = original_stream(self, *args, **kwargs)
        except BaseException as exc:
            duration_ms = int((time.perf_counter() - start) * 1000)
            _emit_anthropic_stream_failure(
                client=client, ctx=ctx, event_id=event_id, sequence=sequence,
                duration_ms=duration_ms, model=model, system_text=system_text,
                user_message=user_message, exc=exc, async_mode=True,
            )
            raise

        return _AnthropicStreamManagerWrapper(
            inner=manager,
            ctx=ctx,
            client=client,
            event_id=event_id,
            sequence=sequence,
            model=model,
            system_text=system_text,
            user_message=user_message,
            start=start,
            async_mode=True,
        )

    patched_stream.__name__ = getattr(original_stream, "__name__", "stream")
    patched_stream.__doc__ = getattr(original_stream, "__doc__", None)
    async_messages_class.stream = patched_stream  # type: ignore[assignment]
    _stream_patched_classes.add(async_messages_class)
    return True


class _AnthropicStreamManagerWrapper:
    """Proxies an Anthropic MessageStreamManager (sync OR async) and
    emits the llm_call event at stream close.

    Customers' iteration protocol is preserved: __enter__ returns the
    actual inner MessageStream so `with stream() as stream: for event
    in stream:` works unchanged. The wrapper stays alive as the
    context manager so __exit__ (or __aexit__) fires when the
    customer's `with` / `async with` block exits.

    On close:
      - If the customer's block exited cleanly (exc_val is None), the
        wrapper calls inner.get_final_message() to extract the final
        response + token counts, then emits a status=ok llm_call event.
      - If the block exited with an exception (mid-stream failure),
        the wrapper builds a failure payload using the same canonical
        error_class mapping as the .create() patcher and emits a
        status=failed event + auto-fires the throttling event when
        applicable. The exception is re-raised through the inner's
        __exit__.
      - If the customer drops the stream early (exits before
        consuming all chunks), __exit__ still fires; get_final_message
        returns what was buffered (empty response_text + 0 tokens is
        acceptable — that's an accurate reflection of what the call
        delivered).
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
        async_mode: bool,
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
        self._async_mode = async_mode
        self._stream: Any = None
        self._emitted = False

    # Proxy any non-overridden attribute access to the inner manager.
    # Customers who use the manager's methods directly (rare; the
    # documented usage is `with`-blocks) get the original behavior.
    def __getattr__(self, name: str) -> Any:
        return getattr(self._inner, name)

    def __enter__(self) -> Any:
        self._stream = self._inner.__enter__()
        return self._stream

    def __exit__(self, exc_type: Any, exc_val: Any, exc_tb: Any) -> Any:
        self._emit_event(exc_val=exc_val, endpoint="/v1/messages")
        return self._inner.__exit__(exc_type, exc_val, exc_tb)

    async def __aenter__(self) -> Any:
        self._stream = await self._inner.__aenter__()
        return self._stream

    async def __aexit__(self, exc_type: Any, exc_val: Any, exc_tb: Any) -> Any:
        self._emit_event(exc_val=exc_val, endpoint="/v1/messages")
        return await self._inner.__aexit__(exc_type, exc_val, exc_tb)

    def _emit_event(self, *, exc_val: Optional[BaseException], endpoint: str) -> None:
        if self._emitted:
            return
        self._emitted = True
        duration_ms = int((time.perf_counter() - self._start) * 1000)
        if exc_val is not None:
            # Mid-stream failure — use same canonical error classifier
            # + throttling auto-emit as the .create() failure path.
            _emit_anthropic_stream_failure(
                client=self._client,
                ctx=self._ctx,
                event_id=self._event_id,
                sequence=self._sequence,
                duration_ms=duration_ms,
                model=self._model,
                system_text=self._system_text,
                user_message=self._user_message,
                exc=exc_val,
                async_mode=self._async_mode,
                endpoint=endpoint,
            )
            return
        # Success path — try to extract final message. Failure here
        # is defensive: customer may have dropped the stream early
        # leaving no final message available. Log + emit a partial
        # event with empty response_text + 0 tokens rather than
        # losing the event entirely.
        response_text = ""
        input_tokens = 0
        output_tokens = 0
        try:
            stream = self._stream if self._stream is not None else self._inner
            final = stream.get_final_message()
            response_text, input_tokens, output_tokens = _extract_response_fields(final)
        except Exception as emit_exc:
            logger.debug("mesedi: stream get_final_message failed: %s", emit_exc)
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


def _emit_anthropic_stream_failure(
    *,
    client: Any,
    ctx: Any,
    event_id: str,
    sequence: int,
    duration_ms: int,
    model: str,
    system_text: str,
    user_message: str,
    exc: BaseException,
    async_mode: bool,
    endpoint: str = "/v1/messages",
) -> None:
    """Emit a status=failed llm_call event + fire infrastructure_event
    auto-throttling. Shared between request-time failures (caught
    around original_stream() call) and mid-stream failures (caught
    via __exit__'s exc_val).
    """
    failure_payload = {
        "provider": _PROVIDER,
        "model": model,
        "system_prompt": _truncate(system_text, _MAX_SYSTEM),
        "user_message": _truncate(user_message, _MAX_USER_MSG),
        "status": "failed",
        "error_class": classify_anthropic_exception(exc),
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
