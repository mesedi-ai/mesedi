"""
End-to-end test of mesedi.instrument_anthropic() — WITHOUT requiring
the real anthropic package to be installed.

How it works: ``instrument_anthropic()`` accepts an optional
``messages_class`` parameter that lets the caller inject the class to
patch. In production this is the actual
``anthropic.resources.messages.Messages``. Here we inject a tiny
hand-rolled ``FakeMessages`` class with the same shape (a ``create()``
method, a Message-shaped response with ``content`` blocks and a
``usage`` attribute). The patching logic + payload extraction is
identical to what runs against the real package.

Prereqs:
  - Backend running:
      cd ../../backend && go run cmd/api/main.go
  - SDK installed locally in editable mode:
      cd ../  &&  python3 -m pip install -e .

Run:
  python3 anthropic_test.py

Verify in SQLite:
  cd ../../backend
  sqlite3 mesedi-dev.db "
    SELECT event_type, sequence,
           json_extract(payload, '$.model')          AS model,
           json_extract(payload, '$.status')         AS status,
           json_extract(payload, '$.input_tokens')   AS in_tok,
           json_extract(payload, '$.output_tokens')  AS out_tok,
           duration_ms
    FROM events ORDER BY rowid DESC LIMIT 5;
    "
"""

import time

import mesedi

mesedi.configure(
    api_key="mesedi_sk_dev_local_only",
    base_url="http://localhost:8080",
)


# ── Fake Anthropic-shaped classes (no real API call) ─────────────────


class FakeUsage:
    """Mirrors anthropic.types.Usage."""

    def __init__(self, input_tokens: int, output_tokens: int):
        self.input_tokens = input_tokens
        self.output_tokens = output_tokens


class FakeTextBlock:
    """Mirrors anthropic.types.TextBlock."""

    def __init__(self, text: str):
        self.text = text


class FakeMessage:
    """Mirrors anthropic.types.Message."""

    def __init__(self, content_text: str, in_tok: int, out_tok: int):
        self.content = [FakeTextBlock(content_text)]
        self.usage = FakeUsage(in_tok, out_tok)


class FakeMessages:
    """Stand-in for anthropic.resources.messages.Messages.

    The real Anthropic SDK has roughly this shape:
        client = anthropic.Anthropic()
        response = client.messages.create(model=..., messages=[...])

    instrument_anthropic() patches the .create() method on the class so
    every call from every instance becomes observed. We test that same
    path here.
    """

    def create(
        self,
        model: str = "claude-fake",
        messages: list = None,  # type: ignore[assignment]
        system: str = "",
        max_tokens: int = 1024,
    ) -> FakeMessage:
        # Pretend to call the Anthropic API. Simulate a 30ms latency
        # so duration_ms is non-zero and the timing path is exercised.
        time.sleep(0.03)

        # Simulate a 5x ratio between output and input tokens (typical
        # for assistant responses to short user prompts).
        last_user_message = ""
        for msg in (messages or []):
            if msg.get("role") == "user":
                last_user_message = msg.get("content", "")

        input_tokens = max(1, len(last_user_message.split()))
        output_tokens = input_tokens * 5
        return FakeMessage(
            content_text=f"[fake response to {last_user_message!r}]",
            in_tok=input_tokens,
            out_tok=output_tokens,
        )


class FakeAnthropicClient:
    """Stand-in for anthropic.Anthropic — exposes a .messages attribute."""

    def __init__(self):
        self.messages = FakeMessages()


# ── Patch the fake class, then exercise it inside @wrap ──────────────


# Inject our fake into instrument_anthropic. This is the exact same
# patching code that runs against the real anthropic package — we just
# point it at FakeMessages instead.
patched_ok = mesedi.instrument_anthropic(messages_class=FakeMessages)
print(f"\ninstrument_anthropic(FakeMessages) → {patched_ok}")


@mesedi.wrap
def ask_fake_anthropic(question: str) -> str:
    """Agent that calls the (fake) Anthropic API once."""
    client = FakeAnthropicClient()
    response = client.messages.create(
        model="claude-opus-4-6",
        system="You are a helpful research assistant.",
        messages=[{"role": "user", "content": question}],
        max_tokens=1024,
    )
    return response.content[0].text


@mesedi.wrap
def multi_turn_agent(question: str) -> str:
    """Agent that makes TWO LLM calls in one execution.

    Verifies that sequence numbers are assigned correctly across
    multiple LLM calls in the same execution.
    """
    client = FakeAnthropicClient()
    response1 = client.messages.create(
        model="claude-opus-4-6",
        messages=[{"role": "user", "content": f"think about: {question}"}],
        max_tokens=512,
    )
    response2 = client.messages.create(
        model="claude-opus-4-6",
        messages=[{"role": "user", "content": "now summarize that"}],
        max_tokens=256,
    )
    return f"{response1.content[0].text} → {response2.content[0].text}"


def _ms(seconds: float) -> str:
    return f"{seconds * 1000:.1f}ms"


if __name__ == "__main__":
    print("\n── Run 1: ask_fake_anthropic (1 LLM call) ──")
    t = time.perf_counter()
    result = ask_fake_anthropic("what is the meaning of life?")
    print(f"  wall-clock: {_ms(time.perf_counter() - t)}")
    print(f"  result: {result}")

    print("\n── Run 2: multi_turn_agent (2 LLM calls in one execution) ──")
    t = time.perf_counter()
    result = multi_turn_agent("pickleball strategy")
    print(f"  wall-clock: {_ms(time.perf_counter() - t)}")
    print(f"  result: {result}")

    print("\n── Run 3: instrument_anthropic is idempotent ──")
    re_patched = mesedi.instrument_anthropic(messages_class=FakeMessages)
    print(f"  second call returned: {re_patched} (expected: True, no double-wrap)")

    print("\n── Flushing shipper queue... ──")
    t = time.perf_counter()
    ok = mesedi.flush(timeout=5.0)
    print(f"  flush ok={ok} in {_ms(time.perf_counter() - t)}")

    print("\n── Done. Verify in SQLite:")
    print("  cd ../../backend")
    print(
        '  sqlite3 mesedi-dev.db "SELECT event_type, sequence, '
        "json_extract(payload, '\\$.model') AS model, "
        "json_extract(payload, '\\$.status') AS status, "
        "json_extract(payload, '\\$.input_tokens') AS in_tok, "
        "json_extract(payload, '\\$.output_tokens') AS out_tok, "
        'duration_ms FROM events ORDER BY rowid DESC LIMIT 5;"'
    )
