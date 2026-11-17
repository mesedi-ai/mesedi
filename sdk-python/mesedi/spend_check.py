"""mesedi-spend-check: compare your AI provider's bill with the spend
Mesedi saw in monitored runs, on your own computer.

Why it exists. Spend that happens outside a monitored run, a stolen key
used from somewhere else included, never reaches Mesedi, so Mesedi
cannot alert on it. The provider's bill sees all of it. The difference
between the two, day by day, is the one place that spend shows up.

Why it runs here and not in Mesedi. Reading the bill needs the
provider's admin key, which can manage the whole AI account; there is
no read-only kind. Mesedi never holds it. This check reads the key from
an environment variable on your computer and sends it only to the
provider. It sends your Mesedi API key only to Mesedi, to read
``GET /me/spend/daily``.

What the numbers mean. Mesedi's figure is estimated from token counts
at list prices, so it will not match the bill to the cent: cached
tokens, batch discounts and non-token charges differ. A day is flagged
when the bill exceeds what Mesedi saw by more than the tolerance. A
flag says spend happened that no monitored run accounts for: an app not
instrumented with Mesedi, or a key used by someone else. Which one is
yours to find out. Set a spending limit at the provider as well; this
check finds spend after the fact, a limit stops it.

Usage::

    export MESEDI_API_KEY=mesedi_sk_...           # one per Mesedi project;
    export MESEDI_EXTRA_API_KEYS=mesedi_sk_...,...  # more projects, optional
    export ANTHROPIC_ADMIN_KEY=...                  # or OPENAI_ADMIN_KEY
    mesedi-spend-check --provider anthropic --days 30

Exit codes: 0 no day flagged, 1 at least one day flagged, 2 the check
could not run.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from dataclasses import dataclass
from datetime import date, datetime, timedelta, timezone
from typing import Callable, Dict, List, Optional

import httpx

ANTHROPIC_COST_REPORT = "https://api.anthropic.com/v1/organizations/cost_report"
OPENAI_COSTS = "https://api.openai.com/v1/organization/costs"
DEFAULT_MESEDI_URL = "https://api.mesedi.ai"
_MESEDI_MAX_DAYS = 93  # the endpoint's limit per request

ADMIN_KEY_ENV = {"anthropic": "ANTHROPIC_ADMIN_KEY", "openai": "OPENAI_ADMIN_KEY"}


class SpendCheckError(Exception):
    """The check could not run; the message says why."""


def _days(start: date, end: date) -> List[date]:
    return [start + timedelta(days=i) for i in range((end - start).days + 1)]


def anthropic_bill(http: httpx.Client, admin_key: str, start: date, end: date) -> Dict[str, float]:
    """USD per UTC day from Anthropic's Cost Report. Amounts are decimal
    strings in cents (the documented "lowest currency units")."""
    out: Dict[str, float] = {}
    begin = datetime(start.year, start.month, start.day, tzinfo=timezone.utc)
    finish = datetime(end.year, end.month, end.day, tzinfo=timezone.utc) + timedelta(days=1)
    params: Dict[str, str] = {
        "starting_at": begin.strftime("%Y-%m-%dT%H:%M:%SZ"),
        "ending_at": finish.strftime("%Y-%m-%dT%H:%M:%SZ"),
        "bucket_width": "1d",
        "limit": "31",
    }
    headers = {"x-api-key": admin_key, "anthropic-version": "2023-06-01", "Accept": "application/json"}
    while True:
        r = http.get(ANTHROPIC_COST_REPORT, params=params, headers=headers)
        if r.status_code != 200:
            raise SpendCheckError(f"Anthropic cost report: HTTP {r.status_code}: {r.text[:300]}")
        body = r.json()
        for bucket in body.get("data") or []:
            day = str(bucket.get("starting_at", ""))[:10]
            total = 0.0
            for res in bucket.get("results") or []:
                currency = str(res.get("currency") or "USD").upper()
                if currency != "USD":
                    raise SpendCheckError(f"Anthropic reported a cost in {currency}; this check compares USD only")
                total += float(res.get("amount") or 0) / 100.0
            out[day] = out.get(day, 0.0) + total
        if not body.get("has_more") or not body.get("next_page"):
            return out
        params["page"] = str(body["next_page"])


def openai_bill(http: httpx.Client, admin_key: str, start: date, end: date) -> Dict[str, float]:
    """USD per UTC day from OpenAI's organization Costs endpoint, whose
    amount.value is in the currency named beside it."""
    out: Dict[str, float] = {}
    begin = datetime(start.year, start.month, start.day, tzinfo=timezone.utc)
    finish = datetime(end.year, end.month, end.day, tzinfo=timezone.utc) + timedelta(days=1)
    params: Dict[str, str] = {
        "start_time": str(int(begin.timestamp())),
        "end_time": str(int(finish.timestamp())),
        "bucket_width": "1d",
        "limit": "180",
    }
    headers = {"Authorization": f"Bearer {admin_key}", "Content-Type": "application/json"}
    while True:
        r = http.get(OPENAI_COSTS, params=params, headers=headers)
        if r.status_code != 200:
            raise SpendCheckError(f"OpenAI costs: HTTP {r.status_code}: {r.text[:300]}")
        body = r.json()
        for bucket in body.get("data") or []:
            day = datetime.fromtimestamp(int(bucket.get("start_time", 0)), tz=timezone.utc).strftime("%Y-%m-%d")
            total = 0.0
            for res in bucket.get("results") or []:
                amount = res.get("amount") or {}
                currency = str(amount.get("currency") or "usd").upper()
                if currency != "USD":
                    raise SpendCheckError(f"OpenAI reported a cost in {currency}; this check compares USD only")
                total += float(amount.get("value") or 0)
            out[day] = out.get(day, 0.0) + total
        if not body.get("has_more") or not body.get("next_page"):
            return out
        params["page"] = str(body["next_page"])


BILL_READERS: Dict[str, Callable[[httpx.Client, str, date, date], Dict[str, float]]] = {
    "anthropic": anthropic_bill,
    "openai": openai_bill,
}


def mesedi_spend(http: httpx.Client, base_url: str, api_keys: List[str], provider: str,
                 start: date, end: date) -> Dict[str, float]:
    """USD per UTC day that Mesedi saw for this provider, summed over the
    given projects' keys."""
    out: Dict[str, float] = {}
    for key in api_keys:
        chunk_start = start
        while chunk_start <= end:
            chunk_end = min(end, chunk_start + timedelta(days=_MESEDI_MAX_DAYS - 1))
            r = http.get(f"{base_url.rstrip('/')}/me/spend/daily",
                         params={"from": chunk_start.isoformat(), "to": chunk_end.isoformat()},
                         headers={"Authorization": f"Bearer {key}"})
            if r.status_code != 200:
                raise SpendCheckError(f"Mesedi spend report: HTTP {r.status_code}: {r.text[:300]}")
            for d in r.json().get("days") or []:
                usd = float((d.get("providers") or {}).get(provider, 0.0))
                out[d["date"]] = out.get(d["date"], 0.0) + usd
            chunk_start = chunk_end + timedelta(days=1)
    return out


@dataclass
class DayResult:
    date: str
    bill_usd: float
    seen_usd: float
    unaccounted_usd: float
    flagged: bool


def compare(bill: Dict[str, float], seen: Dict[str, float], days: List[date],
            tolerance_usd: float, tolerance_percent: float) -> List[DayResult]:
    """Flag a day when the bill exceeds Mesedi's figure by more than
    tolerance_usd and by more than tolerance_percent of the bill."""
    out = []
    for d in days:
        key = d.isoformat()
        b, s = round(bill.get(key, 0.0), 6), round(seen.get(key, 0.0), 6)
        gap = round(b - s, 6)
        flagged = gap > tolerance_usd and gap > b * tolerance_percent / 100.0
        out.append(DayResult(key, b, s, gap, flagged))
    return out


def run_check(provider: str, start: date, end: date, admin_key: str, mesedi_keys: List[str],
              mesedi_url: str, tolerance_usd: float, tolerance_percent: float,
              http: Optional[httpx.Client] = None) -> List[DayResult]:
    if provider not in BILL_READERS:
        raise SpendCheckError(f"unknown provider {provider!r}; supported: {', '.join(sorted(BILL_READERS))}")
    if end < start:
        raise SpendCheckError("the period ends before it starts")
    client = http or httpx.Client(timeout=30.0)
    try:
        bill = BILL_READERS[provider](client, admin_key, start, end)
        seen = mesedi_spend(client, mesedi_url, mesedi_keys, provider, start, end)
    except httpx.HTTPError as exc:
        raise SpendCheckError(f"network error: {exc}") from exc
    finally:
        if http is None:
            client.close()
    return compare(bill, seen, _days(start, end), tolerance_usd, tolerance_percent)


def _say(text: str = "", err: bool = False) -> None:
    """The command's output: this is a command-line tool, so it writes to
    stdout (results) and stderr (why it could not run) on purpose."""
    (sys.stderr if err else sys.stdout).write(text + "\n")


def main(argv: Optional[List[str]] = None, http: Optional[httpx.Client] = None) -> int:
    p = argparse.ArgumentParser(
        prog="mesedi-spend-check",
        description="Compare your provider's bill with the spend Mesedi saw in monitored runs. "
        "Keys are read from the environment: MESEDI_API_KEY (and MESEDI_EXTRA_API_KEYS, comma "
        "separated), and ANTHROPIC_ADMIN_KEY or OPENAI_ADMIN_KEY. The admin key is sent only "
        "to the provider. Exit 0: no day flagged; 1: a day flagged; 2: the check could not run.")
    p.add_argument("--provider", required=True, choices=sorted(BILL_READERS))
    p.add_argument("--days", type=int, default=30,
                   help="whole UTC days to check, ending yesterday (default 30)")
    p.add_argument("--from", dest="start", help="first day, YYYY-MM-DD (instead of --days)")
    p.add_argument("--to", dest="end", help="last day, YYYY-MM-DD (default yesterday)")
    p.add_argument("--tolerance-usd", type=float, default=1.0,
                   help="ignore a day's difference up to this many dollars (default 1)")
    p.add_argument("--tolerance-percent", type=float, default=10.0,
                   help="ignore a day's difference up to this share of the bill (default 10)")
    p.add_argument("--mesedi-url", default=os.environ.get("MESEDI_BASE_URL", DEFAULT_MESEDI_URL))
    p.add_argument("--json", action="store_true", help="machine-readable output")
    args = p.parse_args(argv)

    admin_env = ADMIN_KEY_ENV[args.provider]
    admin_key = os.environ.get(admin_env, "").strip()
    keys = [k.strip() for k in [os.environ.get("MESEDI_API_KEY", "")] +
            os.environ.get("MESEDI_EXTRA_API_KEYS", "").split(",") if k.strip()]
    try:
        if not admin_key:
            raise SpendCheckError(f"set {admin_env} to the provider's admin key")
        if not keys:
            raise SpendCheckError("set MESEDI_API_KEY to a Mesedi project's API key")
        yesterday = datetime.now(timezone.utc).date() - timedelta(days=1)
        end = date.fromisoformat(args.end) if args.end else yesterday
        start = date.fromisoformat(args.start) if args.start else end - timedelta(days=args.days - 1)
        results = run_check(args.provider, start, end, admin_key, keys, args.mesedi_url,
                            args.tolerance_usd, args.tolerance_percent, http=http)
    except (SpendCheckError, ValueError) as exc:
        _say(f"mesedi-spend-check: {exc}", err=True)
        return 2

    flagged = [r for r in results if r.flagged]
    if args.json:
        _say(json.dumps({"provider": args.provider, "days": [r.__dict__ for r in results],
                          "flagged_days": [r.date for r in flagged]}, indent=2))
    else:
        _say(f"{args.provider} bill against spend Mesedi saw, per UTC day (USD)")
        _say(f"{'day':<12}{'bill':>12}{'Mesedi saw':>14}{'unaccounted':>14}")
        for r in results:
            mark = "  FLAG" if r.flagged else ""
            _say(f"{r.date:<12}{r.bill_usd:>12.2f}{r.seen_usd:>14.2f}{r.unaccounted_usd:>14.2f}{mark}")
        if flagged:
            _say(f"\n{len(flagged)} day(s) flagged: the bill exceeds what monitored runs account "
                  "for. That is an app not instrumented with Mesedi, or a key used by someone "
                  "else. Check the provider's console for which key spent it, and set a spending "
                  "limit at the provider.")
        else:
            _say("\nNo day flagged. Mesedi's figure is an estimate at list prices, so small "
                  "differences are expected.")
    return 1 if flagged else 0


if __name__ == "__main__":  # pragma: no cover
    sys.exit(main())
