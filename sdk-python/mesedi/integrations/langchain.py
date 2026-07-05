"""LangChain callback handler that emits Mesedi telemetry.

Usage:

    import mesedi
    from mesedi.integrations.langchain import MesediCallbackHandler

    @mesedi.wrap
    def run_agent(question: str) -> str:
        chain = build_chain()
        result = chain.invoke(
            {"input": question},
            config={"callbacks": [MesediCallbackHandler()]},
        )
        return result["output"]

Design:

The ``@mesedi.wrap`` decorator manages the execution boundary
(execution_started, execution_completed, crash signature). The
callback handler emits the intra-execution events, ``llm_call``
and ``tool_call``, that Mesedi's detectors consume. Splitting
responsibility this way lets the customer adopt Mesedi without
reshaping their LangChain code: the @wrap goes around their entry
point, the callback gets attached at the existing ``callbacks=``
slot the LangChain config already accepts.

LangChain's import path has churned over versions. We try the
``langchain-core`` location first (modern), then fall back to the
legacy ``langchain`` location, then to a stub (so the module
imports cleanly even when neither package is installed, tests can
exercise the translation logic by calling the handler's methods
directly).

The handler is duck-typed against LangChain's BaseCallbackHandler:
it implements the methods LangChain dispatches to, with the
keyword-argument signatures recent versions use (``run_id``,
``parent_run_id``, plus ``**kwargs`` for forward-compat). When
LangChain is installed it subclasses the real base; when not it
subclasses a local stub. Either way, the methods do the same work.

Out of scope for this slice:
  - Streaming responses (on_llm_new_token), receivers see only
    the final assembled response. Streaming attribution is a v2
    concern that needs an event-payload schema change.
  - Async callbacks (async on_llm_start etc), sync only for slice
    1; async parity follows when we wire up async-shipper.
  - Per-chain depth tracking, every chain on_chain_start fires,
    but we ignore them because @wrap already owns the execution
    boundary. A chain-as-execution mode (no @wrap required) is a
    later iteration.
  - Multi-modal content blocks (images in messages), we extract
    the text part and ignore the rest.
"""

from __future__ import annotations

import time
import uuid
from dataclasses import dataclass
from typing import Any, Dict, List, Optional

from mesedi._context import current_execution_context
from mesedi.client import get_client
from mesedi.events import Event, EventType, utcnow_rfc3339
from mesedi.observe import emit_llm_call


# Lazy import of LangChain's BaseCallbackHandler. The integration is
# OPTIONAL, importing this module does not require langchain to be
# installed. Customers who actually use it will have langchain in
# their environment; tests stub it out by calling the handler's
# methods directly.
_LC_AVAILABLE = False
try:
    from langchain_core.callbacks import BaseCallbackHandler  # type: ignore  # noqa: F401
    _LC_AVAILABLE = True
except ImportError:
    try:
        from langchain.callbacks.base import BaseCallbackHandler  # type: ignore  # noqa: F401
        _LC_AVAILABLE = True
    except ImportError:
        # Provide a stub so the class can be defined and tested even
        # when langchain is not installed. LangChain's dispatcher will
        # never call into this stub (because the customer's code path
        # would import langchain itself, making _LC_AVAILABLE=True),
        # but our sandbox test exercises the handler methods directly.
        class BaseCallbackHandler:  # type: ignore[no-redef]
            """Stub used when langchain is not installed."""
            pass


# Truncation budgets, kept in sync with mesedi.observe.emit_llm_call
# and mesedi.tool so wire-format payloads from this adapter and from
# hand-written code are indistinguishable.
_MAX_TOOL_INPUT_REPR = 200
_MAX_TOOL_OUTPUT_REPR = 500
_MAX_EXC_MSG = 500


@dataclass
class _LLMStartContext:
    """In-flight state for an LLM run, keyed by LangChain's run_id."""

    model: str
    user_message: str
    system_prompt: str
    started_at: float


@dataclass
class _ToolStartContext:
    """In-flight state for a tool run."""

    name: str
    input_str: str
    started_at: float


class MesediCallbackHandler(BaseCallbackHandler):
    """LangChain callback handler that emits Mesedi events.

    Attach to any LangChain chain / agent / runnable via the standard
    ``callbacks=`` config slot:

        chain.invoke(
            {"input": ...},
            config={"callbacks": [MesediCallbackHandler()]},
        )

    Emits one ``llm_call`` event per LLM invocation (matching the
    wire format from ``emit_llm_call`` and the Anthropic patch) and
    one ``tool_call`` event per tool invocation (matching the wire
    format from ``@mesedi.tool``). Both event types feed the standard
    Mesedi detector chain, drift, identical/similar-call loops,
    tool-failures, cost-velocity, prompt-injection.

    Outside a ``@mesedi.wrap`` execution, all emissions silently
    no-op (matching the rest of Mesedi's observe layer).
    """

    # Tell LangChain's dispatcher this handler is safe to share
    # across runs and inherits across chains. Required attribute on
    # recent LangChain versions; ignored by older ones.
    raise_error: bool = False

    def __init__(self) -> None:
        super().__init__()
        # run_id → start context. LangChain assigns each LLM / tool
        # invocation a UUID4 run_id; on_llm_end / on_tool_end echo it
        # back so we can pair start+end and compute duration.
        self._llm_starts: Dict[Any, _LLMStartContext] = {}
        self._tool_starts: Dict[Any, _ToolStartContext] = {}

    # ── LLM events ──────────────────────────────────────────────────

    def on_llm_start(
        self,
        serialized: Dict[str, Any],
        prompts: List[str],
        *,
        run_id: Any,
        parent_run_id: Optional[Any] = None,
        **kwargs: Any,
    ) -> None:
        """Plain (non-chat) LLM invocation started.

        ``prompts`` is a list of completed prompt strings (LangChain
        has already done its template substitution). We record the
        last prompt as ``user_message``, for the common case of a
        single-prompt invocation that IS the prompt; for the rare
        multi-prompt case we record the last one and accept the
        truncation.
        """
        model = self._extract_model(serialized, kwargs)
        user_message = prompts[-1] if prompts else ""
        self._llm_starts[run_id] = _LLMStartContext(
            model=model,
            user_message=user_message,
            system_prompt="",
            started_at=time.perf_counter(),
        )

    def on_chat_model_start(
        self,
        serialized: Dict[str, Any],
        messages: List[List[Any]],
        *,
        run_id: Any,
        parent_run_id: Optional[Any] = None,
        **kwargs: Any,
    ) -> None:
        """Chat-model invocation started.

        ``messages`` is a list of conversations (LangChain supports
        batched invocation); each conversation is a list of
        BaseMessage objects (System / Human / AI / Tool). We extract
        the last user message and the last system message to match
        emit_llm_call's wire format.
        """
        model = self._extract_model(serialized, kwargs)
        last_conversation = messages[-1] if messages else []
        user_msg, system_msg = self._extract_role_messages(last_conversation)
        self._llm_starts[run_id] = _LLMStartContext(
            model=model,
            user_message=user_msg,
            system_prompt=system_msg,
            started_at=time.perf_counter(),
        )

    def on_llm_end(
        self,
        response: Any,
        *,
        run_id: Any,
        parent_run_id: Optional[Any] = None,
        **kwargs: Any,
    ) -> None:
        ctx = self._llm_starts.pop(run_id, None)
        if ctx is None:
            return
        response_text = self._extract_response_text(response)
        input_tokens, output_tokens = self._extract_token_usage(response)
        duration_ms = int((time.perf_counter() - ctx.started_at) * 1000)
        emit_llm_call(
            model=ctx.model,
            user_message=ctx.user_message,
            system_prompt=ctx.system_prompt,
            response_text=response_text,
            input_tokens=input_tokens,
            output_tokens=output_tokens,
            duration_ms=duration_ms,
            status="ok",
        )

    def on_llm_error(
        self,
        error: BaseException,
        *,
        run_id: Any,
        parent_run_id: Optional[Any] = None,
        **kwargs: Any,
    ) -> None:
        ctx = self._llm_starts.pop(run_id, None)
        if ctx is None:
            return
        duration_ms = int((time.perf_counter() - ctx.started_at) * 1000)
        emit_llm_call(
            model=ctx.model,
            user_message=ctx.user_message,
            system_prompt=ctx.system_prompt,
            response_text="",
            input_tokens=0,
            output_tokens=0,
            duration_ms=duration_ms,
            status="failed",
        )

    # ── Tool events ─────────────────────────────────────────────────

    def on_tool_start(
        self,
        serialized: Dict[str, Any],
        input_str: str,
        *,
        run_id: Any,
        parent_run_id: Optional[Any] = None,
        **kwargs: Any,
    ) -> None:
        name = self._extract_tool_name(serialized)
        self._tool_starts[run_id] = _ToolStartContext(
            name=name,
            input_str=input_str if isinstance(input_str, str) else repr(input_str),
            started_at=time.perf_counter(),
        )

    def on_tool_end(
        self,
        output: Any,
        *,
        run_id: Any,
        parent_run_id: Optional[Any] = None,
        **kwargs: Any,
    ) -> None:
        ctx = self._tool_starts.pop(run_id, None)
        if ctx is None:
            return
        duration_ms = int((time.perf_counter() - ctx.started_at) * 1000)
        self._emit_tool_event(
            tool_name=ctx.name,
            input_str=ctx.input_str,
            result_summary=str(output) if output is not None else "",
            duration_ms=duration_ms,
            status="ok",
        )

    def on_tool_error(
        self,
        error: BaseException,
        *,
        run_id: Any,
        parent_run_id: Optional[Any] = None,
        **kwargs: Any,
    ) -> None:
        ctx = self._tool_starts.pop(run_id, None)
        if ctx is None:
            return
        duration_ms = int((time.perf_counter() - ctx.started_at) * 1000)
        self._emit_tool_event(
            tool_name=ctx.name,
            input_str=ctx.input_str,
            result_summary="",
            duration_ms=duration_ms,
            status="failed",
            exception_type=type(error).__name__,
            exception_message=str(error),
        )

    # ── Extractors ──────────────────────────────────────────────────
    #
    # LangChain's serialized-object shape and message-class hierarchy
    # have shifted across versions. Each extractor below tries the
    # known shapes in order and falls through to a safe default so
    # the adapter still emits a useful event on a version we haven't
    # seen.

    @staticmethod
    def _extract_model(serialized: Dict[str, Any], kwargs: Dict[str, Any]) -> str:
        """Pull the model identifier from LangChain's serialized payload.

        Tried in order: invocation_params.model, kwargs.invocation_params.model,
        serialized.kwargs.model, serialized.kwargs.model_name, serialized.id (last segment),
        serialized.name.
        """
        inv = kwargs.get("invocation_params") or {}
        for key in ("model", "model_name", "deployment_name"):
            v = inv.get(key)
            if isinstance(v, str) and v:
                return v
        if isinstance(serialized, dict):
            kw = serialized.get("kwargs") or {}
            for key in ("model", "model_name", "deployment_name"):
                v = kw.get(key)
                if isinstance(v, str) and v:
                    return v
            ident = serialized.get("id")
            if isinstance(ident, list) and ident:
                last = ident[-1]
                if isinstance(last, str):
                    return last
            name = serialized.get("name")
            if isinstance(name, str) and name:
                return name
        return "unknown"

    @staticmethod
    def _extract_role_messages(conversation: List[Any]) -> tuple[str, str]:
        """Return (last_user_message, last_system_message) from a
        LangChain conversation list. Handles both string content and
        the newer multi-modal list-of-blocks content.
        """
        user_msg = ""
        system_msg = ""
        for msg in conversation:
            msg_type = getattr(msg, "type", None) or msg.__class__.__name__.lower()
            content = getattr(msg, "content", "")
            if isinstance(content, list):
                # Multi-modal: pull out text-typed blocks, join them.
                text_parts: List[str] = []
                for block in content:
                    if isinstance(block, dict):
                        if block.get("type") == "text" and isinstance(block.get("text"), str):
                            text_parts.append(block["text"])
                    elif isinstance(block, str):
                        text_parts.append(block)
                content = " ".join(text_parts)
            if not isinstance(content, str):
                content = str(content)
            if msg_type in ("system", "systemmessage"):
                system_msg = content
            elif msg_type in ("human", "humanmessage", "user", "usermessage"):
                user_msg = content
        return user_msg, system_msg

    @staticmethod
    def _extract_response_text(response: Any) -> str:
        """Pull assembled response text out of a LangChain LLMResult.

        LLMResult.generations is List[List[Generation]]. Each
        Generation has either .text (string) or .message.content
        (chat). We take the first generation of the first batch.
        """
        try:
            generations = getattr(response, "generations", None) or []
            if not generations:
                return ""
            first_batch = generations[0]
            if not first_batch:
                return ""
            gen = first_batch[0]
            text = getattr(gen, "text", None)
            if isinstance(text, str) and text:
                return text
            message = getattr(gen, "message", None)
            if message is not None:
                content = getattr(message, "content", None)
                if isinstance(content, str):
                    return content
                if isinstance(content, list):
                    parts: List[str] = []
                    for block in content:
                        if isinstance(block, dict):
                            if block.get("type") == "text" and isinstance(block.get("text"), str):
                                parts.append(block["text"])
                        elif isinstance(block, str):
                            parts.append(block)
                    return " ".join(parts)
        except Exception:
            # Defensive: anything goes wrong, fall through to empty.
            pass
        return ""

    @staticmethod
    def _extract_token_usage(response: Any) -> tuple[int, int]:
        """Pull (input_tokens, output_tokens) from a LangChain LLMResult.

        Token usage lives in response.llm_output['token_usage'] for
        most providers; some providers use 'usage' or attach it to
        individual generations' .usage_metadata. Try the common
        shapes; default to (0, 0) on any mismatch.
        """
        try:
            llm_output = getattr(response, "llm_output", None) or {}
            usage: Dict[str, Any] = {}
            if isinstance(llm_output, dict):
                usage = (
                    llm_output.get("token_usage")
                    or llm_output.get("usage")
                    or {}
                )
            input_tokens = (
                int(usage.get("prompt_tokens", 0) or 0)
                or int(usage.get("input_tokens", 0) or 0)
            )
            output_tokens = (
                int(usage.get("completion_tokens", 0) or 0)
                or int(usage.get("output_tokens", 0) or 0)
            )
            if input_tokens or output_tokens:
                return input_tokens, output_tokens
            # Newer LangChain: usage_metadata on the generation itself.
            generations = getattr(response, "generations", None) or []
            if generations and generations[0]:
                gen = generations[0][0]
                message = getattr(gen, "message", None)
                if message is not None:
                    meta = getattr(message, "usage_metadata", None)
                    if isinstance(meta, dict):
                        return (
                            int(meta.get("input_tokens", 0) or 0),
                            int(meta.get("output_tokens", 0) or 0),
                        )
        except Exception:
            pass
        return 0, 0

    @staticmethod
    def _extract_tool_name(serialized: Dict[str, Any]) -> str:
        """Pull the tool name from LangChain's serialized payload."""
        if not isinstance(serialized, dict):
            return "unknown_tool"
        name = serialized.get("name")
        if isinstance(name, str) and name:
            return name
        ident = serialized.get("id")
        if isinstance(ident, list) and ident:
            last = ident[-1]
            if isinstance(last, str):
                return last
        return "unknown_tool"

    # ── Emission helper ─────────────────────────────────────────────

    @staticmethod
    def _emit_tool_event(
        *,
        tool_name: str,
        input_str: str,
        result_summary: str,
        duration_ms: int,
        status: str,
        exception_type: str = "",
        exception_message: str = "",
    ) -> None:
        """Emit a tool_call event matching @mesedi.tool's wire format.

        Mesedi's tool-failures detector reads payload.tool_name and
        payload.status from the event, so this adapter's events feed
        the same detector chain as hand-written @mesedi.tool calls.
        The ``arguments`` slot mimics the @tool layout
        ({"args": [...], "kwargs": {...}}) so dashboard rendering is
        uniform, LangChain tools take a single ``input`` string, so
        we put it in ``args[0]``.
        """
        ctx = current_execution_context()
        if ctx is None:
            return

        client = get_client()
        payload: Dict[str, Any] = {
            "tool_name": tool_name,
            "arguments": {
                "args": [_truncate(input_str, _MAX_TOOL_INPUT_REPR)],
                "kwargs": {},
            },
            "status": status,
        }
        if status == "ok":
            payload["result_summary"] = _truncate(result_summary, _MAX_TOOL_OUTPUT_REPR)
        else:
            if exception_type:
                payload["exception_type"] = exception_type
            if exception_message:
                payload["exception_message"] = _truncate(exception_message, _MAX_EXC_MSG)

        # Halt-safe boundary: tool boundaries are a canonical halt
        # check point, matching @mesedi.tool's behavior.
        ctx.check_budget()
        if ctx.budget_tracker is not None:
            ctx.budget_tracker.increment_steps()

        client.submit_event(Event(
            event_id=f"evt-{uuid.uuid4().hex[:12]}",
            execution_id=ctx.execution_id,
            event_type=EventType.TOOL_CALL,
            sequence=ctx.next_sequence(),
            timestamp=utcnow_rfc3339(),
            duration_ms=duration_ms,
            payload=payload,
        ))


def _truncate(s: str, max_len: int) -> str:
    if len(s) <= max_len:
        return s
    return s[: max_len - 3] + "..."
