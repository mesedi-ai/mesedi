"""
contract_reviewer — legal contract redline agent.

Industry: Law firm / in-house legal. A clause arrives with a redline objective;
the agent classifies the clause type, looks up the firm's playbook position,
fetches precedent language, drafts redlines, and validates them.

Failure modes naturally exercised:
  - identical_call loops: the agent re-reads the same clause across multiple
    drafting iterations (the same model+user_message hash recurs), which
    triggers the loops:identical_call detector
  - step_count: deep precedent search chains many tool calls
  - validator_failures: redline schema check fails when redlines lack
    required structure

Workflow:
    1. Receive clause + redline objective
    2. Classify clause type
    3. Look up playbook position (tool)
    4. Fetch precedents (tool)
    5. Draft redlines (LLM — called 2–3 times intentionally with same input
       to simulate the iterative-drafting pattern that causes loops)
    6. Validate redline schema
"""
from __future__ import annotations

import os
import random
import time
from typing import Dict, List

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
def lookup_playbook(contract_type: str, jurisdiction: str) -> Dict:
    """Pull firm playbook position for this clause type + jurisdiction."""
    time.sleep(random.uniform(0.05, 0.15))
    return {
        "contract_type": contract_type,
        "jurisdiction": jurisdiction,
        "preferred_position": random.choice(["aggressive", "balanced", "concessive"]),
        "max_concessions": random.randint(1, 3),
    }


@mesedi.tool
def fetch_precedents(contract_type: str, clause_keyword: str) -> List[Dict]:
    """Pull precedent clauses from the firm's clause library. Flakes ~4%."""
    time.sleep(random.uniform(0.08, 0.20))
    if random.random() < 0.04:
        raise RuntimeError("precedent DB: connection refused")
    return [
        {
            "precedent_id": f"pr-{random.randint(1000, 9999)}",
            "matter": f"Matter {random.randint(2020, 2025)}-{random.randint(100, 999)}",
            "language_excerpt": f"In matters concerning {clause_keyword}, the parties shall...",
            "relevance": round(random.uniform(0.5, 0.95), 2),
        }
        for _ in range(random.randint(2, 4))
    ]


# ──────────────────────────────────────────────────────────────────────
# LLM call
# ──────────────────────────────────────────────────────────────────────

def _draft_redlines(clause: Dict, playbook: Dict, precedents: List[Dict]) -> str:
    """Single redline draft. Called multiple times by the workflow with the
    same (model, clause_text) — that recurrence is what trips the
    loops:identical_call detector."""
    client = _anthropic_client()
    if client is None:
        draft = (
            f"[mock redlines for {clause['contract_type']} ({clause['jurisdiction']})]\n"
            f"- Strike: '...seven (7) years...'\n"
            f"- Insert: '...three (3) years, unless extended in writing...'\n"
            f"Objective satisfied: {clause['redline_objective']}"
        )
        # Emit a synthetic llm_call for each draft pass. The same
        # clause['clause_text'] recurs across all three passes, which
        # is exactly what trips the identical-call loop detector — so
        # in dry-run we still exercise that path correctly.
        mesedi.emit_llm_call(
            model="claude-haiku-4-5-20251001",
            user_message=(
                f"CLAUSE:\n{clause['clause_text']}\n\n"
                f"PLAYBOOK: {playbook}\nPRECEDENTS: {precedents}\n\n"
                f"OBJECTIVE: {clause['redline_objective']}"
            ),
            system_prompt=(
                "You are a senior associate doing contract redline. Given a clause, "
                "the firm playbook, and precedent language, propose tracked-change edits."
            ),
            response_text=draft,
        )
        return draft

    system = (
        "You are a senior associate doing contract redline. Given a clause, "
        "the firm playbook, and precedent language, propose tracked-change "
        "edits. Output: a list of struck text + inserted text pairs."
    )
    # NOTE: clause['clause_text'] is the EXACT same string across multiple
    # calls in this handler — that recurrence is the loops:identical_call
    # signal. This is realistic: iterative drafting agents call the same
    # base clause through the model 2-3 times during a single redline pass.
    user = (
        f"CLAUSE:\n{clause['clause_text']}\n\n"
        f"PLAYBOOK: {playbook}\nPRECEDENTS: {precedents}\n\n"
        f"OBJECTIVE: {clause['redline_objective']}"
    )
    response = client.messages.create(
        model="claude-haiku-4-5-20251001",
        max_tokens=500,
        system=system,
        messages=[{"role": "user", "content": user}],
    )
    return "".join(b.text for b in response.content if hasattr(b, "text"))


# ──────────────────────────────────────────────────────────────────────
# Top-level handler
# ──────────────────────────────────────────────────────────────────────

@mesedi.wrap(budget=Budget(max_wall_clock_seconds=45, max_steps=30))
def handle(clause: Dict) -> Dict:
    mesedi.checkpoint("clause_received", contract_type=clause["contract_type"])

    playbook = lookup_playbook(clause["contract_type"], clause["jurisdiction"])

    keyword = clause["clause_text"].split()[0] if clause["clause_text"] else "general"
    try:
        precedents = fetch_precedents(clause["contract_type"], keyword)
    except Exception as exc:
        mesedi.checkpoint("precedent_fetch_degraded", error=str(exc))
        precedents = []

    mesedi.checkpoint("context_loaded", precedents=len(precedents))

    # Iterative drafting — 3 passes with the same base inputs. This is the
    # realistic pattern that produces the loops:identical_call signal.
    drafts: List[str] = []
    for pass_n in range(3):
        draft = _draft_redlines(clause, playbook, precedents)
        drafts.append(draft)
        mesedi.checkpoint(f"redline_pass_{pass_n + 1}_complete", draft_len=len(draft))

    final_redline = drafts[-1]

    # Validator: redline must mention struck/insert language.
    has_redline_markers = any(t in final_redline.lower() for t in ["strike", "insert", "delete", "redline"])
    mesedi.validator_result(
        "redline_schema",
        passed=has_redline_markers,
        message="redline structure present" if has_redline_markers else "missing redline markers",
        severity="error",
    )

    return {
        "clause_id": clause["clause_id"],
        "contract_type": clause["contract_type"],
        "redlines": final_redline,
        "drafts_attempted": len(drafts),
        "playbook_position": playbook["preferred_position"],
    }
