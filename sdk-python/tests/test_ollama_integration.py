"""Unit tests for Wave 2.5.1 — instrument_ollama field extraction +
idempotent patching.

End-to-end "patch fires event inside @wrap" coverage is integration-
test territory and ships in Wave 2.5.6. This file's job is the wiring
seams: response-shape extraction, messages-array walking, truncation,
and the idempotency guarantee that protects customers who call
instrument_ollama() twice (e.g., once in app boot, once in a test
setup) without realizing it.
"""

from __future__ import annotations

from types import SimpleNamespace
from typing import Any

import pytest

from mesedi import ollama_integration as ollama_mod
from mesedi.ollama_integration import (
    _PROVIDER,
    _content_to_text,
    _extract_first_system_message,
    _extract_last_user_message,
    _extract_messages,
    _extract_model,
    _extract_response_fields,
    _msg_role_and_content,
    _patched_classes,
    _truncate,
    instrument_ollama,
)


# ──────────────────────────────────────────────────────────────────────
# _PROVIDER must be stable across SDK versions
# ──────────────────────────────────────────────────────────────────────


def test_provider_constant_is_stable() -> None:
    """The provider string is the cluster key on the backend's
    provider_incident detector. Changing it silently misroutes every
    Ollama customer's events. This test pins the value so a typo or
    rename in a future wave fails loudly."""
    assert _PROVIDER == "ollama"


# ──────────────────────────────────────────────────────────────────────
# _extract_response_fields: Ollama → canonical token-field translation
# ──────────────────────────────────────────────────────────────────────


def test_extract_response_fields_translates_ollama_token_names() -> None:
    """Ollama uses prompt_eval_count + eval_count. The backend
    expects input_tokens + output_tokens. The translation MUST land
    on every chat call or detector telemetry is incomplete."""
    response = {
        "model": "llama3.1:8b",
        "message": {"role": "assistant", "content": "Hello."},
        "prompt_eval_count": 23,
        "eval_count": 47,
        "done": True,
    }
    text, in_tok, out_tok = _extract_response_fields(response)
    assert text == "Hello."
    assert in_tok == 23
    assert out_tok == 47


def test_extract_response_fields_handles_object_shape() -> None:
    """The typed ollama client returns objects, not dicts. The walker
    must accept either. _safe_get handles dict OR getattr lookup."""
    response = SimpleNamespace(
        model="llama3.1:8b",
        message=SimpleNamespace(role="assistant", content="Hi."),
        prompt_eval_count=10,
        eval_count=5,
    )
    text, in_tok, out_tok = _extract_response_fields(response)
    assert text == "Hi."
    assert in_tok == 10
    assert out_tok == 5


def test_extract_response_fields_degrades_gracefully_on_missing_fields() -> None:
    """A model that doesn't report token counts (rare but real for
    older Ollama versions or unusual fine-tunes) must not crash the
    wrapped call. Tokens fall through to 0."""
    response = {"message": {"content": "ok"}}
    text, in_tok, out_tok = _extract_response_fields(response)
    assert text == "ok"
    assert in_tok == 0
    assert out_tok == 0


def test_extract_response_fields_handles_non_int_token_counts() -> None:
    """If Ollama returns string-typed token counts (some proxies
    re-serialize), coerce or fall back to 0 — never raise into the
    customer's hot path."""
    response = {
        "message": {"content": "ok"},
        "prompt_eval_count": "12",
        "eval_count": "garbage",
    }
    text, in_tok, out_tok = _extract_response_fields(response)
    assert in_tok == 12
    assert out_tok == 0


# ──────────────────────────────────────────────────────────────────────
# _extract_messages: positional vs kwarg call shapes
# ──────────────────────────────────────────────────────────────────────


def test_extract_messages_prefers_kwarg_form() -> None:
    """Customers calling client.chat(model="x", messages=[...]) — the
    documented form — must have their messages extracted correctly."""
    msgs = [{"role": "user", "content": "hi"}]
    out = _extract_messages((), {"model": "x", "messages": msgs})
    assert out == msgs


def test_extract_messages_falls_back_to_positional_form() -> None:
    """Customers calling client.chat("x", [...]) (positional) — less
    common but documented — also work."""
    msgs = [{"role": "user", "content": "hi"}]
    out = _extract_messages(("x", msgs), {})
    assert out == msgs


def test_extract_messages_returns_empty_when_absent() -> None:
    """A bare client.chat() call (caller error, but must not crash
    the wrapper) returns an empty messages list."""
    assert _extract_messages((), {}) == []


# ──────────────────────────────────────────────────────────────────────
# Message-content extraction (OpenAI-shaped messages array)
# ──────────────────────────────────────────────────────────────────────


def test_extract_first_system_message_returns_first_match() -> None:
    """The FIRST system message wins (matches openai_integration
    discipline) so multi-system conversations don't surprise the
    customer with the wrong one."""
    msgs = [
        {"role": "system", "content": "you are a help bot"},
        {"role": "user", "content": "hi"},
        {"role": "system", "content": "should not be picked"},
    ]
    assert _extract_first_system_message(msgs) == "you are a help bot"


def test_extract_last_user_message_walks_backwards() -> None:
    """Multi-turn conversations should report the MOST RECENT user
    turn, not the first. Matches the other integrations."""
    msgs = [
        {"role": "user", "content": "first turn"},
        {"role": "assistant", "content": "ok"},
        {"role": "user", "content": "second turn"},
        {"role": "assistant", "content": "still ok"},
        {"role": "user", "content": "third turn"},
    ]
    assert _extract_last_user_message(msgs) == "third turn"


def test_extract_handles_object_message_shape() -> None:
    """ollama's typed Message class exposes .role / .content as
    attributes. The walker must accept either dict or object."""
    msgs = [SimpleNamespace(role="user", content="from object")]
    assert _extract_last_user_message(msgs) == "from object"


def test_msg_role_and_content_handles_both_shapes() -> None:
    role, content = _msg_role_and_content({"role": "user", "content": "x"})
    assert role == "user"
    assert content == "x"
    role, content = _msg_role_and_content(SimpleNamespace(role="assistant", content="y"))
    assert role == "assistant"
    assert content == "y"


def test_content_to_text_handles_string_list_and_none() -> None:
    assert _content_to_text("plain") == "plain"
    assert _content_to_text(None) == ""
    multimodal = [{"text": "hello"}, {"text": "world"}]
    assert _content_to_text(multimodal) == "hello\nworld"


# ──────────────────────────────────────────────────────────────────────
# Model extraction
# ──────────────────────────────────────────────────────────────────────


def test_extract_model_prefers_kwarg() -> None:
    assert _extract_model(("ignored",), {"model": "llama3.1:8b"}) == "llama3.1:8b"


def test_extract_model_falls_back_to_positional() -> None:
    assert _extract_model(("llama3.1:8b",), {}) == "llama3.1:8b"


def test_extract_model_defaults_to_unknown() -> None:
    """Defensive — never raise; the dashboard can show 'unknown' but
    must not crash the wrapped call."""
    assert _extract_model((), {}) == "unknown"


# ──────────────────────────────────────────────────────────────────────
# _truncate: bounded payload fields
# ──────────────────────────────────────────────────────────────────────


def test_truncate_returns_short_strings_unchanged() -> None:
    assert _truncate("hi", 100) == "hi"
    assert _truncate("", 100) == ""


def test_truncate_appends_marker_on_overflow() -> None:
    s = "x" * 1500
    out = _truncate(s, 1000)
    assert out.startswith("x" * 1000)
    assert out.endswith("...[truncated]")


# ──────────────────────────────────────────────────────────────────────
# instrument_ollama: dependency-injection + idempotent patching
# ──────────────────────────────────────────────────────────────────────


class _FakeSyncClient:
    """Stand-in for ollama.Client. instrument_ollama patches .chat;
    we don't actually call it here — these tests are about the patch
    machinery, not the runtime path."""

    def chat(self, *args: Any, **kwargs: Any) -> Any:  # pragma: no cover
        return None


class _FakeAsyncClient:
    """Stand-in for ollama.AsyncClient."""

    async def chat(self, *args: Any, **kwargs: Any) -> Any:  # pragma: no cover
        return None


@pytest.fixture(autouse=True)
def _reset_patch_registry() -> None:
    """Each test gets a clean registry so idempotency checks don't
    bleed across tests."""
    saved = set(_patched_classes)
    _patched_classes.clear()
    yield
    _patched_classes.clear()
    _patched_classes.update(saved)


def test_instrument_ollama_with_injected_classes_returns_true() -> None:
    """When the caller passes fake classes (dependency injection for
    testability), instrument_ollama returns True and the .chat method
    has been replaced."""
    original_sync_chat = _FakeSyncClient.chat
    result = instrument_ollama(
        chat_class=_FakeSyncClient,
        async_chat_class=_FakeAsyncClient,
    )
    assert result is True
    assert _FakeSyncClient.chat is not original_sync_chat


def test_instrument_ollama_is_idempotent_per_class() -> None:
    """Double-calling instrument_ollama on the same class must not
    re-patch (which would create a double-wrap stack). The second
    call is a no-op."""
    instrument_ollama(chat_class=_FakeSyncClient)
    first_patched = _FakeSyncClient.chat
    instrument_ollama(chat_class=_FakeSyncClient)
    assert _FakeSyncClient.chat is first_patched


def test_instrument_ollama_returns_false_when_nothing_patched() -> None:
    """When neither class is provided AND the ollama package can't
    be imported, instrument_ollama returns False so the caller can
    log a warning. Mock the import failure by passing classes as
    None and relying on the import being unavailable in the test env."""
    # If ollama IS installed in the test env, this assertion would be
    # invalid. Detect that and skip cleanly.
    try:
        import ollama  # noqa: F401
        pytest.skip("ollama package is installed in test env; "
                    "this test verifies the no-import path only.")
    except ImportError:
        pass
    result = instrument_ollama()
    assert result is False
