"""
Demo for the identical-call loop detector (the 4th and final Phase-4
loop sub-detector after time-budget, step-count, and the deferred
similar-call detector).

An execution that calls the same LLM prompt three or more times is
flagged as a loop with a signature like
`identical_call_<8-hex-short-hash>`. The hash is SHA-256(model + user
message) truncated to 8 chars, so multiple loops with different prompts
in the same project end up in distinct groups.

Three runs:
  1. clean_agent, three DIFFERENT prompts. Not classified as loop.
  2. stuck_agent, same prompt 4 times. Triggers identical_call.
  3. doubly_stuck_agent, same prompt 6 times with a different
     prompt-text than #2, so it gets a DIFFERENT signature hash.

Prereqs:
  - Backend running: cd ../../backend && go run cmd/api/main.go
  - SDK installed: pip install -e ..

Run:
  python3 identical_call_agent.py
"""

import time

import mesedi


# ── Fake Anthropic-shaped types (same as anthropic_test.py) ──────────


class FakeUsage:
    def __init__(self, input_tokens, output_tokens):
        self.input_tokens = input_tokens
        self.output_tokens = output_tokens


class FakeTextBlock:
    def __init__(self, text):
        self.text = text


class FakeMessage:
    def __init__(self, content_text, in_tok, out_tok):
        self.content = [FakeTextBlock(content_text)]
        self.usage = FakeUsage(in_tok, out_tok)


class FakeMessages:
    def create(self, model="claude-fake", messages=None, system="", max_tokens=1024):
        time.sleep(0.005)
        last_user = ""
        for m in messages or []:
            if m.get("role") == "user":
                last_user = m.get("content", "")
        return FakeMessage(
            content_text="[fake response]",
            in_tok=max(1, len(last_user.split())),
            out_tok=10,
        )


class FakeAnthropicClient:
    def __init__(self):
        self.messages = FakeMessages()


mesedi.configure(
    api_key="mesedi_sk_dev_local_only",
    base_url="http://localhost:8080",
)
mesedi.instrument_anthropic(messages_class=FakeMessages)


@mesedi.wrap
def clean_agent(query: str) -> str:
    """Three different prompts, should NOT be classified as a loop."""
    c = FakeAnthropicClient()
    c.messages.create(model="claude-opus-4-6",
                      messages=[{"role": "user", "content": "What is 2+2?"}],
                      max_tokens=128)
    c.messages.create(model="claude-opus-4-6",
                      messages=[{"role": "user", "content": "What is the capital of France?"}],
                      max_tokens=128)
    c.messages.create(model="claude-opus-4-6",
                      messages=[{"role": "user", "content": "Tell me a haiku."}],
                      max_tokens=128)
    return "ok"


@mesedi.wrap
def stuck_agent(query: str) -> str:
    """Same prompt 4 times, should trigger identical_call loop."""
    c = FakeAnthropicClient()
    for _ in range(4):
        c.messages.create(
            model="claude-opus-4-6",
            messages=[{"role": "user", "content": "Did you understand my last request?"}],
            max_tokens=128,
        )
    return "stuck"


@mesedi.wrap
def doubly_stuck_agent(query: str) -> str:
    """Same prompt 6 times, different prompt-text than stuck_agent,
    so gets a different signature hash → separate failure_group."""
    c = FakeAnthropicClient()
    for _ in range(6):
        c.messages.create(
            model="claude-opus-4-6",
            messages=[{"role": "user", "content": "Please clarify the answer to my question."}],
            max_tokens=128,
        )
    return "doubly stuck"


def _ms(seconds: float) -> str:
    return f"{seconds * 1000:.0f}ms"


if __name__ == "__main__":
    print("\n── Run 1: clean_agent (3 different prompts, should NOT trigger) ──")
    t = time.perf_counter()
    clean_agent("x")
    print(f"  wall-clock: {_ms(time.perf_counter() - t)}")

    print("\n── Run 2: stuck_agent (same prompt 4x, triggers identical_call) ──")
    t = time.perf_counter()
    stuck_agent("x")
    print(f"  wall-clock: {_ms(time.perf_counter() - t)}")

    print("\n── Run 3: doubly_stuck_agent (different repeated prompt 6x) ──")
    t = time.perf_counter()
    doubly_stuck_agent("x")
    print(f"  wall-clock: {_ms(time.perf_counter() - t)}")

    print("\n── Flushing shipper queue... ──")
    t = time.perf_counter()
    ok = mesedi.flush(timeout=5.0)
    print(f"  flush ok={ok} in {_ms(time.perf_counter() - t)}")

    print("\n── Done. Verify: ──")
    print("  Dashboard: http://localhost:8080/ui/")
    print("  Expected: 2 new failure groups, both failure_class=loops,")
    print("            signature like identical_call_<short_hash> with")
    print("            DISTINCT hashes for stuck vs doubly_stuck.")
