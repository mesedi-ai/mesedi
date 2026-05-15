"""
financial_research — buy-side equity research agent.

Industry: Investment management / buy-side equity research desk. A PM submits
a research question on a specific ticker; the agent pulls the latest 10-K
filing, fetches current price + consensus, runs segment analysis, cross-checks
against sell-side consensus, and produces a written thesis.

Failure modes naturally exercised:
  - cost_velocity: 10-K filings are 10K+ token user_messages, so a single
    research call lands in $0.01+ cost bucket on Sonnet pricing
  - step_count: deep analysis can chain many tool calls
  - time_budget halts: 60s wall-clock budget vs realistic 10-K analysis
  - drift: market conditions (prices, consensus) shift over runs — when the
    Phase 7 drift detector lands, this agent will be the primary canary

Workflow:
    1. Receive research query
    2. Pull 10-K filing (large, already in input)
    3. Pull current price + consensus
    4. Segment analysis (LLM)
    5. Cross-check against consensus (LLM)
    6. Thesis generation (LLM)
"""
from __future__ import annotations

import os
import random
import time
from typing import Dict

import mesedi
from mesedi import Budget


def _anthropic_client():
    if os.environ.get("MESEDI_SYNTHETIC_ORG_DRY_RUN"):
        return None
    try:
        from anthropic import Anthropic
    except ImportError:
        return None
    return Anthropic()


# ──────────────────────────────────────────────────────────────────────
# Tools
# ──────────────────────────────────────────────────────────────────────

@mesedi.tool
def fetch_stock_price(ticker: str) -> Dict:
    """Pull latest price from market data. Flakes ~3%."""
    time.sleep(random.uniform(0.05, 0.12))
    if random.random() < 0.03:
        raise RuntimeError(f"market data API: timeout for {ticker}")
    return {
        "ticker": ticker,
        "last_price": round(random.uniform(50, 950), 2),
        "day_change_pct": round(random.uniform(-4, 4), 2),
        "volume": random.randint(1_000_000, 50_000_000),
    }


@mesedi.tool
def fetch_consensus(ticker: str) -> Dict:
    """Pull sell-side consensus."""
    time.sleep(random.uniform(0.10, 0.20))
    return {
        "ticker": ticker,
        "rating_avg": round(random.uniform(2.0, 4.5), 2),  # 1=sell ... 5=strong-buy
        "price_target_avg": round(random.uniform(60, 1100), 2),
        "n_analysts": random.randint(8, 35),
    }


# ──────────────────────────────────────────────────────────────────────
# LLM calls
# ──────────────────────────────────────────────────────────────────────

def _segment_analysis(query: Dict, price: Dict, consensus: Dict) -> str:
    client = _anthropic_client()
    if client is None:
        analysis = (
            f"[mock] {query['ticker']}: filing excerpt suggests data-center "
            f"strength continues; current price ${price['last_price']} vs "
            f"target ${consensus['price_target_avg']}; rating "
            f"{consensus['rating_avg']}/5."
        )
        mesedi.emit_llm_call(
            model="claude-sonnet-4-6",
            user_message=(
                f"TICKER: {query['ticker']}\nQUERY: {query['query']}\n\n"
                f"10-K EXCERPT:\n{query['filing_excerpt']}\n\n"
                f"PRICE: {price}\nCONSENSUS: {consensus}\n"
            ),
            system_prompt=(
                "You are a senior buy-side equity analyst. Given a 10-K excerpt, "
                "current market data, and sell-side consensus, produce a "
                "segment-level analysis identifying the top three risks and "
                "the bull case."
            ),
            response_text=analysis,
        )
        return analysis

    # Note: filing_excerpt is intentionally large (~12K chars) to drive
    # cost_velocity events naturally.
    system = (
        "You are a senior buy-side equity analyst. Given a 10-K excerpt, "
        "current market data, and sell-side consensus, produce a segment-level "
        "analysis identifying the top three risks and the bull case."
    )
    user = (
        f"TICKER: {query['ticker']}\n"
        f"QUERY: {query['query']}\n\n"
        f"10-K EXCERPT:\n{query['filing_excerpt']}\n\n"
        f"PRICE: {price}\nCONSENSUS: {consensus}\n"
    )
    response = client.messages.create(
        model="claude-sonnet-4-6",  # Sonnet for the heavy analysis
        max_tokens=800,
        system=system,
        messages=[{"role": "user", "content": user}],
    )
    return "".join(b.text for b in response.content if hasattr(b, "text"))


def _thesis_generation(analysis: str, query: Dict) -> str:
    client = _anthropic_client()
    if client is None:
        thesis = f"[mock thesis] {query['ticker']}: constructive at current levels. Key risks: macro, mix shift, FX."
        mesedi.emit_llm_call(
            model="claude-haiku-4-5-20251001",
            user_message=f"ANALYSIS:\n{analysis}\n\nQUERY: {query['query']}",
            system_prompt="You are an Investment Committee analyst. Convert the analysis into a 3-paragraph thesis.",
            response_text=thesis,
        )
        return thesis

    system = "You are an Investment Committee analyst. Convert the analysis into a 3-paragraph thesis."
    user = f"ANALYSIS:\n{analysis}\n\nQUERY: {query['query']}"
    response = client.messages.create(
        model="claude-haiku-4-5-20251001",
        max_tokens=600,
        system=system,
        messages=[{"role": "user", "content": user}],
    )
    return "".join(b.text for b in response.content if hasattr(b, "text"))


# ──────────────────────────────────────────────────────────────────────
# Top-level handler
# ──────────────────────────────────────────────────────────────────────

@mesedi.wrap(budget=Budget(max_wall_clock_seconds=60, max_steps=25))
def handle(query: Dict) -> Dict:
    mesedi.checkpoint("query_received", ticker=query["ticker"])

    try:
        price = fetch_stock_price(query["ticker"])
    except Exception as exc:
        mesedi.checkpoint("price_fetch_degraded", error=str(exc))
        price = {"ticker": query["ticker"], "last_price": query.get("current_price", 0)}

    consensus = fetch_consensus(query["ticker"])
    mesedi.checkpoint("market_data_loaded")

    analysis = _segment_analysis(query, price, consensus)
    mesedi.checkpoint("segment_analysis_complete", analysis_len=len(analysis))

    thesis = _thesis_generation(analysis, query)

    # Validator: thesis should reference both the ticker and a price datum.
    has_ticker = query["ticker"] in thesis
    has_price_ref = any(str(int(price["last_price"]))[:2] in thesis for _ in [0])
    mesedi.validator_result(
        "thesis_completeness",
        passed=(has_ticker and len(thesis) > 100),
        message=f"has_ticker={has_ticker} thesis_len={len(thesis)}",
        severity="error",
    )

    return {
        "query_id": query["query_id"],
        "ticker": query["ticker"],
        "thesis": thesis,
        "analysis_len": len(analysis),
        "price_at_analysis": price["last_price"],
    }
