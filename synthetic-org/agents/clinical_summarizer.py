"""
clinical_summarizer — healthcare chart-review summarizer.

Industry: Hospital / clinic chart-review automation. A clinician submits a
clinical note; the agent extracts structured fields, checks for drug
interactions, fetches relevant patient history, generates a one-paragraph
summary, and validates the summary against (a) a schema and (b) a no-PHI
guardrail.

Failure modes naturally exercised:
  - validator_failures: schema mismatch when notes contain conflicting info,
    or no-PHI check catches an MRN/SSN-shaped string in the summary
  - prompt_injection: detected on the backend when the chief_complaint field
    contains injection-style instructions
  - tool_failures: drug-interaction API flakes
  - cost_velocity: long histories drive token totals up

Workflow:
    1. Receive clinical note
    2. Extract vitals (regex-light, no LLM)
    3. Drug-interaction check (tool call to formulary API)
    4. Fetch patient history (tool call)
    5. Generate summary (LLM)
    6. Schema validation + PHI guardrail
"""
from __future__ import annotations

import os
import random
import re
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
def check_drug_interactions(meds_text: str) -> Dict:
    """Query the formulary API for interactions. Flakes ~5%."""
    time.sleep(random.uniform(0.10, 0.25))
    if random.random() < 0.05:
        raise RuntimeError("formulary API: 504 gateway timeout")
    n = len(re.findall(r"\b[a-z]+\b", meds_text.lower()))
    return {
        "interactions_found": random.randint(0, max(1, n // 4)),
        "severity_max": random.choice(["none", "minor", "moderate", "major"]),
    }


@mesedi.tool
def fetch_patient_history(mrn: str) -> List[Dict]:
    """Pull prior encounters from the EHR. Flakes ~3%."""
    time.sleep(random.uniform(0.08, 0.20))
    if random.random() < 0.03:
        raise RuntimeError(f"EHR query failed for {mrn}")
    return [
        {"encounter_date": f"2025-{random.randint(1, 12):02d}-{random.randint(1, 28):02d}",
         "department": random.choice(["ED", "primary_care", "cardiology", "ortho"]),
         "primary_dx": random.choice(["I10", "E11.9", "M54.5", "F41.1", "J45.909"])}
        for _ in range(random.randint(0, 4))
    ]


# ──────────────────────────────────────────────────────────────────────
# Local helpers
# ──────────────────────────────────────────────────────────────────────

def _extract_vitals(vitals_str: str) -> Dict:
    """Light regex extraction — not all fields will be present."""
    out: Dict = {}
    if m := re.search(r"BP\s+(\d+)/(\d+)", vitals_str):
        out["bp_systolic"] = int(m.group(1))
        out["bp_diastolic"] = int(m.group(2))
    if m := re.search(r"HR\s+(\d+)", vitals_str):
        out["hr"] = int(m.group(1))
    if m := re.search(r"SpO2\s+(\d+)", vitals_str):
        out["spo2"] = int(m.group(1))
    return out


def _looks_like_phi(text: str) -> bool:
    """Crude PHI detection — MRN, SSN, DOB patterns. Production version
    would use a model-based classifier."""
    if re.search(r"\bMRN\d{7}\b", text):
        return True
    if re.search(r"\b\d{3}-\d{2}-\d{4}\b", text):  # SSN shape
        return True
    return False


def _generate_summary(note: Dict, vitals: Dict, interactions: Dict, history: List[Dict]) -> str:
    """LLM call to produce the summary."""
    client = _anthropic_client()
    if client is None:
        # Dry-run mock — emit synthetic llm_call for detector dogfood.
        summary = (
            f"{note['patient_age']}{note['patient_sex']} presenting to {note['specialty']} "
            f"with {note['chief_complaint'][:60]}. Vitals notable for BP "
            f"{vitals.get('bp_systolic','?')}/{vitals.get('bp_diastolic','?')}. "
            f"{interactions['interactions_found']} drug interactions identified "
            f"(severity: {interactions['severity_max']}). "
            f"Patient has {len(history)} prior encounters in last 12 months."
        )
        mesedi.emit_llm_call(
            model="claude-haiku-4-5-20251001",
            user_message=f"NOTE: {note}\nVITALS: {vitals}\nINTERACTIONS: {interactions}\nHISTORY: {history}",
            system_prompt=(
                "You are a clinical documentation specialist. Produce a one-paragraph "
                "structured summary suitable for a hospital chart. Be precise."
            ),
            response_text=summary,
        )
        return summary

    system = (
        "You are a clinical documentation specialist. Produce a one-paragraph "
        "structured summary suitable for a hospital chart. Be precise. "
        "Do NOT include patient identifiers (MRN, SSN, full DOB) in the output."
    )
    user = f"NOTE: {note}\nVITALS: {vitals}\nINTERACTIONS: {interactions}\nHISTORY: {history}"
    response = client.messages.create(
        model="claude-haiku-4-5-20251001",
        max_tokens=400,
        system=system,
        messages=[{"role": "user", "content": user}],
    )
    return "".join(b.text for b in response.content if hasattr(b, "text"))


# ──────────────────────────────────────────────────────────────────────
# Top-level handler
# ──────────────────────────────────────────────────────────────────────

@mesedi.wrap(budget=Budget(max_wall_clock_seconds=45, max_steps=15))
def handle(note: Dict) -> Dict:
    """Summarize one clinical note. Returns {summary, validations, ...}."""
    mesedi.checkpoint("note_received", specialty=note["specialty"], mrn=note["patient_mrn"])

    vitals = _extract_vitals(note["vitals"])

    try:
        interactions = check_drug_interactions(note["history"])
    except Exception as exc:
        mesedi.checkpoint("drug_interaction_check_degraded", error=str(exc))
        interactions = {"interactions_found": 0, "severity_max": "unknown"}

    try:
        history = fetch_patient_history(note["patient_mrn"])
    except Exception as exc:
        mesedi.checkpoint("history_fetch_degraded", error=str(exc))
        history = []

    mesedi.checkpoint("context_gathered", history_len=len(history), interactions=interactions["interactions_found"])

    summary = _generate_summary(note, vitals, interactions, history)

    # Validator #1: schema — summary must mention specialty + complaint shape.
    schema_ok = (note["specialty"][:5].lower() in summary.lower()
                 or note["chief_complaint"][:20].lower() in summary.lower())
    mesedi.validator_result(
        "summary_schema",
        passed=schema_ok,
        message="summary references specialty/complaint" if schema_ok else "summary missing key fields",
        severity="error",
    )

    # Validator #2: no-PHI guardrail. If the summary contains MRN-shaped text,
    # this should fail — and it's a real risk because the LLM has seen the MRN.
    phi_present = _looks_like_phi(summary)
    mesedi.validator_result(
        "no_phi_in_summary",
        passed=not phi_present,
        message="PHI leaked into summary" if phi_present else "ok",
        severity="critical" if phi_present else "error",
    )

    return {
        "note_id": note["note_id"],
        "summary": summary,
        "vitals_extracted": vitals,
        "history_count": len(history),
        "validations": {"schema": schema_ok, "no_phi": not phi_present},
    }
