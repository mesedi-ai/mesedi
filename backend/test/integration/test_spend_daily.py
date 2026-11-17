"""The Mesedi half of the self-run spend check, against the real
backend: model calls sent through the SDK appear in GET /me/spend/daily
for their UTC day and provider, and the check's reader sums them."""

from __future__ import annotations

from datetime import datetime, timezone

import httpx

from conftest import Backend


def test_spend_daily_reports_what_the_sdk_sent(backend: Backend, configured_sdk):
    mesedi = configured_sdk

    @mesedi.wrap
    def agent():
        # claude-haiku-4-5 is priced at $1.00 per million input tokens.
        mesedi.emit_llm_call(model="claude-haiku-4-5", user_message="hi", provider="anthropic",
                             input_tokens=1_000_000)
        mesedi.emit_llm_call(model="claude-haiku-4-5", user_message="hi", provider="openai",
                             input_tokens=500_000)

    agent()
    mesedi.flush(timeout=5.0)

    from mesedi import spend_check

    today = datetime.now(timezone.utc).date()
    with httpx.Client(timeout=10.0) as http:
        seen = spend_check.mesedi_spend(http, backend.base_url, [backend.api_key], "anthropic", today, today)
        seen_openai = spend_check.mesedi_spend(http, backend.base_url, [backend.api_key], "openai", today, today)
    assert seen[today.isoformat()] >= 1.0, seen
    assert seen_openai[today.isoformat()] >= 0.5, seen_openai
