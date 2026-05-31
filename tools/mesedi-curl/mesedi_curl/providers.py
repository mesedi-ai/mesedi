# Provider detection and request/response parsing.
#
# Each supported provider maps a URL host to a parser that knows how to
# extract:
#   - the model name from the request body
#   - the input + output token counts from the response (or from the
#     terminal SSE event for streaming responses)
#   - the finish_reason from the response
#
# Unknown hosts fall through to GENERIC_PROVIDER, which records the call
# without provider-specific token math. The dashboard still sees the
# request and response; token counts stay zero.
#
# Provider detection is intentionally permissive: we match on hostname
# substring rather than exact equality, so api.openai.com and
# my-org.openai.azure.com both resolve to "openai". The cost of a false
# positive is benign (a slightly mis-parsed event); the cost of being
# too strict is silent gaps in coverage.
from __future__ import annotations

import json
from dataclasses import dataclass
from typing import Any, Optional
from urllib.parse import urlparse


@dataclass
class ProviderParseResult:
    """Output of parsing one request + response pair through a provider.

    All fields are best-effort. If a field cannot be extracted (for
    example, the response was not valid JSON, or the request body was
    not parsable) it stays at its zero value. The shim's contract with
    the backend is that missing fields are OK; the dashboard surfaces
    the call regardless.
    """

    provider: str = "generic"
    model: str = ""
    input_tokens: int = 0
    output_tokens: int = 0
    finish_reason: str = ""
    user_prompt_preview: str = ""
    response_preview: str = ""
    is_streaming: bool = False


# Maximum number of characters from the prompt + response we record on
# the event payload. Long prompts can run into millions of tokens and
# we do not want to bloat the event database. Backend can later choose
# to redact further.
PREVIEW_CHARS = 4000


def detect_provider(url: str) -> str:
    """Map a URL to one of: 'anthropic', 'openai', 'gemini', 'generic'."""
    host = (urlparse(url).hostname or "").lower()
    if "anthropic" in host:
        return "anthropic"
    if "openai" in host:
        return "openai"
    # Google's Gemini API hosts:
    #   generativelanguage.googleapis.com (PaLM/Gemini public API)
    #   us-central1-aiplatform.googleapis.com (Vertex AI)
    if "generativelanguage.googleapis.com" in host or (
        "aiplatform.googleapis.com" in host
    ):
        return "gemini"
    return "generic"


def parse_request_body(provider: str, body: Optional[bytes]) -> dict[str, Any]:
    """Extract structured info from the request body.

    Returns a dict that always contains:
        model: str
        user_prompt_preview: str
        stream: bool (best-effort; True if the body asked for streaming)
    """
    out: dict[str, Any] = {"model": "", "user_prompt_preview": "", "stream": False}
    if not body:
        return out
    try:
        data = json.loads(body)
    except (json.JSONDecodeError, UnicodeDecodeError, TypeError):
        # Body is not JSON. Could be form data, multipart, etc. Common
        # enough that we just bail silently. The wrapper still records
        # an opaque event with the URL and timing.
        return out
    if not isinstance(data, dict):
        return out

    out["model"] = str(data.get("model", ""))
    out["stream"] = bool(data.get("stream", False))

    # Provider-specific extraction of the prompt-ish text. The
    # dashboard uses this for the loop / drift detectors and for
    # human-readable previews on the execution detail page.
    if provider == "anthropic":
        # Anthropic Messages API: messages: [{role, content}, ...]
        # content can be a string OR a list of typed blocks. The last
        # user-role message is the "user_prompt" for our purposes.
        for msg in reversed(data.get("messages", []) or []):
            if isinstance(msg, dict) and msg.get("role") == "user":
                out["user_prompt_preview"] = _flatten_content(msg.get("content", ""))[
                    :PREVIEW_CHARS
                ]
                break
    elif provider == "openai":
        # OpenAI chat completions: messages: [{role, content}, ...]
        for msg in reversed(data.get("messages", []) or []):
            if isinstance(msg, dict) and msg.get("role") == "user":
                out["user_prompt_preview"] = _flatten_content(msg.get("content", ""))[
                    :PREVIEW_CHARS
                ]
                break
        # OpenAI also has legacy completions: prompt: "..."
        if not out["user_prompt_preview"] and "prompt" in data:
            out["user_prompt_preview"] = str(data["prompt"])[:PREVIEW_CHARS]
    elif provider == "gemini":
        # Gemini generateContent: contents: [{role, parts: [{text}, ...]}, ...]
        for content in reversed(data.get("contents", []) or []):
            if isinstance(content, dict) and content.get("role") in ("user", None):
                parts = content.get("parts", []) or []
                text_parts = [
                    p.get("text", "") for p in parts if isinstance(p, dict)
                ]
                out["user_prompt_preview"] = " ".join(text_parts)[:PREVIEW_CHARS]
                if out["user_prompt_preview"]:
                    break

    return out


def parse_response_body(
    provider: str, body: bytes, is_streaming: bool
) -> dict[str, Any]:
    """Extract structured info from the response body.

    For non-streaming responses, this is a single JSON parse. For
    streaming responses, the body is the concatenation of all SSE
    chunks captured by the streaming module, and we walk it
    line-by-line looking for the terminal usage-stats event.

    Returns a dict that always contains:
        input_tokens: int
        output_tokens: int
        finish_reason: str
        response_preview: str
    """
    out: dict[str, Any] = {
        "input_tokens": 0,
        "output_tokens": 0,
        "finish_reason": "",
        "response_preview": "",
    }
    if not body:
        return out

    if is_streaming:
        return _parse_streaming_response(provider, body, out)

    try:
        data = json.loads(body)
    except (json.JSONDecodeError, UnicodeDecodeError):
        # Non-JSON response (HTML error page, plain-text 429, etc.).
        # Record a preview of the raw body so the user can see what
        # happened in the dashboard.
        out["response_preview"] = _safe_decode(body)[:PREVIEW_CHARS]
        return out
    if not isinstance(data, dict):
        return out

    if provider == "anthropic":
        usage = data.get("usage", {}) or {}
        out["input_tokens"] = int(usage.get("input_tokens", 0) or 0)
        out["output_tokens"] = int(usage.get("output_tokens", 0) or 0)
        out["finish_reason"] = str(data.get("stop_reason", "") or "")
        # content: [{type: "text", text: "..."}, ...]
        out["response_preview"] = _flatten_content(data.get("content", ""))[
            :PREVIEW_CHARS
        ]
    elif provider == "openai":
        usage = data.get("usage", {}) or {}
        out["input_tokens"] = int(usage.get("prompt_tokens", 0) or 0)
        out["output_tokens"] = int(usage.get("completion_tokens", 0) or 0)
        choices = data.get("choices", []) or []
        if choices and isinstance(choices[0], dict):
            choice = choices[0]
            out["finish_reason"] = str(choice.get("finish_reason", "") or "")
            msg = choice.get("message", {}) or {}
            out["response_preview"] = _flatten_content(msg.get("content", ""))[
                :PREVIEW_CHARS
            ]
    elif provider == "gemini":
        # usageMetadata: {promptTokenCount, candidatesTokenCount, ...}
        usage = data.get("usageMetadata", {}) or {}
        out["input_tokens"] = int(usage.get("promptTokenCount", 0) or 0)
        out["output_tokens"] = int(usage.get("candidatesTokenCount", 0) or 0)
        candidates = data.get("candidates", []) or []
        if candidates and isinstance(candidates[0], dict):
            cand = candidates[0]
            out["finish_reason"] = str(cand.get("finishReason", "") or "")
            content = cand.get("content", {}) or {}
            parts = content.get("parts", []) or []
            text_parts = [p.get("text", "") for p in parts if isinstance(p, dict)]
            out["response_preview"] = " ".join(text_parts)[:PREVIEW_CHARS]
    else:
        # Generic provider: just capture a preview.
        out["response_preview"] = _safe_decode(body)[:PREVIEW_CHARS]

    return out


def _parse_streaming_response(
    provider: str, body: bytes, out: dict[str, Any]
) -> dict[str, Any]:
    """Walk SSE chunks looking for terminal token-usage events.

    Each provider emits usage stats differently:
      Anthropic:  event: message_delta, with {usage: {output_tokens: N}}
                  event: message_start has input_tokens.
      OpenAI:     usage is in the final chunk if requested via
                  stream_options.include_usage=true. Otherwise zero.
      Gemini:     usageMetadata appears in the terminal chunk.

    For the response preview, we concatenate text deltas as we walk.
    """
    text_chunks: list[str] = []
    for line in body.splitlines():
        if not line.startswith(b"data:"):
            continue
        payload = line[5:].strip()
        if not payload or payload == b"[DONE]":
            continue
        try:
            evt = json.loads(payload)
        except (json.JSONDecodeError, UnicodeDecodeError):
            continue
        if not isinstance(evt, dict):
            continue

        if provider == "anthropic":
            # message_start has usage.input_tokens
            if evt.get("type") == "message_start":
                usage = (evt.get("message", {}) or {}).get("usage", {}) or {}
                out["input_tokens"] = int(usage.get("input_tokens", 0) or 0)
            # message_delta accumulates output_tokens
            elif evt.get("type") == "message_delta":
                usage = evt.get("usage", {}) or {}
                if usage.get("output_tokens"):
                    out["output_tokens"] = int(usage["output_tokens"])
                delta = evt.get("delta", {}) or {}
                if delta.get("stop_reason"):
                    out["finish_reason"] = str(delta["stop_reason"])
            # content_block_delta carries text
            elif evt.get("type") == "content_block_delta":
                delta = evt.get("delta", {}) or {}
                if delta.get("type") == "text_delta":
                    text_chunks.append(str(delta.get("text", "")))
        elif provider == "openai":
            # OpenAI streaming chunk: {choices: [{delta: {content}, finish_reason}], usage}
            usage = evt.get("usage")
            if isinstance(usage, dict):
                out["input_tokens"] = int(usage.get("prompt_tokens", 0) or 0)
                out["output_tokens"] = int(usage.get("completion_tokens", 0) or 0)
            for choice in evt.get("choices", []) or []:
                if not isinstance(choice, dict):
                    continue
                if choice.get("finish_reason"):
                    out["finish_reason"] = str(choice["finish_reason"])
                delta = choice.get("delta", {}) or {}
                if delta.get("content"):
                    text_chunks.append(str(delta["content"]))
        elif provider == "gemini":
            # Gemini streamGenerateContent: each event is a full
            # GenerateContentResponse with usageMetadata on the last.
            usage = evt.get("usageMetadata")
            if isinstance(usage, dict):
                out["input_tokens"] = int(usage.get("promptTokenCount", 0) or 0)
                out["output_tokens"] = int(usage.get("candidatesTokenCount", 0) or 0)
            for cand in evt.get("candidates", []) or []:
                if not isinstance(cand, dict):
                    continue
                if cand.get("finishReason"):
                    out["finish_reason"] = str(cand["finishReason"])
                content = cand.get("content", {}) or {}
                for part in content.get("parts", []) or []:
                    if isinstance(part, dict) and part.get("text"):
                        text_chunks.append(str(part["text"]))

    if text_chunks:
        joined = "".join(text_chunks)
        out["response_preview"] = joined[:PREVIEW_CHARS]
    return out


def _flatten_content(content: Any) -> str:
    """Reduce an Anthropic-style content list (or plain string) to text.

    Anthropic and OpenAI both accept either a plain string or a list of
    typed content blocks. We normalize both shapes to a single string.
    """
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts: list[str] = []
        for block in content:
            if isinstance(block, str):
                parts.append(block)
            elif isinstance(block, dict):
                # {type: "text", text: "..."} is the common shape.
                if block.get("type") == "text":
                    parts.append(str(block.get("text", "")))
                elif "text" in block:
                    parts.append(str(block["text"]))
        return "\n".join(parts)
    return str(content) if content is not None else ""


def _safe_decode(body: bytes) -> str:
    """Decode bytes to str, never raising on bad encodings."""
    try:
        return body.decode("utf-8", errors="replace")
    except Exception:
        return ""
