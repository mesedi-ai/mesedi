"""mesedi-spend-check against stand-in provider and Mesedi endpoints
answering in the documented formats (Anthropic Cost Report, OpenAI
organization Costs, Mesedi GET /me/spend/daily)."""

from __future__ import annotations

import json
from datetime import date
from typing import Any, Dict, List

import httpx

from mesedi import spend_check

SEEN = {"2026-11-10": {"anthropic": 4.0, "openai": 1.0}, "2026-11-11": {"anthropic": 2.0}}


def _transport(calls: List[httpx.Request], anthropic_pages: List[Dict[str, Any]], openai_body: Dict[str, Any]) -> httpx.MockTransport:
    pages = list(anthropic_pages)

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        if request.url.host == "api.anthropic.com":
            return httpx.Response(200, json=pages.pop(0))
        if request.url.host == "api.openai.com":
            return httpx.Response(200, json=openai_body)
        if request.url.path == "/me/spend/daily":
            days = [{"date": d, "providers": SEEN.get(d, {})} for d in ("2026-11-10", "2026-11-11", "2026-11-12")]
            return httpx.Response(200, json={"days": days})
        return httpx.Response(404)

    return httpx.MockTransport(handler)


ANTHROPIC_PAGES = [
    {"data": [{"starting_at": "2026-11-10T00:00:00Z", "ending_at": "2026-11-11T00:00:00Z",
               "results": [{"amount": "400.00", "currency": "USD"}, {"amount": "25.5", "currency": "USD"}]}],
     "has_more": True, "next_page": "page_2"},
    {"data": [{"starting_at": "2026-11-11T00:00:00Z", "ending_at": "2026-11-12T00:00:00Z",
               "results": [{"amount": "5200", "currency": "USD"}]},
              {"starting_at": "2026-11-12T00:00:00Z", "ending_at": "2026-11-13T00:00:00Z", "results": []}],
     "has_more": False, "next_page": None},
]


def test_anthropic_bill_against_mesedi() -> None:
    calls: List[httpx.Request] = []
    http = httpx.Client(transport=_transport(calls, ANTHROPIC_PAGES, {}))
    results = spend_check.run_check("anthropic", date(2026, 11, 10), date(2026, 11, 12), "ADMINKEY",
                                    ["mesedi_sk_a"], "https://mesedi.test", 1.0, 10.0, http=http)
    by_day = {r.date: r for r in results}
    # Day 1: $4.255 billed, $4 seen: within $1. Day 2: $52 billed, $2 seen.
    assert not by_day["2026-11-10"].flagged and round(by_day["2026-11-10"].bill_usd, 3) == 4.255
    assert by_day["2026-11-11"].flagged and by_day["2026-11-11"].unaccounted_usd == 50.0
    assert not by_day["2026-11-12"].flagged

    anthropic_calls = [c for c in calls if c.url.host == "api.anthropic.com"]
    assert len(anthropic_calls) == 2 and anthropic_calls[1].url.params["page"] == "page_2"
    assert anthropic_calls[0].url.params["starting_at"] == "2026-11-10T00:00:00Z"
    assert anthropic_calls[0].url.params["ending_at"] == "2026-11-13T00:00:00Z"
    # The admin key goes to the provider and nowhere else.
    for c in calls:
        sent = str(c.headers) + str(c.url)
        if c.url.host == "api.anthropic.com":
            assert c.headers["x-api-key"] == "ADMINKEY"
        else:
            assert "ADMINKEY" not in sent
            assert c.headers["authorization"] == "Bearer mesedi_sk_a"


def test_openai_bill() -> None:
    body = {"object": "page", "data": [
        {"object": "bucket", "start_time": 1794268800, "end_time": 1794355200,  # 2026-11-10
         "results": [{"object": "organization.costs.result", "amount": {"value": 30.0, "currency": "usd"}}]}],
        "has_more": False, "next_page": None}
    calls: List[httpx.Request] = []
    http = httpx.Client(transport=_transport(calls, [], body))
    results = spend_check.run_check("openai", date(2026, 11, 10), date(2026, 11, 10), "OPENAIADMIN",
                                    ["mesedi_sk_a"], "https://mesedi.test", 1.0, 10.0, http=http)
    assert results[0].bill_usd == 30.0 and results[0].seen_usd == 1.0 and results[0].flagged
    oc = [c for c in calls if c.url.host == "api.openai.com"][0]
    assert oc.headers["authorization"] == "Bearer OPENAIADMIN"
    assert oc.url.params["start_time"] == "1794268800" and oc.url.params["bucket_width"] == "1d"


def test_tolerance_needs_both_dollars_and_percent() -> None:
    days = [date(2026, 11, 10)]
    # $5 over on a $1,000 bill: over the $1 floor, under 10 percent.
    assert not spend_check.compare({"2026-11-10": 1000.0}, {"2026-11-10": 995.0}, days, 1.0, 10.0)[0].flagged
    # $0.80 over on a $1 bill: over 10 percent, under the $1 floor.
    assert not spend_check.compare({"2026-11-10": 1.0}, {"2026-11-10": 0.2}, days, 1.0, 10.0)[0].flagged
    assert spend_check.compare({"2026-11-10": 20.0}, {"2026-11-10": 0.0}, days, 1.0, 10.0)[0].flagged


def test_several_projects_add_up() -> None:
    calls: List[httpx.Request] = []
    http = httpx.Client(transport=_transport(calls, [], {}))
    seen = spend_check.mesedi_spend(http, "https://mesedi.test", ["k1", "k2"], "anthropic",
                                    date(2026, 11, 10), date(2026, 11, 11))
    assert seen["2026-11-10"] == 8.0 and seen["2026-11-11"] == 4.0


def test_cli_exit_codes(monkeypatch: Any, capsys: Any) -> None:
    monkeypatch.delenv("ANTHROPIC_ADMIN_KEY", raising=False)
    monkeypatch.setenv("MESEDI_API_KEY", "mesedi_sk_a")
    assert spend_check.main(["--provider", "anthropic"]) == 2
    assert "ANTHROPIC_ADMIN_KEY" in capsys.readouterr().err

    monkeypatch.setenv("ANTHROPIC_ADMIN_KEY", "ADMINKEY")
    http = httpx.Client(transport=_transport([], [dict(p) for p in ANTHROPIC_PAGES], {}))
    code = spend_check.main(["--provider", "anthropic", "--from", "2026-11-10", "--to", "2026-11-12",
                             "--mesedi-url", "https://mesedi.test", "--json"], http=http)
    out = json.loads(capsys.readouterr().out)
    assert code == 1 and out["flagged_days"] == ["2026-11-11"]


def test_a_failing_provider_is_exit_2(monkeypatch: Any, capsys: Any) -> None:
    monkeypatch.setenv("ANTHROPIC_ADMIN_KEY", "ADMINKEY")
    monkeypatch.setenv("MESEDI_API_KEY", "mesedi_sk_a")
    http = httpx.Client(transport=httpx.MockTransport(lambda r: httpx.Response(401, text="bad key")))
    assert spend_check.main(["--provider", "anthropic", "--days", "2"], http=http) == 2
    assert "HTTP 401" in capsys.readouterr().err


def test_say_writes_results_to_stdout_and_reasons_to_stderr(capsys: Any) -> None:
    spend_check._say("result line")
    spend_check._say("could not run", err=True)
    out = capsys.readouterr()
    assert out.out == "result line\n" and out.err == "could not run\n"
