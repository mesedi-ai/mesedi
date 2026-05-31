# Unit tests for the provider detection + parsing module.
#
# These tests use only the bytes-in/dict-out parts of providers.py, so
# they run with zero network I/O. Real provider response fixtures are
# inlined to keep the test self-contained.

import json

import pytest

from mesedi_curl import providers


# ---------- Provider detection ----------


@pytest.mark.parametrize(
    "url, expected",
    [
        ("https://api.anthropic.com/v1/messages", "anthropic"),
        ("https://API.ANTHROPIC.COM/v1/messages", "anthropic"),
        ("https://api.openai.com/v1/chat/completions", "openai"),
        ("https://my-org.openai.azure.com/openai/deployments/x/chat", "openai"),
        ("https://generativelanguage.googleapis.com/v1/models/gemini:generateContent", "gemini"),
        ("https://us-central1-aiplatform.googleapis.com/v1/projects/x/locations/y", "gemini"),
        ("https://example.com/api/something", "generic"),
        ("", "generic"),
    ],
)
def test_detect_provider(url, expected):
    assert providers.detect_provider(url) == expected


# ---------- Request body parsing ----------


def test_parse_anthropic_request():
    body = json.dumps(
        {
            "model": "claude-opus-4-6",
            "max_tokens": 1024,
            "messages": [
                {"role": "user", "content": "Hello, Claude"},
            ],
            "stream": True,
        }
    ).encode()
    out = providers.parse_request_body("anthropic", body)
    assert out["model"] == "claude-opus-4-6"
    assert out["user_prompt_preview"] == "Hello, Claude"
    assert out["stream"] is True


def test_parse_openai_request():
    body = json.dumps(
        {
            "model": "gpt-4o",
            "messages": [
                {"role": "system", "content": "You are helpful."},
                {"role": "user", "content": "What is 2+2?"},
            ],
        }
    ).encode()
    out = providers.parse_request_body("openai", body)
    assert out["model"] == "gpt-4o"
    assert out["user_prompt_preview"] == "What is 2+2?"
    assert out["stream"] is False


def test_parse_gemini_request():
    body = json.dumps(
        {
            "contents": [
                {"role": "user", "parts": [{"text": "Tell me a joke."}]},
            ]
        }
    ).encode()
    out = providers.parse_request_body("gemini", body)
    assert out["user_prompt_preview"] == "Tell me a joke."


def test_parse_request_handles_non_json():
    out = providers.parse_request_body("anthropic", b"not json at all")
    assert out["model"] == ""
    assert out["user_prompt_preview"] == ""


def test_parse_request_handles_anthropic_content_blocks():
    """Anthropic accepts content as a list of typed blocks."""
    body = json.dumps(
        {
            "model": "claude-opus-4-6",
            "messages": [
                {
                    "role": "user",
                    "content": [
                        {"type": "text", "text": "Block one."},
                        {"type": "text", "text": "Block two."},
                    ],
                },
            ],
        }
    ).encode()
    out = providers.parse_request_body("anthropic", body)
    assert "Block one." in out["user_prompt_preview"]
    assert "Block two." in out["user_prompt_preview"]


# ---------- Response body parsing (non-streaming) ----------


def test_parse_anthropic_response():
    body = json.dumps(
        {
            "model": "claude-opus-4-6",
            "stop_reason": "end_turn",
            "usage": {"input_tokens": 12, "output_tokens": 47},
            "content": [{"type": "text", "text": "Hello, human."}],
        }
    ).encode()
    out = providers.parse_response_body("anthropic", body, is_streaming=False)
    assert out["input_tokens"] == 12
    assert out["output_tokens"] == 47
    assert out["finish_reason"] == "end_turn"
    assert out["response_preview"] == "Hello, human."


def test_parse_openai_response():
    body = json.dumps(
        {
            "model": "gpt-4o",
            "choices": [
                {
                    "finish_reason": "stop",
                    "message": {"role": "assistant", "content": "4"},
                }
            ],
            "usage": {"prompt_tokens": 20, "completion_tokens": 1, "total_tokens": 21},
        }
    ).encode()
    out = providers.parse_response_body("openai", body, is_streaming=False)
    assert out["input_tokens"] == 20
    assert out["output_tokens"] == 1
    assert out["finish_reason"] == "stop"
    assert out["response_preview"] == "4"


def test_parse_gemini_response():
    body = json.dumps(
        {
            "candidates": [
                {
                    "finishReason": "STOP",
                    "content": {
                        "parts": [{"text": "Sure, here is a joke."}],
                        "role": "model",
                    },
                }
            ],
            "usageMetadata": {
                "promptTokenCount": 8,
                "candidatesTokenCount": 6,
            },
        }
    ).encode()
    out = providers.parse_response_body("gemini", body, is_streaming=False)
    assert out["input_tokens"] == 8
    assert out["output_tokens"] == 6
    assert out["finish_reason"] == "STOP"
    assert "joke" in out["response_preview"]


def test_parse_response_handles_non_json():
    out = providers.parse_response_body("openai", b"<html>5xx error</html>", is_streaming=False)
    assert out["input_tokens"] == 0
    assert out["output_tokens"] == 0
    assert "<html>" in out["response_preview"]


# ---------- Response body parsing (streaming) ----------


def test_parse_anthropic_streaming_response():
    """Anthropic SSE format: message_start has input tokens; message_delta
    accumulates output tokens; content_block_delta carries text."""
    chunks = [
        b'event: message_start\ndata: {"type":"message_start","message":{"usage":{"input_tokens":15,"output_tokens":1}}}\n\n',
        b'event: content_block_delta\ndata: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}\n\n',
        b'event: content_block_delta\ndata: {"type":"content_block_delta","delta":{"type":"text_delta","text":", "}}\n\n',
        b'event: content_block_delta\ndata: {"type":"content_block_delta","delta":{"type":"text_delta","text":"world"}}\n\n',
        b'event: message_delta\ndata: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}\n\n',
    ]
    body = b"".join(chunks)
    out = providers.parse_response_body("anthropic", body, is_streaming=True)
    assert out["input_tokens"] == 15
    assert out["output_tokens"] == 42
    assert out["finish_reason"] == "end_turn"
    assert out["response_preview"] == "Hello, world"


def test_parse_openai_streaming_response():
    """OpenAI SSE: each event is a delta, final has finish_reason; usage
    only appears with stream_options.include_usage=true."""
    chunks = [
        b'data: {"choices":[{"delta":{"content":"4"},"finish_reason":null}]}\n\n',
        b'data: {"choices":[{"delta":{"content":"2"},"finish_reason":null}]}\n\n',
        b'data: {"choices":[{"delta":{},"finish_reason":"stop"}]}\n\n',
        b'data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":2}}\n\n',
        b"data: [DONE]\n\n",
    ]
    body = b"".join(chunks)
    out = providers.parse_response_body("openai", body, is_streaming=True)
    assert out["input_tokens"] == 11
    assert out["output_tokens"] == 2
    assert out["finish_reason"] == "stop"
    assert out["response_preview"] == "42"


def test_parse_gemini_streaming_response():
    """Gemini streamGenerateContent: each event is a full response shape,
    usageMetadata appears on the last chunk."""
    chunks = [
        b'data: {"candidates":[{"content":{"parts":[{"text":"Once"}]}}]}\n\n',
        b'data: {"candidates":[{"content":{"parts":[{"text":" upon"}]}}]}\n\n',
        b'data: {"candidates":[{"content":{"parts":[{"text":" a time."}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":4}}\n\n',
    ]
    body = b"".join(chunks)
    out = providers.parse_response_body("gemini", body, is_streaming=True)
    assert out["input_tokens"] == 3
    assert out["output_tokens"] == 4
    assert out["finish_reason"] == "STOP"
    assert out["response_preview"] == "Once upon a time."
