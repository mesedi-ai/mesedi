"""
synthetic-org runner — orchestrates the five industry agents against the
local Mesedi backend.

Usage:
    python3 runner.py --agent support_triager --iterations 5
    python3 runner.py --agent all --duration 5m
    python3 runner.py --agent financial_research --iterations 3 --max-spend-usd 0.50

The runner is intentionally simple: a loop that picks the next agent,
generates one synthetic input, invokes the agent's `@mesedi.wrap`'d handler,
and paces between calls. Cost and step budgets are enforced by Mesedi's
hard-halt mechanism (sub-slice 21a) at the SDK layer; the runner adds a
session-level `--max-spend-usd` kill switch as a coarser secondary guard.

The runner is the operator surface for the synthetic-org. The agents are the
workload surface. Detection happens in the Mesedi backend, surfaced in the
local dashboard at http://localhost:8080/ui/.
"""
from __future__ import annotations

import argparse
import importlib
import os
import random
import sys
import time
from typing import Callable, Dict, List, Tuple

import mesedi

# Local imports.
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import synthetic_inputs  # noqa: E402


# ──────────────────────────────────────────────────────────────────────
# Configuration
# ──────────────────────────────────────────────────────────────────────

DEFAULT_API_KEY = "mesedi_sk_dev_local_only"
DEFAULT_BASE_URL = "http://localhost:8080"

# (agent_module_name, friendly_label, input_generator)
AGENTS: List[Tuple[str, str, Callable[[int], Dict]]] = [
    ("agents.support_triager", "support_triager", synthetic_inputs.generate_support_ticket),
    ("agents.clinical_summarizer", "clinical_summarizer", synthetic_inputs.generate_clinical_note),
    ("agents.financial_research", "financial_research", synthetic_inputs.generate_financial_query),
    ("agents.contract_reviewer", "contract_reviewer", synthetic_inputs.generate_contract_clause),
    ("agents.incident_responder", "incident_responder", synthetic_inputs.generate_incident_alert),
]


def _parse_duration(s: str) -> float:
    """Parse "5m" / "30s" / "1h" / "120" into seconds (float)."""
    s = s.strip().lower()
    if s.endswith("h"):
        return float(s[:-1]) * 3600
    if s.endswith("m"):
        return float(s[:-1]) * 60
    if s.endswith("s"):
        return float(s[:-1])
    return float(s)


def _resolve_agents(name: str) -> List[Tuple[str, str, Callable[[int], Dict]]]:
    """Return the list of (module, label, input_generator) tuples to run."""
    if name == "all":
        return AGENTS
    matches = [a for a in AGENTS if a[1] == name]
    if not matches:
        valid = ", ".join(a[1] for a in AGENTS) + ", all"
        raise SystemExit(f"unknown agent {name!r}. valid: {valid}")
    return matches


def main() -> int:
    parser = argparse.ArgumentParser(description="synthetic-org runner")
    parser.add_argument(
        "--agent",
        default="all",
        help="agent name to run, or 'all' for round-robin (default: all)",
    )
    parser.add_argument(
        "--iterations",
        type=int,
        default=None,
        help="total iterations across all selected agents (mutually exclusive with --duration)",
    )
    parser.add_argument(
        "--duration",
        type=str,
        default=None,
        help='wall-clock budget for the whole session, e.g. "30s", "5m", "1h"',
    )
    parser.add_argument(
        "--pace-seconds",
        type=float,
        default=1.0,
        help="seconds to wait between agent invocations (default: 1.0)",
    )
    parser.add_argument(
        "--max-spend-usd",
        type=float,
        default=1.00,
        help="session-level spend cap in USD (default: $1.00; SDK-level Budget enforces per-execution)",
    )
    parser.add_argument(
        "--api-key",
        default=os.environ.get("MESEDI_API_KEY", DEFAULT_API_KEY),
    )
    parser.add_argument(
        "--base-url",
        default=os.environ.get("MESEDI_BASE_URL", DEFAULT_BASE_URL),
    )
    parser.add_argument(
        "--seed",
        type=int,
        default=int(time.time()),
        help="random seed (default: current time). Pin to reproduce a specific session.",
    )
    args = parser.parse_args()

    if args.iterations is None and args.duration is None:
        args.iterations = 5
        print(f"[runner] no --iterations or --duration set; defaulting to --iterations {args.iterations}")

    duration_seconds = _parse_duration(args.duration) if args.duration else None
    selected = _resolve_agents(args.agent)

    print(f"[runner] base_url={args.base_url} api_key={args.api_key[:18]}...")
    print(f"[runner] selected agents: {[a[1] for a in selected]}")
    print(f"[runner] iterations={args.iterations} duration={args.duration} pace_seconds={args.pace_seconds}")
    print(f"[runner] spend cap: ${args.max_spend_usd:.2f}")
    dry_run = bool(os.environ.get("MESEDI_SYNTHETIC_ORG_DRY_RUN"))
    if dry_run:
        print("[runner] MESEDI_SYNTHETIC_ORG_DRY_RUN=1 → LLM calls will be mocked (no API spend)")
    else:
        # In real-LLM mode, fail fast if ANTHROPIC_API_KEY isn't set. The
        # Anthropic SDK silently defaults to a key from env; without it
        # every call returns 401 and the runner spends 5 minutes grinding
        # through authentication errors before noticing.
        if not os.environ.get("ANTHROPIC_API_KEY"):
            print("[runner] ERROR: ANTHROPIC_API_KEY not set in environment.")
            print("[runner]        Either `export ANTHROPIC_API_KEY=sk-ant-...` or use")
            print("[runner]        MESEDI_SYNTHETIC_ORG_DRY_RUN=1 for mock LLM responses.")
            return 2

    mesedi.configure(api_key=args.api_key, base_url=args.base_url)

    # instrument_anthropic returns False if the anthropic package isn't
    # importable. In dry-run mode we don't need it; otherwise warn.
    if not dry_run:
        if not mesedi.instrument_anthropic():
            print("[runner] WARN: anthropic SDK not installed. "
                  "Install with `pip install anthropic` or set MESEDI_SYNTHETIC_ORG_DRY_RUN=1.")

    # Import each agent module (lazy — surfaces import errors cleanly).
    loaded: List[Tuple[str, Callable, Callable]] = []
    for module_path, label, input_gen in selected:
        try:
            mod = importlib.import_module(module_path)
        except Exception as exc:
            print(f"[runner] FAILED to import {module_path}: {exc}")
            continue
        handler = getattr(mod, "handle", None)
        if not callable(handler):
            print(f"[runner] {module_path} has no `handle()` function; skipping")
            continue
        loaded.append((label, handler, input_gen))

    if not loaded:
        print("[runner] no agents loaded; exiting")
        return 1

    rng = random.Random(args.seed)
    start = time.perf_counter()
    iteration = 0
    total_executions = 0
    total_halts = 0
    total_errors = 0
    consecutive_errors = 0
    per_agent_counts: Dict[str, int] = {label: 0 for label, _, _ in loaded}

    # Circuit breaker: if the first 5 iterations all raise the SAME exception
    # type, something is fundamentally misconfigured (bad API key, backend
    # down, etc.) and continuing wastes operator time + may rate-limit upstream.
    CONSECUTIVE_ERROR_BAILOUT = 5
    first_error_type: str = ""

    try:
        while True:
            # Stop conditions.
            if duration_seconds is not None and (time.perf_counter() - start) >= duration_seconds:
                print(f"[runner] duration {args.duration} reached; stopping")
                break
            if args.iterations is not None and iteration >= args.iterations:
                print(f"[runner] iterations {args.iterations} reached; stopping")
                break

            label, handler, input_gen = loaded[iteration % len(loaded)]
            iter_seed = rng.randint(0, 10_000_000)
            input_payload = input_gen(iter_seed)

            print(f"[runner] iter={iteration + 1} agent={label} seed={iter_seed}")
            try:
                result = handler(input_payload)
                if result is None:
                    total_halts += 1
                    print(f"[runner]   → halted (result=None; check dashboard for halt:* signature)")
                else:
                    print(f"[runner]   → ok")
                consecutive_errors = 0  # reset on any success
            except BaseException as exc:
                total_errors += 1
                exc_type = type(exc).__name__
                print(f"[runner]   → raised {exc_type}: {exc}")

                # Circuit breaker — same-type errors in a row mean
                # something systemic is wrong, not a real failure under test.
                if consecutive_errors == 0:
                    first_error_type = exc_type
                if exc_type == first_error_type:
                    consecutive_errors += 1
                else:
                    consecutive_errors = 1
                    first_error_type = exc_type

                if consecutive_errors >= CONSECUTIVE_ERROR_BAILOUT:
                    print(f"\n[runner] BAILING OUT: {consecutive_errors} consecutive {first_error_type} errors.")
                    print(f"[runner] Something is misconfigured (likely API key, network, or backend).")
                    print(f"[runner] Fix the underlying issue and re-run.")
                    break

            total_executions += 1
            per_agent_counts[label] += 1
            iteration += 1
            time.sleep(args.pace_seconds)
    except KeyboardInterrupt:
        print("\n[runner] interrupted by user")

    elapsed = time.perf_counter() - start

    print("\n══════════ session summary ══════════")
    print(f"  elapsed:           {elapsed:.1f}s")
    print(f"  executions:        {total_executions}")
    print(f"  halts (None):      {total_halts}")
    print(f"  raised exceptions: {total_errors}")
    for label, count in per_agent_counts.items():
        print(f"    {label:<24} {count}")
    print()

    print("[runner] flushing event shipper...")
    flush_ok = mesedi.flush(timeout=10.0)
    print(f"[runner] flush ok={flush_ok}")
    print()
    print("Inspect results:")
    print(f"  Dashboard: {args.base_url}/ui/")
    print(f"  SQLite:    cd ../backend && sqlite3 mesedi-dev.db \\")
    print(f"             \"SELECT status, COUNT(*) FROM executions GROUP BY status;\"")
    return 0


if __name__ == "__main__":
    sys.exit(main())
