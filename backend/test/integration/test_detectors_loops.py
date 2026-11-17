"""
test_detectors_loops.py: the identical_call_loop integration test, which
makes a real model call and so skips without ANTHROPIC_API_KEY.

Moved out of test_detectors.py when it gained its missing skip mark,
because that file is past the size the commit audit allows to grow.
"""

from __future__ import annotations

import os

import pytest

from conftest import Backend, await_failure_group

needs_anthropic = pytest.mark.skipif(
    not os.environ.get("ANTHROPIC_API_KEY"),
    reason="ANTHROPIC_API_KEY not set; skipping real-LLM scenario",
)


@needs_anthropic
def test_identical_call_loop(backend: Backend, configured_sdk):
    """Three llm_call events with identical (model, user_message)
    hash within one execution → identical_call_loop detector fires
    under the loops failure class.

    Detector chain: token_waste runs earlier in the chain on the
    same llm_call payloads, but the two detectors create INDEPENDENT
    failure_group rows on the same execution rather than first-
    match-wins claiming. Both clusters surface (a `token_waste:<hex>`
    cluster AND an `identical_call_<hash>` cluster) — by design,
    each detector answers a different question about the same
    underlying pattern. This test asserts the identical_call signal
    is present; the parallel `test_token_waste` asserts the
    token_waste signal.

    (Closes loops.G1 — earlier audit-doc claim that identical_call_loop
    was \"structurally suppressed by token_waste\" was stale. The
    chain has never short-circuited at this junction; both Group<X>
    calls run independently against the same execution row.)"""
    from anthropic import Anthropic

    mesedi = configured_sdk
    client = Anthropic()
    prompt = "In one short sentence, name a color."

    @mesedi.wrap
    def looping_agent():
        for _ in range(3):
            client.messages.create(
                model="claude-haiku-4-5",
                max_tokens=16,
                messages=[{"role": "user", "content": prompt}],
            )

    looping_agent()
    mesedi.flush(timeout=5.0)

    await_failure_group(
        backend,
        failure_class="loops",
        signature_prefix="identical_call_",
    )
