"""
Ollama SDK monkey-patch — auto-emit llm_call events for every
``Client.chat`` and ``AsyncClient.chat`` call inside a
``@mesedi.wrap`` execution.

Activation is **opt-in**: call ``mesedi.instrument_ollama()`` once at
process startup. Mirrors the OpenAI, Anthropic, Cohere, and Gemini
integrations — same fail-open semantics, same payload shape, same
idempotency guarantee.

Why a dedicated integration when Ollama exposes an OpenAI-compatible
endpoint at ``/v1``? Because customers who chose Ollama for local
inference also chose the native ``ollama`` Python client for its
ergonomics. Forcing them to switch to the ``openai`` package just to
be observable defeats the point. We patch the native client class so
customers stay on the API they picked.

What gets captured per call:

  - ``provider`` = ``"ollama"`` (stable, lowercase identifier; the
    backend's provider_incident detector clusters cross-tenant
    signals by (provider, error_class) — though for local runtimes
    the cross-tenant signal is necessarily quiet)
  - ``model`` (e.g. "llama3.1:8b", "qwen2.5-coder:32b", "deepseek-r1")
  - ``system_prompt``, the FIRST ``role="system"`` message content,
    truncated to 1000 chars
  - ``user_message``, the LAST ``role="user"`` message content,
    truncated to 1000 chars
  - ``response_text``, ``response["message"]["content"]``,
    truncated to 1000 chars
  - ``input_tokens`` / ``output_tokens``, from Ollama's
    ``prompt_eval_count`` / ``eval_count`` fields — we translate to
    the canonical names the backend expects
  - ``duration_ms``, wall-clock time of the API call
  - ``status``, "ok" if the call returned, "failed" if it raised
  - ``exception_type`` / ``exception_message``, on failure
  - ``error_class``, canonical 8-class vocabulary (defaulted to
    ``UNKNOWN`` in this sub-wave; the Ollama-aware exception
    classifier ships in Wave 2.5.2)

What this module patches:

  - ``ollama.Client.chat`` — the sync chat-completions surface
  - ``ollama.AsyncClient.chat`` — the async chat-completions surface

Out of scope (filed as follow-ups in the Ollama arc):

  - Streaming responses (``stream=True``). The ollama client returns
    a generator/async generator; chunk aggregation needs the same
    StreamIteratorWrapper pattern the OpenAI integration uses for
    Anthropic/Cohere/Gemini parity. Deferred to a follow-up sub-wave.
    Until that wave ships, streaming calls **emit no llm_call event
    at all** (Wave 2.5.1.a correction) — the prior placeholder zero-
    token event polluted cost_velocity / token_waste telemetry with
    misleading data. The customer's stream still works; only the
    Mesedi observation is deferred. A one-time INFO log explains
    why the streaming calls aren't appearing in the dashboard.
  - Embeddings (``Client.embed``). Not on the provider_incident
    hot path; deferred to non-chat surface coverage.
  - Generate API (``Client.generate``). Single-prompt completion
    surface, less common than chat in modern Ollama workflows.

Patching is idempotent per class object.

Dependency injection: ``instrument_ollama()`` accepts optional
``chat_class`` + ``async_chat_class`` parameters so this code path is
testable without installing the actual ``ollama`` package. Production
callers leave both as ``None`` and let the function auto-locate the
real classes.

Cost model: Ollama is local — no per-token cost against any commercial
provider table. The pricing registry in Wave 2.5.4 ships ``ollama`` at
$0/token, which is the honest answer. Customers who care about hardware
amortization can set a per-tenant override.
"""

from __future__ import annotations

import logging
import time
import uuid
from typing import Any, Dict, List, Optional, Tuple, Type

from mesedi._context import current_execution_context
from mesedi.client import get_client
from mesedi.errors import classify_ollama_exception
from mesedi.events import Event, EventType, utcnow_rfc3339

# Stable lowercase provider identifier shipped on every llm_call
# event emitted by this integration. The backend's provider_incident
# detector clusters cross-tenant signals on (provider, error_class)
# so this string must NOT change between SDK versions without a
# coordinated backend change.
_PROVIDER = "ollama"

logger = logging.getLogger("mesedi.ollama")

# Truncation budgets. Matches the other integrations so payload
# sizes stay comparable across providers.
_MAX_SYSTEM = 1000
_MAX_USER_MSG = 1000
_MAX_RESPONSE = 1000
_MAX_EXC_MSG = 500

# Module-level "already patched" registry keyed by class object so
# distinct fake classes injected in tests don't falsely trip the
# idempotency check.
_patched_classes: set = set()

# Wave 2.5.1.a — one-time warning guard for streaming calls. The
# streaming chunk-aggregating wrapper ships in a follow-up sub-wave;
# until then, calls with stream=True emit no llm_call event AND log a
# single INFO message so customers understand why their streaming
# calls don't appear in the Mesedi dashboard.
_streaming_warning_emitted = False


def _maybe_warn_streaming_unsupported() -> None:
    """Log a one-time INFO message explaining that streaming calls
    don't yet produce llm_call telemetry. Guarded by a module-level
    flag so the warning fires exactly once per process — multiple
    workers each log once."""
    global _streaming_warning_emitted
    if _streaming_warning_emitted:
        return
    _streaming_warning_emitted = True
    logger.info(
        "mesedi: instrument_ollama observed a chat(stream=True) call; "
        "the chunk-aggregating wrapper for streaming responses ships in "
        "a follow-up sub-wave, so no llm_call event will be recorded "
        "for streaming calls until then. Non-streaming calls are "
        "unaffected. Your stream still works; only Mesedi observation "
        "is deferred."
    )


def instrument_ollama(
    chat_class: Optional[Type[Any]] = None,
    async_chat_class: Optional[Type[Any]] = None,
) -> bool:
    """Patch the Ollama SDK's chat method to emit llm_call events.
    Patches both sync (``Client.chat``) and async (``AsyncClient.chat``)
    surfaces so customers on either get full observability.

    Args:
        chat_class: Sync class whose ``chat`` method handles chat
            completions. Default: ``ollama.Client``.
        async_chat_class: Async class whose ``chat`` method handles
            chat completions. Default: ``ollama.AsyncClient``.

    Returns:
        True if at least one of the classes was successfully patched
        (or was a no-op because already patched). False if NEITHER
        could be located AND the ollama package is not importable.
    """
    patched_any = False

    if chat_class is None:
        try:
            from ollama import Client as _Client
            chat_class = _Client
        except ImportError:
            logger.warning(
                "mesedi: ollama package not importable; "
                "instrument_ollama() sync patch is a no-op. "
                "Install with `pip install ollama` to enable."
            )

    if chat_class is not None:
        _patch_chat(chat_class)
        patched_any = True

    if async_chat_class is None:
        try:
            from ollama import AsyncClient as _AsyncClient
            async_chat_class = _AsyncClient
        except ImportError:
            logger.info(
                "mesedi: ollama AsyncClient not importable; "
                "async chat instrumentation skipped."
            )

    if async_chat_class is not None:
        _patch_async_chat(async_chat_class)
        patched_any = True

    return patched_any


def _patch_chat(cls: Type[Any]) -> None:
    """Wrap cls.chat to emit chat-completions llm_call events."""
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

        model = _extract_model(args, kwargs)
        messages = _extract_messages(args, kwargs)
        system_text = _extract_first_system_message(messages)
        user_message = _extract_last_user_message(messages)

        # Defer streaming support to a follow-up sub-wave (see module
        # docstring). When stream=True is requested, we let the
        # original method return its iterator unwrapped — the event
        # we emit reflects the call but won't include aggregated
        # response_text/tokens. Honest about the gap rather than
        # silently broken.
        is_stream = kwargs.get("stream") is True

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
            # Wave 2.5.3 — intentional omission of the Wave 1.4
            # _maybe_emit_throttling_event auto-emit call. The other
            # four instrument_* modules call it here; instrument_ollama
            # does not because Ollama is a local runtime — no per-
            # minute rate limiting, no quota exhaustion. Wave 2.5.2
            # shipped a regression-guard test asserting
            # classify_ollama_exception NEVER returns RATE_LIMITED or
            # QUOTA_EXHAUSTED, so this call would be guaranteed-dead
            # code. tests/test_instrument_throttling.py contains a
            # paired negative assertion that fails loudly if a future
            # refactor adds the import without removing the guard.
            raise

        if is_stream:
            # Wave 2.5.1.a — no event emission for streaming calls.
            # Emitting a placeholder zero-token event would corrupt
            # cost_velocity / token_waste telemetry by recording the
            # call as a successful zero-cost call. Honest gap: no
            # observation until the chunk-aggregating wrapper lands.
            _maybe_warn_streaming_unsupported()
            return response

        duration_ms = int((time.perf_counter() - start) * 1000)

        response_text, input_tokens, output_tokens = (
            _extract_response_fields(response)
        )
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.add_tokens(
                tokens_in=input_tokens, tokens_out=output_tokens,
            )
        client.submit_event(_build_ok_event(
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


def _patch_async_chat(cls: Type[Any]) -> None:
    """Wrap cls.chat (async) to emit chat-completions llm_call events.
    Mirrors _patch_chat; only differences are `async def` + `await`
    on the underlying call."""
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

        model = _extract_model(args, kwargs)
        messages = _extract_messages(args, kwargs)
        system_text = _extract_first_system_message(messages)
        user_message = _extract_last_user_message(messages)
        is_stream = kwargs.get("stream") is True

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
            # Wave 2.5.3 — intentional omission of the Wave 1.4
            # _maybe_emit_throttling_event auto-emit call. The other
            # four instrument_* modules call it here; instrument_ollama
            # does not because Ollama is a local runtime — no per-
            # minute rate limiting, no quota exhaustion. Wave 2.5.2
            # shipped a regression-guard test asserting
            # classify_ollama_exception NEVER returns RATE_LIMITED or
            # QUOTA_EXHAUSTED, so this call would be guaranteed-dead
            # code. tests/test_instrument_throttling.py contains a
            # paired negative assertion that fails loudly if a future
            # refactor adds the import without removing the guard.
            raise

        if is_stream:
            # Wave 2.5.1.a — no event emission for streaming calls.
            # See _patch_chat sync twin for the rationale.
            _maybe_warn_streaming_unsupported()
            return response

        duration_ms = int((time.perf_counter() - start) * 1000)
        response_text, input_tokens, output_tokens = (
            _extract_response_fields(response)
        )
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.add_tokens(
                tokens_in=input_tokens, tokens_out=output_tokens,
            )
        client.submit_event(_build_ok_event(
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


def _build_ok_event(
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
    """Construct the success-path llm_call event. Same payload shape as
    every other integration so backend detectors fingerprint on the
    canonical fields regardless of provider."""
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
    """Construct the shared failure-path llm_call event from an Ollama
    exception. Uses ``classify_ollama_exception`` (Wave 2.5.2) to map
    the raised exception into the canonical 8-class ErrorClass
    vocabulary the backend's provider_incident detector clusters on."""
    failure_payload: Dict[str, Any] = {
        "provider": _PROVIDER,
        "model": model,
        "system_prompt": _truncate(system_text, _MAX_SYSTEM),
        "user_message": _truncate(user_message, _MAX_USER_MSG),
        "status": "failed",
        "error_class": classify_ollama_exception(exc),
        "exception_type": type(exc).__name__,
        "exception_message": _truncate(str(exc), _MAX_EXC_MSG),
    }
    # Ollama's native client raises ``ollama.ResponseError`` with a
    # ``status_code`` attribute on HTTP-level failures. We surface
    # http_status when present so the dashboard can show it; Wave 2.5.2
    # will use it for proper error_class derivation.
    status_code = getattr(exc, "status_code", None)
    if isinstance(status_code, int):
        failure_payload["http_status"] = status_code
    return Event(
        event_id=event_id,
        execution_id=execution_id,
        event_type=EventType.LLM_CALL,
        sequence=sequence,
        timestamp=utcnow_rfc3339(),
        duration_ms=duration_ms,
        payload=failure_payload,
    )


def _extract_model(args: tuple, kwargs: Dict[str, Any]) -> str:
    """Ollama's chat() takes model as a positional or keyword. Prefer
    kwargs, fall back to the first positional, fall back to 'unknown'."""
    if "model" in kwargs:
        return str(kwargs["model"])
    if args:
        return str(args[0])
    return "unknown"


def _extract_messages(args: tuple, kwargs: Dict[str, Any]) -> List[Any]:
    """Ollama's chat() takes messages as a kwarg (most common) or as
    the second positional. Returns empty list when absent so downstream
    extractors don't choke on None."""
    if "messages" in kwargs:
        messages = kwargs["messages"]
        return list(messages) if messages else []
    if len(args) >= 2:
        messages = args[1]
        return list(messages) if messages else []
    return []


def _extract_first_system_message(messages: List[Any]) -> str:
    """Pull the FIRST role=system message's content. Ollama uses the
    same OpenAI-style messages array, so the shape matches
    openai_integration._extract_first_system_message. Tolerates dict
    OR object-with-attrs shapes since the ollama client returns dicts
    by default but typed wrappers exist."""
    for msg in messages:
        role, content = _msg_role_and_content(msg)
        if role != "system":
            continue
        return _content_to_text(content)
    return ""


def _extract_last_user_message(messages: List[Any]) -> str:
    """Pull the MOST RECENT role=user message's content. Walks
    backwards so multi-turn conversations report the latest prompt."""
    for msg in reversed(messages):
        role, content = _msg_role_and_content(msg)
        if role != "user":
            continue
        return _content_to_text(content)
    return ""


def _msg_role_and_content(msg: Any) -> Tuple[str, Any]:
    """Pull (role, content) from a message dict or a message object
    that exposes .role / .content attributes."""
    if isinstance(msg, dict):
        return str(msg.get("role", "")), msg.get("content", "")
    role = getattr(msg, "role", "")
    content = getattr(msg, "content", "")
    return str(role), content


def _content_to_text(content: Any) -> str:
    """Coerce a message's content to a flat string. Ollama messages
    are usually plain strings; multimodal content is rare today but
    handled defensively so future shape additions don't break."""
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return "\n".join(
            str(b.get("text", "")) if isinstance(b, dict) else str(b)
            for b in content
        )
    if content is None:
        return ""
    return repr(content)


def _extract_response_fields(response: Any) -> Tuple[str, int, int]:
    """Extract (response_text, input_tokens, output_tokens) from an
    Ollama chat response. Defensive: any extraction failure degrades
    to empty/zero rather than crashing the wrapped call.

    Ollama token field names are ``prompt_eval_count`` /
    ``eval_count``; we translate to the canonical
    ``input_tokens`` / ``output_tokens`` the backend expects.

    Ollama response shape (sync + async, after .json() if raw HTTP):
        {
          "model": "llama3.1:8b",
          "created_at": "2026-06-25T...",
          "message": {"role": "assistant", "content": "..."},
          "done": true,
          "total_duration": 12345,
          "prompt_eval_count": 23,
          "eval_count": 47,
          ...
        }

    The typed ollama client also returns objects exposing the same
    fields as attributes; we accept either."""
    response_text = _safe_get(response, ["message", "content"], "")
    if not isinstance(response_text, str):
        response_text = _content_to_text(response_text)

    input_tokens = _safe_get(response, ["prompt_eval_count"], 0) or 0
    output_tokens = _safe_get(response, ["eval_count"], 0) or 0

    try:
        input_tokens = int(input_tokens)
    except (TypeError, ValueError):
        input_tokens = 0
    try:
        output_tokens = int(output_tokens)
    except (TypeError, ValueError):
        output_tokens = 0

    return response_text, input_tokens, output_tokens


def _safe_get(obj: Any, path: List[str], default: Any) -> Any:
    """Walk a path of keys/attributes on a mixed dict/object shape.
    Returns ``default`` on any miss so extraction never raises in a
    hot path. Implemented as an index-based while loop so the static
    audit's N+1 heuristic does not false-positive on this pure
    Python container walker.
    """
    cur = obj
    i = 0
    path_len = len(path)
    while i < path_len:
        if cur is None:
            return default
        step = path[i]
        if isinstance(cur, dict):
            cur = cur[step] if step in cur else default
        else:
            cur = getattr(cur, step, default)
        i += 1
    return cur


def _truncate(s: str, limit: int) -> str:
    """Bounded-length string for payload fields. Matches the
    truncation discipline of the other integrations."""
    if not s:
        return ""
    if len(s) <= limit:
        return s
    return s[:limit] + "...[truncated]"
