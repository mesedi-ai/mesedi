"""
support_triager — SaaS customer-support ticket-triage agent.

Industry: B2B SaaS customer support.

Workflow:
    1. Receive a support ticket.
    2. Classify it (billing / technical / account / refund / feature_request).
    3. Look up the customer in the CRM.
    4. Search the knowledge base for relevant articles.
    5. Draft a response.
    6. Quality-check the response.
    7. Send (or escalate to human if quality fails).

This is the **reference implementation** for synthetic-org agents. Every other
agent in the folder follows the same patterns at smaller scale:

  - top-level @mesedi.wrap with a Budget()
  - @mesedi.tool for each external-side-effect function
  - mesedi.checkpoint() at workflow milestones
  - mesedi.validator_result() for quality / schema / safety checks
  - LLM calls go through Anthropic; auto-instrumented via
    mesedi.instrument_anthropic() at runner startup
  - dry-run mode (MESEDI_SYNTHETIC_ORG_DRY_RUN=1) replaces the LLM call
    with a deterministic mock, so structure runs free.

Failure modes naturally produced (none deliberately planted):
  - tool_failures when crm_lookup / knowledge_base_search hit their
    random flake rate (~5–8%)
  - validator_failures when the quality-check LLM rates a response < 0.6
  - prompt_injection when the inbound ticket body contains an injection
    payload (regex-detected on the backend after the event arrives)
  - cost_velocity on unusually long tickets
"""
from __future__ import annotations

import os
import random
import time
from typing import Dict, List

import mesedi
from mesedi import Budget


# Wire up Anthropic lazily — the runner is responsible for calling
# mesedi.instrument_anthropic(). We just need the client.
def _anthropic_client():
    """Return an Anthropic client, or None in dry-run mode."""
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
def crm_lookup(customer_id: str) -> Dict:
    """Fetch the customer record from the CRM. Flakes ~6% of the time
    (simulated transient API failure) and raises on flake — Mesedi will
    record a failed tool_call event with the exception in the payload."""
    time.sleep(random.uniform(0.05, 0.15))
    if random.random() < 0.06:
        raise RuntimeError(f"CRM API timeout fetching {customer_id}")
    return {
        "customer_id": customer_id,
        "name": f"Acme {customer_id.split('-')[-1]}",
        "plan_tier": random.choice(["free", "starter", "growth", "pro", "enterprise"]),
        "monthly_value_usd": random.randint(0, 5000),
        "csat_30d": round(random.uniform(2.5, 5.0), 1),
        "open_tickets": random.randint(0, 4),
    }


@mesedi.tool
def knowledge_base_search(query: str, k: int = 3) -> List[Dict]:
    """Search the KB for relevant articles. Flakes ~4% of the time."""
    time.sleep(random.uniform(0.08, 0.18))
    if random.random() < 0.04:
        raise RuntimeError("knowledge-base service unavailable (503)")
    return [
        {
            "article_id": f"kb-{random.randint(1000, 9999)}",
            "title": f"How to handle {query[:30]}",
            "snippet": "Step 1: confirm the issue. Step 2: ...",
            "relevance_score": round(random.uniform(0.5, 0.95), 2),
        }
        for _ in range(k)
    ]


@mesedi.tool
def send_response(ticket_id: str, body: str) -> Dict:
    """Send the drafted response to the customer."""
    time.sleep(random.uniform(0.05, 0.10))
    return {"ticket_id": ticket_id, "status": "sent", "delivery_ms": random.randint(80, 220)}


@mesedi.tool
def escalate_to_human(ticket_id: str, reason: str) -> Dict:
    """Escalate the ticket to a human agent."""
    time.sleep(random.uniform(0.03, 0.08))
    return {"ticket_id": ticket_id, "status": "escalated", "reason": reason}


# ──────────────────────────────────────────────────────────────────────
# LLM calls (real or mock)
# ──────────────────────────────────────────────────────────────────────

def _classify_and_draft(ticket: Dict, customer: Dict, kb_articles: List[Dict]) -> Dict:
    """Single LLM call that returns {category, draft_response, confidence}."""
    client = _anthropic_client()
    system = (
        "You are a senior customer-support engineer for a B2B SaaS product. "
        "Given a ticket plus customer context plus knowledge-base snippets, "
        "classify the ticket and draft a concise, accurate response. "
        "Never invent facts. If unsure, recommend escalation."
    )
    user = (
        f"TICKET:\nSubject: {ticket['subject']}\nBody: {ticket['body']}\n"
        f"Priority: {ticket['priority']}\n\n"
        f"CUSTOMER:\n{customer}\n\n"
        f"KNOWLEDGE BASE:\n{kb_articles}\n\n"
        "Return: classification (one word), draft (≤200 words), "
        "and your confidence (0-1)."
    )

    if client is None:
        # Dry-run mock — emit a synthetic llm_call event so detectors
        # that read llm_call payloads (drift, similar-call, identical-
        # call, cost-velocity, prompt-injection) still dogfood correctly.
        draft = (
            f"Hi — thanks for reaching out about your {ticket['category_hint']} "
            f"question. Based on your {customer['plan_tier']} plan and the "
            f"linked KB articles, here's what I can confirm... [mock response]."
        )
        mesedi.emit_llm_call(
            model="claude-haiku-4-5-20251001",
            user_message=user,
            system_prompt=system,
            response_text=draft,
        )
        return {
            "category": ticket.get("category_hint", "technical"),
            "draft": draft,
            "confidence": round(random.uniform(0.5, 0.95), 2),
        }

    response = client.messages.create(
        model="claude-haiku-4-5-20251001",
        max_tokens=500,
        system=system,
        messages=[{"role": "user", "content": user}],
    )
    text = "".join(b.text for b in response.content if hasattr(b, "text"))
    # Lightweight parse — production version would use JSON-mode.
    return {
        "category": "technical",  # default; real parsing in a real product
        "draft": text,
        "confidence": 0.75,
    }


def _quality_check(draft: str) -> Dict:
    """Score the draft response. Returns {pass: bool, score: float, reason: str}.

    In dry-run mode, ~12% of drafts fail the quality bar (deterministic via
    Python's RNG state). With a real LLM, the LLM grades it."""
    client = _anthropic_client()
    if client is None:
        score = random.uniform(0.3, 0.95)
        passed = score >= 0.6
        # Synthetic llm_call event for quality-check round-trip so the
        # second LLM call is also detector-visible in dry-run mode.
        mesedi.emit_llm_call(
            model="claude-haiku-4-5-20251001",
            user_message=f"Draft response:\n{draft}\n\nReturn a single float 0-1.",
            system_prompt="You are a QA reviewer. Rate the customer-support response 0-1.",
            response_text=f"{score:.2f}",
        )
        return {
            "passed": passed,
            "score": round(score, 2),
            "reason": "ok" if passed else "draft too generic; lacks customer-specific context",
        }

    response = client.messages.create(
        model="claude-haiku-4-5-20251001",
        max_tokens=200,
        system="You are a QA reviewer. Rate the customer-support response 0-1.",
        messages=[{"role": "user", "content": f"Draft response:\n{draft}\n\nReturn a single float 0-1."}],
    )
    text = "".join(b.text for b in response.content if hasattr(b, "text"))
    try:
        score = float(text.strip().split()[0])
    except (ValueError, IndexError):
        score = 0.5
    return {
        "passed": score >= 0.6,
        "score": score,
        "reason": "ok" if score >= 0.6 else "below quality threshold",
    }


# ──────────────────────────────────────────────────────────────────────
# Top-level handler
# ──────────────────────────────────────────────────────────────────────

@mesedi.wrap(budget=Budget(max_wall_clock_seconds=30, max_steps=20))
def handle(ticket: Dict) -> Dict:
    """Triage one support ticket. Returns {resolution, sent_or_escalated, ...}.

    Mesedi-wrapped; halts at the wall-clock or step-count budget if the
    agent stalls.
    """
    mesedi.checkpoint("ticket_received", ticket_id=ticket["ticket_id"], priority=ticket["priority"])

    # Step 1: CRM lookup. Tool may flake — Mesedi records the tool_call
    # exception event and we degrade to "no customer context" rather than
    # crashing the whole agent.
    try:
        customer = crm_lookup(ticket["customer_id"])
    except Exception as exc:
        mesedi.checkpoint("crm_lookup_degraded", error=str(exc))
        customer = {"customer_id": ticket["customer_id"], "plan_tier": "unknown", "monthly_value_usd": 0}

    # Step 2: KB search. Same flake-tolerance pattern.
    try:
        kb_articles = knowledge_base_search(ticket["body"][:80])
    except Exception as exc:
        mesedi.checkpoint("kb_search_degraded", error=str(exc))
        kb_articles = []

    mesedi.checkpoint("context_gathered", kb_hits=len(kb_articles), customer_tier=customer.get("plan_tier"))

    # Step 3: classify + draft (LLM call).
    result = _classify_and_draft(ticket, customer, kb_articles)
    draft = result["draft"]

    # Step 4: quality check (second LLM call).
    qc = _quality_check(draft)
    mesedi.validator_result(
        "response_quality",
        passed=qc["passed"],
        message=f"score={qc['score']:.2f} reason={qc['reason']}",
        severity="warning" if not qc["passed"] else "error",
    )

    # Step 5: send or escalate.
    if qc["passed"]:
        delivery = send_response(ticket["ticket_id"], draft)
        return {
            "ticket_id": ticket["ticket_id"],
            "outcome": "sent",
            "category": result["category"],
            "confidence": result["confidence"],
            "quality_score": qc["score"],
            "delivery_ms": delivery["delivery_ms"],
        }
    else:
        esc = escalate_to_human(ticket["ticket_id"], reason=qc["reason"])
        return {
            "ticket_id": ticket["ticket_id"],
            "outcome": "escalated",
            "reason": esc["reason"],
            "quality_score": qc["score"],
        }
