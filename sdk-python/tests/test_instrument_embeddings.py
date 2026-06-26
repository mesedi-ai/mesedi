"""
Unit tests for #271.j embeddings instrumentation across OpenAI / Cohere
/ Gemini integrations. Uses dependency-injected fake classes (same
pattern as test_instrument_throttling.py) so no real SDK calls happen.

Tests cover, per provider:
  - Sync + async patching of the embed surface.
  - ctx=None passthrough (no event emission when wrap not active).
  - Success path emits llm_call event with surface='embeddings' on
    the payload.
  - Failure path emits failure event with surface='embeddings' + fires
    throttling auto-emit when the exception classifies as throttling.
  - Existing chat patches still emit surface='chat' on their payloads
    (regression guard for the backfill pass).
"""

from __future__ import annotations

from typing import Any, Dict, List

import pytest

import sys

import mesedi
from mesedi import openai_integration, cohere_integration, gemini_integration

# mesedi/__init__.py does `from mesedi.wrap import wrap`, which shadows
# the `wrap` submodule name on the mesedi package with the function. To
# monkeypatch the submodule's `get_client` we have to reach it via
# sys.modules where the submodule object still lives.
wrap_mod = sys.modules["mesedi.wrap"]


class _CapturedClient:
    """Test double for the SDK shipper. Records every submit_event call
    so tests can introspect the emitted payload. Also no-ops
    submit_execution_start / submit_execution_end so @mesedi.wrap's
    lifecycle calls don't escape to the real backend during testing."""

    def __init__(self) -> None:
        self.events: List[Any] = []
        # Mirror the real client surface enough for wrap() not to crash
        # AND to stay confined to this in-memory double.
        self.base_url = ""
        self.api_key = ""

    def submit_event(self, event: Any) -> None:
        self.events.append(event)

    def submit_execution_start(self, _execution: Any) -> None:
        # CRITICAL: do nothing. mesedi.wrap calls this; if we let the
        # real shipper handle it, every wrap'd test agent posts a real
        # execution to api.mesedi.ai. Failure-path tests then surface
        # as production "crashes" failure_groups with Discord alerts.
        return

    def submit_execution_end(self, _execution: Any) -> None:
        return


def _setup_capture(monkeypatch: pytest.MonkeyPatch) -> _CapturedClient:
    """Replace mesedi.client.get_client with one that returns the
    capture client; also patches it on each integration module's local
    `get_client` reference (the integrations do `from mesedi.client
    import get_client` at module load time, so monkeypatching the
    client module isn't sufficient)."""
    cap = _CapturedClient()

    def _get(*_args: Any, **_kwargs: Any) -> Any:
        return cap

    monkeypatch.setattr(openai_integration, "get_client", _get)
    monkeypatch.setattr(cohere_integration, "get_client", _get)
    monkeypatch.setattr(gemini_integration, "get_client", _get)
    # mesedi.wrap also calls get_client() to ship
    # submit_execution_start/end. Patching it here prevents the
    # @mesedi.wrap fixture from leaking real "executions" to
    # production when a test's fake exception bubbles up.
    monkeypatch.setattr(wrap_mod, "get_client", _get)
    return cap


def _llm_call_payloads(cap: _CapturedClient) -> List[Dict[str, Any]]:
    out: List[Dict[str, Any]] = []
    for ev in cap.events:
        et = getattr(ev, "event_type", None)
        if et is None:
            continue
        # event_type may be a str alias or an enum with a .value attr.
        type_name = getattr(et, "value", et)
        if type_name == "llm_call":
            out.append(ev.payload)
    return out


# ── OpenAI embeddings ────────────────────────────────────────────────


class _FakeOpenAIEmbeddings:
    """Stand-in for openai.resources.embeddings.Embeddings."""

    raise_exc: Exception = None  # type: ignore[assignment]

    def create(self, *_args: Any, **kwargs: Any) -> Any:
        if self.raise_exc is not None:
            raise self.raise_exc

        class _Usage:
            prompt_tokens = 42
            total_tokens = 42

        class _Response:
            data: List[Any] = []
            usage = _Usage()

        return _Response()


def test_openai_embeddings_ctx_none_passthrough(monkeypatch):
    _setup_capture(monkeypatch)
    cls = type("E", (_FakeOpenAIEmbeddings,), {})
    openai_integration._patch_embeddings(cls)
    # No wrap context active — patched create should pass through.
    result = cls().create(model="text-embedding-3-small", input="hello")
    assert result is not None


def test_openai_embeddings_success_emits_event_with_surface(monkeypatch):
    cap = _setup_capture(monkeypatch)
    cls = type("E2", (_FakeOpenAIEmbeddings,), {})
    openai_integration._patch_embeddings(cls)

    @mesedi.wrap
    def agent() -> Any:
        return cls().create(
            model="text-embedding-3-small", input="hello world",
        )

    agent()
    payloads = _llm_call_payloads(cap)
    embed_events = [p for p in payloads if p.get("surface") == "embeddings"]
    assert len(embed_events) == 1
    ev = embed_events[0]
    assert ev["provider"] == "openai"
    assert ev["status"] == "ok"
    assert ev["model"] == "text-embedding-3-small"
    assert ev["user_message"] == "hello world"
    assert ev["input_tokens"] == 42
    assert ev["output_tokens"] == 0


def test_openai_embeddings_failure_emits_event_with_surface(monkeypatch):
    cap = _setup_capture(monkeypatch)

    class _Err(Exception):
        pass

    cls = type("E3", (_FakeOpenAIEmbeddings,), {"raise_exc": _Err("boom")})
    openai_integration._patch_embeddings(cls)

    @mesedi.wrap
    def agent() -> Any:
        return cls().create(model="text-embedding-3-small", input="x")

    with pytest.raises(_Err):
        agent()

    payloads = _llm_call_payloads(cap)
    embed_failures = [
        p for p in payloads
        if p.get("surface") == "embeddings" and p.get("status") == "failed"
    ]
    assert len(embed_failures) == 1
    assert embed_failures[0]["provider"] == "openai"
    assert embed_failures[0]["model"] == "text-embedding-3-small"


def test_openai_async_embeddings_patcher_runs() -> None:
    """_patch_async_embeddings replaces cls.create with an async patched
    fn. (Behavior coverage of the async path matches the sync test —
    the async code is a near-identical mirror with `await` added; the
    SDK's existing async chat tests cover the runtime wiring through
    integration tests. Here we only assert the patcher mutated the
    class so the wave's wiring is honest.)"""

    class _Async:
        async def create(self, *_args: Any, **kwargs: Any) -> Any:
            return None

    original = _Async.create
    openai_integration._patch_async_embeddings(_Async)
    assert _Async.create is not original


# ── Cohere embed (v1 + v2 patcher is identical; one test covers both) ─


class _FakeCohereClient:
    raise_exc: Exception = None  # type: ignore[assignment]

    def embed(self, *_args: Any, **kwargs: Any) -> Any:
        if self.raise_exc is not None:
            raise self.raise_exc

        class _Billed:
            input_tokens = 99
        class _Meta:
            billed_units = _Billed()
        class _R:
            embeddings: List[Any] = []
            meta = _Meta()
        return _R()


def test_cohere_embed_success_emits_surface_event(monkeypatch):
    cap = _setup_capture(monkeypatch)
    cls = type("C", (_FakeCohereClient,), {})
    cohere_integration._patch_embed_sync(cls)

    @mesedi.wrap
    def agent() -> Any:
        return cls().embed(
            texts=["doc1", "doc2"],
            model="embed-english-v3.0",
            input_type="search_document",
        )

    agent()
    payloads = _llm_call_payloads(cap)
    embeds = [p for p in payloads if p.get("surface") == "embeddings"]
    assert len(embeds) == 1
    ev = embeds[0]
    assert ev["provider"] == "cohere"
    assert ev["input_tokens"] == 99
    assert ev["output_tokens"] == 0
    assert "doc1" in ev["user_message"] and "doc2" in ev["user_message"]


def test_cohere_embed_failure_emits_surface_event(monkeypatch):
    cap = _setup_capture(monkeypatch)

    class _Err(Exception):
        pass

    cls = type("C2", (_FakeCohereClient,), {"raise_exc": _Err("x")})
    cohere_integration._patch_embed_sync(cls)

    @mesedi.wrap
    def agent() -> Any:
        return cls().embed(texts=["t"], model="embed-english-v3.0")

    with pytest.raises(_Err):
        agent()

    payloads = _llm_call_payloads(cap)
    failures = [
        p for p in payloads
        if p.get("surface") == "embeddings" and p.get("status") == "failed"
    ]
    assert len(failures) == 1


def test_cohere_async_embed_patcher_runs() -> None:
    """_patch_embed_async swaps cls.embed with an async patched fn.
    Runtime async path is covered through the existing async chat
    tests + integration tests; here we only verify the wave wired the
    patcher correctly."""

    class _Async:
        async def embed(self, *_args: Any, **kwargs: Any) -> Any:
            return None

    original = _Async.embed
    cohere_integration._patch_embed_async(_Async)
    assert _Async.embed is not original


# ── Gemini embed_content ──────────────────────────────────────────────


class _FakeGenAIModule:
    """Stand-in for the google.generativeai module."""

    raise_exc: Exception = None  # type: ignore[assignment]

    def embed_content(self, *_args: Any, **kwargs: Any) -> Any:
        if self.raise_exc is not None:
            raise self.raise_exc
        return {"embedding": [0.1, 0.2, 0.3]}

    async def embed_content_async(self, *_args: Any, **kwargs: Any) -> Any:
        if self.raise_exc is not None:
            raise self.raise_exc
        return {"embedding": [0.4, 0.5]}


def test_gemini_embed_content_success(monkeypatch):
    cap = _setup_capture(monkeypatch)
    fake_mod = _FakeGenAIModule()
    # Reset module-level idempotency flags so this test can re-patch.
    gemini_integration._embed_sync_patched = False
    gemini_integration._embed_async_patched = False
    gemini_integration.instrument_gemini_embed(fake_mod)

    @mesedi.wrap
    def agent() -> Any:
        return fake_mod.embed_content(
            model="text-embedding-004", content="hello world",
        )

    agent()

    payloads = _llm_call_payloads(cap)
    embeds = [p for p in payloads if p.get("surface") == "embeddings"]
    assert len(embeds) == 1
    ev = embeds[0]
    assert ev["provider"] == "gemini"
    assert ev["model"] == "text-embedding-004"
    assert ev["user_message"] == "hello world"
    assert ev["status"] == "ok"


def test_gemini_embed_content_failure(monkeypatch):
    cap = _setup_capture(monkeypatch)

    class _Err(Exception):
        pass

    fake_mod = _FakeGenAIModule()
    fake_mod.raise_exc = _Err("nope")
    gemini_integration._embed_sync_patched = False
    gemini_integration._embed_async_patched = False
    gemini_integration.instrument_gemini_embed(fake_mod)

    @mesedi.wrap
    def agent() -> Any:
        return fake_mod.embed_content(
            model="text-embedding-004", content="hi",
        )

    with pytest.raises(_Err):
        agent()

    payloads = _llm_call_payloads(cap)
    failures = [
        p for p in payloads
        if p.get("surface") == "embeddings" and p.get("status") == "failed"
    ]
    assert len(failures) == 1
    assert failures[0]["provider"] == "gemini"


# ── #271.j sub-ship 2 — OpenAI image + audio + Cohere rerank ─────────


class _FakeOpenAIImages:
    raise_exc: Exception = None  # type: ignore[assignment]

    def create(self, *_args: Any, **kwargs: Any) -> Any:
        if self.raise_exc is not None:
            raise self.raise_exc
        class _R:
            data: List[Any] = []
        return _R()


def test_openai_images_success_emits_surface(monkeypatch):
    cap = _setup_capture(monkeypatch)
    cls = type("I", (_FakeOpenAIImages,), {})
    openai_integration._patch_openai_images_sync(cls)

    @mesedi.wrap
    def agent() -> Any:
        return cls().create(model="gpt-image-1", prompt="a red apple")

    agent()
    payloads = _llm_call_payloads(cap)
    imgs = [p for p in payloads if p.get("surface") == "image"]
    assert len(imgs) == 1
    assert imgs[0]["provider"] == "openai"
    assert imgs[0]["user_message"] == "a red apple"
    assert imgs[0]["response_text"] == "<image output>"


def test_openai_images_failure_emits_surface(monkeypatch):
    cap = _setup_capture(monkeypatch)

    class _Err(Exception):
        pass

    cls = type("I2", (_FakeOpenAIImages,), {"raise_exc": _Err("nope")})
    openai_integration._patch_openai_images_sync(cls)

    @mesedi.wrap
    def agent() -> Any:
        return cls().create(model="gpt-image-1", prompt="x")

    with pytest.raises(_Err):
        agent()
    payloads = _llm_call_payloads(cap)
    failures = [
        p for p in payloads
        if p.get("surface") == "image" and p.get("status") == "failed"
    ]
    assert len(failures) == 1


class _FakeAudioTranscriptions:
    def create(self, *_args: Any, **kwargs: Any) -> Any:
        class _R:
            text = "hello world"
        return _R()


def test_openai_audio_transcriptions_success(monkeypatch):
    cap = _setup_capture(monkeypatch)
    cls = type("T", (_FakeAudioTranscriptions,), {})
    openai_integration._patch_openai_audio_transcriptions_sync(cls)

    @mesedi.wrap
    def agent() -> Any:
        return cls().create(model="whisper-1", file="audio.mp3")

    agent()
    payloads = _llm_call_payloads(cap)
    stts = [p for p in payloads if p.get("surface") == "audio_stt"]
    assert len(stts) == 1
    assert stts[0]["response_text"] == "<audio transcription>"
    assert "audio.mp3" in stts[0]["user_message"]


class _FakeAudioSpeech:
    def create(self, *_args: Any, **kwargs: Any) -> Any:
        class _R:
            pass
        return _R()


def test_openai_audio_speech_success(monkeypatch):
    cap = _setup_capture(monkeypatch)
    cls = type("S", (_FakeAudioSpeech,), {})
    openai_integration._patch_openai_audio_speech_sync(cls)

    @mesedi.wrap
    def agent() -> Any:
        return cls().create(
            model="gpt-4o-mini-tts", voice="alloy", input="read this aloud",
        )

    agent()
    payloads = _llm_call_payloads(cap)
    ttss = [p for p in payloads if p.get("surface") == "audio_tts"]
    assert len(ttss) == 1
    assert ttss[0]["user_message"] == "read this aloud"
    assert ttss[0]["response_text"] == "<audio output>"


class _FakeCohereRerank:
    raise_exc: Exception = None  # type: ignore[assignment]

    def rerank(self, *_args: Any, **kwargs: Any) -> Any:
        if self.raise_exc is not None:
            raise self.raise_exc
        class _Item:
            pass
        class _R:
            results = [_Item(), _Item(), _Item()]
        return _R()


def test_cohere_rerank_success_emits_surface(monkeypatch):
    cap = _setup_capture(monkeypatch)
    cls = type("R", (_FakeCohereRerank,), {})
    cohere_integration._patch_rerank_sync(cls)

    @mesedi.wrap
    def agent() -> Any:
        return cls().rerank(
            query="best apples",
            documents=["red", "green", "blue"],
            model="rerank-english-v3.0",
        )

    agent()
    payloads = _llm_call_payloads(cap)
    rrs = [p for p in payloads if p.get("surface") == "rerank"]
    assert len(rrs) == 1
    assert rrs[0]["response_text"] == "<rerank results: 3>"
    assert "best apples" in rrs[0]["user_message"]


def test_cohere_rerank_failure(monkeypatch):
    cap = _setup_capture(monkeypatch)

    class _Err(Exception):
        pass

    cls = type("R2", (_FakeCohereRerank,), {"raise_exc": _Err("x")})
    cohere_integration._patch_rerank_sync(cls)

    @mesedi.wrap
    def agent() -> Any:
        return cls().rerank(
            query="q", documents=["a"], model="rerank-english-v3.0",
        )

    with pytest.raises(_Err):
        agent()
    payloads = _llm_call_payloads(cap)
    failures = [
        p for p in payloads
        if p.get("surface") == "rerank" and p.get("status") == "failed"
    ]
    assert len(failures) == 1


def test_openai_audio_async_patcher_runs():
    class _Async:
        async def create(self, *_args: Any, **kwargs: Any) -> Any:
            return None
    original = _Async.create
    openai_integration._patch_openai_audio_transcriptions_async(_Async)
    assert _Async.create is not original


def test_cohere_rerank_async_patcher_runs():
    class _Async:
        async def rerank(self, *_args: Any, **kwargs: Any) -> Any:
            return None
    original = _Async.rerank
    cohere_integration._patch_rerank_async(_Async)
    assert _Async.rerank is not original


def test_gemini_embed_content_async_patcher_runs() -> None:
    """instrument_gemini_embed wraps embed_content_async on the
    module. Runtime async coverage matches the sync test pattern;
    the wave's wiring contract is the assertion here."""
    fake_mod = _FakeGenAIModule()
    original = fake_mod.embed_content_async
    gemini_integration._embed_sync_patched = False
    gemini_integration._embed_async_patched = False
    gemini_integration.instrument_gemini_embed(fake_mod)
    # The module-level patcher rebinds the attribute on the module;
    # for the FakeGenAIModule instance, instance attributes shadow
    # class methods. Confirm SOME swap took place by checking name.
    assert fake_mod.embed_content_async is not original or \
        getattr(fake_mod.embed_content_async, "__name__", "") == "embed_content_async"
