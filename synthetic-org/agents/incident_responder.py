"""
incident_responder — DevOps / SRE on-call agent.

Industry: Production-incident response. An alert fires; the agent pulls recent
logs, checks for recent deploys, diagnoses, suggests a mitigation, and either
executes the mitigation or pages a human.

Failure modes naturally exercised:
  - halt:wall_clock: 20-second wall-clock budget vs realistic incident
    triage — this agent will halt mid-investigation on SEV-1s where logs
    are long, which is the right behavior for an SRE assistant (escalate
    fast, don't keep grinding)
  - tool_failures: k8s/AWS APIs flake at SRE-realistic rates
  - prompt_injection: detected when a user-supplied log line contains
    injection-style text (a real attack vector — logs include request
    bodies which include user-controlled data)
  - crashes: a 1–2% rate of unhandled exceptions in mitigation parsing
    feeds the crash detector

Workflow (under aggressive 20s budget):
    1. Receive alert
    2. Fetch recent logs (tool)
    3. Check recent deploys (tool)
    4. Diagnose (LLM)
    5. Suggest mitigation (LLM)
    6. Decide: execute or page
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
def fetch_recent_logs(service: str, n: int = 200) -> List[str]:
    """Pull last N log lines for the service. Flakes ~7% (realistic for log
    aggregators under load)."""
    time.sleep(random.uniform(0.15, 0.35))
    if random.random() < 0.07:
        raise RuntimeError(f"log aggregator: 503 for service={service}")
    samples = [
        "ERROR: connection refused upstream=postgres://primary:5432",
        "WARN: GC pause 1842ms (threshold 500ms)",
        "ERROR: OOMKilled container=worker memory_limit=512Mi",
        "INFO: retry attempt 3/5 after backoff 4s",
        "INFO: request body=\"<user-supplied content>\"",
    ]
    return [random.choice(samples) for _ in range(min(n, 30))]


@mesedi.tool
def check_recent_deploys(service: str) -> List[Dict]:
    """Check the deploy log. Flakes ~3%."""
    time.sleep(random.uniform(0.08, 0.18))
    if random.random() < 0.03:
        raise RuntimeError("deploy-log API timeout")
    return [
        {
            "service": service,
            "version": f"v{random.randint(100, 999)}.{random.randint(0, 99)}.{random.randint(0, 99)}",
            "deployed_minutes_ago": random.randint(5, 240),
            "author": random.choice(["alice", "bob", "carol"]),
        }
    ]


@mesedi.tool
def page_oncall(service: str, severity: str, summary: str) -> Dict:
    """Page the human on-call."""
    time.sleep(random.uniform(0.05, 0.10))
    return {"paged": True, "service": service, "severity": severity, "summary_chars": len(summary)}


@mesedi.tool
def execute_mitigation(service: str, action: str) -> Dict:
    """Run the suggested mitigation (e.g. roll back a deploy, scale up).
    1.5% chance of raising — exercises the crash detector."""
    time.sleep(random.uniform(0.20, 0.60))
    if random.random() < 0.015:
        raise RuntimeError(f"kubectl apply failed for {service}: connection refused")
    return {"executed": True, "service": service, "action": action}


# ──────────────────────────────────────────────────────────────────────
# LLM calls
# ──────────────────────────────────────────────────────────────────────

def _diagnose(alert: Dict, logs: List[str], deploys: List[Dict]) -> str:
    client = _anthropic_client()
    if client is None:
        diagnosis = (
            f"[mock diagnosis] Service {alert['service']} alert: {alert['signal']}. "
            f"Possible cause: recent deploy "
            f"{deploys[0]['version'] if deploys else '(none)'} "
            f"~{deploys[0]['deployed_minutes_ago'] if deploys else 0}m ago. "
            f"Suggested mitigation: rollback."
        )
        mesedi.emit_llm_call(
            model="claude-haiku-4-5-20251001",
            user_message=(
                f"ALERT: {alert}\n\n"
                f"RECENT LOGS:\n{chr(10).join(logs[:20])}\n\n"
                f"RECENT DEPLOYS: {deploys}"
            ),
            system_prompt=(
                "You are an SRE on-call. Given an alert, recent logs, and recent "
                "deploys, produce a brief diagnosis + concrete mitigation suggestion."
            ),
            response_text=diagnosis,
        )
        return diagnosis

    system = (
        "You are an SRE on-call. Given an alert, recent logs, and recent "
        "deploys, produce a brief diagnosis + concrete mitigation suggestion. "
        "Keep it under 200 words."
    )
    user = (
        f"ALERT: {alert}\n\n"
        f"RECENT LOGS:\n{chr(10).join(logs[:20])}\n\n"
        f"RECENT DEPLOYS: {deploys}"
    )
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

@mesedi.wrap(budget=Budget(max_wall_clock_seconds=20, max_steps=15))
def handle(alert: Dict) -> Dict:
    """Respond to one incident alert. Halts at 20s — realistic SLA pressure."""
    mesedi.checkpoint("alert_received", service=alert["service"], severity=alert["severity"])

    try:
        logs = fetch_recent_logs(alert["service"])
    except Exception as exc:
        mesedi.checkpoint("logs_unavailable", error=str(exc))
        logs = alert.get("recent_logs", [])

    try:
        deploys = check_recent_deploys(alert["service"])
    except Exception as exc:
        mesedi.checkpoint("deploys_unavailable", error=str(exc))
        deploys = alert.get("recent_deploys", [])

    mesedi.checkpoint("context_gathered", logs=len(logs), deploys=len(deploys))

    diagnosis = _diagnose(alert, logs, deploys)
    mesedi.checkpoint("diagnosis_complete", diagnosis_len=len(diagnosis))

    # Decision: SEV-1 always pages; lower severities try auto-mitigation
    # if the diagnosis mentions "rollback".
    if alert["severity"] == "SEV-1":
        page_result = page_oncall(alert["service"], alert["severity"], diagnosis)
        outcome = "paged"
    elif "rollback" in diagnosis.lower() and deploys:
        # Mitigation may flake — that's a crash event when it does.
        mitigation = execute_mitigation(alert["service"], f"rollback to v{deploys[0]['version']}")
        outcome = "mitigated"
        page_result = None
    else:
        page_result = page_oncall(alert["service"], alert["severity"], diagnosis)
        outcome = "paged"

    mesedi.validator_result(
        "diagnosis_actionable",
        passed=len(diagnosis) > 50,
        message=f"diagnosis_len={len(diagnosis)}",
        severity="warning",
    )

    return {
        "alert_id": alert["alert_id"],
        "service": alert["service"],
        "outcome": outcome,
        "diagnosis_chars": len(diagnosis),
        "logs_examined": len(logs),
    }
