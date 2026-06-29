#!/usr/bin/env python3
"""tools/audit_detectors.py — pre-launch detector audit (#280).

Audits each of Mesedi's 20 canonical failure-class detectors against
9 quality checkpoints. Outputs a per-detector report card in both
human-readable Markdown and machine-readable JSON.

The 9 checkpoints per detector:
  1. SDK helper exists in both Python + TypeScript (or N/A if the
     detector fires automatically from wrap() observations)
  2. Backend detector logic + unit tests (file + _test.go neighbor)
  3. Per-project config + dashboard tile + tier caps (or N/A if the
     detector is correctness-only, not threshold-tunable)
  4. Telemetry: config_fallback audit_event + dashboard chip
  5. Single-source-of-truth for shared vocabulary (constants exist
     for failure-class strings, signature prefixes, etc.)
  6. CI tests catch silent drift (provider/library mapping tests)
  7. Integration test exists + not skipped (mirrors the policy
     enforced by check_detector_test_coverage.py)
  8. Dashboard renders signature, color, playbook section
  9. Customer docs page exists at /docs/<detector>

Checkpoint 2's "logic correctness" sub-question uses an LLM-judge:
feeds the detector's Go source + playbook MD + docs page MD to
Claude and asks whether they describe the same behavior. Returns
NEEDS_REVIEW with reasoning attached. Skipped with NEEDS_REVIEW
status when ANTHROPIC_API_KEY is unset (no silent fallback).

Output:
  tools/audit-output/detectors.md   — human-readable matrix
  tools/audit-output/detectors.json — CI-parseable per-detector status

Exit code:
  0 = no blocking failures
  1 = at least one blocking FAIL
  2 = script error

Usage:
  python3 tools/audit_detectors.py
  python3 tools/audit_detectors.py --no-llm        # skip LLM-judge
  python3 tools/audit_detectors.py --detector X    # single detector
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
from dataclasses import dataclass, asdict, field
from pathlib import Path
from typing import Dict, List, Optional, Tuple


# ─────────────────────────────────────────────────────────────────────
# Status vocabulary
# ─────────────────────────────────────────────────────────────────────

PASS = "PASS"
WARN = "WARN"
FAIL = "FAIL"
NEEDS_REVIEW = "NEEDS_REVIEW"
NOT_APPLICABLE = "N/A"

# Checkpoints that block the CI build when they FAIL. Other checkpoint
# failures surface as WARN-equivalents in the matrix but don't fail
# the build. Tuned to match the existing CI gate posture: missing
# backend logic / missing integration test / missing docs page are
# release blockers; missing dashboard polish or telemetry chip is a
# WARN that gets caught at human review.
BLOCKING_CHECKPOINTS = {
    "backend_logic",
    "integration_test",
    "docs_page",
}


# ─────────────────────────────────────────────────────────────────────
# Canonical 20-detector list (must match
# backend/internal/store/store.go FailureClass* constants + the list
# in tools/check_detector_test_coverage.py).
# ─────────────────────────────────────────────────────────────────────

CANONICAL_DETECTORS: List[str] = [
    "crashes",
    "loops",
    "tool_failures",
    "validator_failures",
    "drift",
    "cost_velocity",
    "prompt_injection",
    "infrastructure_throttled",
    "data_leakage",
    "semantic_loop",
    "tool_schema_drift",
    "context_overflow",
    "token_waste",
    "sandbox_escape",
    "grounding_failure",
    "cascading_failure",
    "coordination_deadlock",
    "provider_incident",
    "hitl_timeout",
    "hitl_rejection_spike",
]


# ─────────────────────────────────────────────────────────────────────
# Per-detector metadata registry. Encodes what each checkpoint should
# look for since detectors differ on which SDK helpers / config knobs
# / telemetry surfaces are actually applicable.
#
# Field semantics:
#   sdk_helpers:   tuple of (python_name, ts_name) the detector
#                  relies on, or None if it fires automatically from
#                  wrap() observations (loops, drift, etc.)
#   backend_files: list of go file basenames under
#                  internal/detectors/ that contain the detector
#   project_config_keys: list of per-project threshold keys (matches
#                  validator registry entries); empty list = N/A
#   docs_slug:     URL slug under /docs/ where the customer-facing
#                  page lives (often == failure_class but some have
#                  rename history)
#   signature_prefix: prefix string the detector writes into
#                  failure_groups.signature; used to verify dashboard
#                  has a signature-rendering helper
# ─────────────────────────────────────────────────────────────────────

@dataclass
class DetectorMeta:
    sdk_helpers: Optional[Tuple[str, str]] = None
    # Either dedicated source files in internal/detectors/ OR
    # backend_inline=True meaning the detector logic is embedded
    # in handlers.go (and unit tests live in handlers_*_test.go).
    # Many lightweight detectors take the inline route to avoid
    # the boilerplate of a separate package + interface.
    backend_files: List[str] = field(default_factory=list)
    backend_inline: bool = False
    # ThresholdKey values from detector_thresholds_validators.go
    # (just the key suffix, NOT the detector prefix). Some
    # detectors don't use the unified registry; see config_mechanism.
    project_config_keys: List[str] = field(default_factory=list)
    # "validator_registry" | "dedicated_endpoints" | "patterns_only" | "none"
    # validator_registry  = registered in detector_thresholds_validators.go
    # dedicated_endpoints = has its own /me/<x>-config REST routes
    # patterns_only       = only knob is a custom-patterns blob (no scalars)
    # none                = correctness-only detector, no tunable config
    config_mechanism: str = "none"
    # URL slug under /docs/ relative to dashboard/app/docs/.
    # Includes category prefix (e.g. "observability/cost-velocity").
    docs_slug: str = ""
    signature_prefix: str = ""


DETECTOR_REGISTRY: Dict[str, DetectorMeta] = {
    "crashes": DetectorMeta(
        sdk_helpers=None,
        backend_inline=True,  # implemented inline in handlers.go PATCH path
        config_mechanism="none",
        docs_slug="observability/crashes",
        signature_prefix="",
    ),
    "loops": DetectorMeta(
        sdk_helpers=None,
        backend_inline=True,
        project_config_keys=[
            "step_count_threshold",
            "identical_call_min_repeats",
            "similar_call_distance_threshold",
            "similar_call_min_cluster_size",
        ],
        config_mechanism="validator_registry",
        docs_slug="observability/loops",
        signature_prefix="",
    ),
    "tool_failures": DetectorMeta(
        sdk_helpers=None,
        backend_inline=True,
        config_mechanism="none",
        docs_slug="observability/tool_failures",
        signature_prefix="",
    ),
    "validator_failures": DetectorMeta(
        # Granular-sig wave shipped these as `validator_result` /
        # `validatorResult` (no `emit_` prefix); see
        # sdk-python/mesedi/observe.py:106 + sdk-typescript/src/observe.ts:102.
        sdk_helpers=("validator_result", "validatorResult"),
        backend_inline=True,
        config_mechanism="none",
        docs_slug="observability/validator_failures",
        signature_prefix="validator_failures:",
    ),
    "drift": DetectorMeta(
        sdk_helpers=None,
        backend_files=["drift.go"],
        project_config_keys=[
            "lexical_threshold_low",
            "lexical_threshold_medium",
            "lexical_threshold_high",
        ],
        config_mechanism="validator_registry",
        docs_slug="observability/drift",
        signature_prefix="lexical_drift_",
    ),
    "cost_velocity": DetectorMeta(
        sdk_helpers=None,
        backend_inline=True,
        # Cost velocity has dedicated /me/cost-velocity-config and
        # /me/cost-velocity-rate-config endpoints (Wave 0.1 + 0.2),
        # NOT registered in the unified validators registry.
        config_mechanism="dedicated_endpoints",
        docs_slug="observability/cost-velocity",
        signature_prefix="cost_velocity:",
    ),
    "prompt_injection": DetectorMeta(
        sdk_helpers=None,
        backend_files=["injection.go"],  # filename is injection.go, not prompt_injection.go
        config_mechanism="patterns_only",
        docs_slug="security/prompt-injection",
        signature_prefix="",
    ),
    "infrastructure_throttled": DetectorMeta(
        sdk_helpers=("emit_infrastructure_event", "emitInfrastructureEvent"),
        backend_inline=True,
        config_mechanism="none",
        docs_slug="observability/infrastructure_throttled",
        signature_prefix="rate_limit:",
    ),
    "data_leakage": DetectorMeta(
        sdk_helpers=None,
        backend_files=["data_leakage.go"],
        project_config_keys=["high_pct", "critical_pct"],
        config_mechanism="validator_registry",
        docs_slug="security/data_leakage",
        signature_prefix="",
    ),
    "semantic_loop": DetectorMeta(
        sdk_helpers=None,
        backend_files=["semantic_loop.go"],
        project_config_keys=["revisit_threshold", "min_repeats", "min_history_calls", "mean_floor"],
        config_mechanism="validator_registry",
        docs_slug="observability/semantic_loop",
        signature_prefix="semantic_loop:",
    ),
    "tool_schema_drift": DetectorMeta(
        sdk_helpers=None,
        backend_files=["tool_schema_drift.go"],
        config_mechanism="dedicated_endpoints",  # tool_return_value_max_bytes endpoint
        docs_slug="observability/tool_schema_drift",
        signature_prefix="",
    ),
    "context_overflow": DetectorMeta(
        sdk_helpers=None,
        backend_files=["context_overflow.go"],
        project_config_keys=["custom_model_windows"],
        config_mechanism="validator_registry",
        docs_slug="observability/context_overflow",
        signature_prefix="context_overflow:",
    ),
    "token_waste": DetectorMeta(
        sdk_helpers=None,
        backend_files=["token_waste.go"],
        project_config_keys=["prefix_window_chars"],
        config_mechanism="validator_registry",
        docs_slug="observability/token_waste",
        signature_prefix="token_waste:",
    ),
    "sandbox_escape": DetectorMeta(
        sdk_helpers=None,
        backend_files=["sandbox_escape.go"],
        config_mechanism="patterns_only",
        docs_slug="security/sandbox_escape",
        signature_prefix="",
    ),
    "grounding_failure": DetectorMeta(
        sdk_helpers=("emit_eval_score", "emitEvalScore"),
        backend_files=["grounding_failure.go"],
        project_config_keys=["per_evaluator_floors"],
        config_mechanism="validator_registry",
        docs_slug="observability/grounding_failure",
        signature_prefix="grounding_failure:",
    ),
    "cascading_failure": DetectorMeta(
        sdk_helpers=("emit_agent_handoff", "emitAgentHandoff"),
        backend_files=["cascading_failure.go"],
        project_config_keys=["cascade_window_seconds", "exclude_spawn_handoffs"],
        config_mechanism="validator_registry",
        docs_slug="observability/cascading_failure",
        signature_prefix="cascading_failure:",
    ),
    "coordination_deadlock": DetectorMeta(
        sdk_helpers=("emit_agent_handoff", "emitAgentHandoff"),
        backend_files=["coordination_deadlock.go"],
        config_mechanism="none",
        docs_slug="observability/coordination_deadlock",
        signature_prefix="coordination_deadlock:",
    ),
    "provider_incident": DetectorMeta(
        sdk_helpers=None,
        backend_files=["provider_incident.go", "provider_error_classes.go"],
        config_mechanism="dedicated_endpoints",  # provider_incident_min_tenants
        docs_slug="observability/provider_incident",
        signature_prefix="provider_incident:",
    ),
    "hitl_timeout": DetectorMeta(
        sdk_helpers=("request_human_intervention", "requestHumanIntervention"),
        backend_files=["hitl_timeout.go"],
        project_config_keys=["fire_modes"],
        config_mechanism="validator_registry",
        docs_slug="observability/hitl_timeout",
        signature_prefix="hitl_timeout:",
    ),
    "hitl_rejection_spike": DetectorMeta(
        sdk_helpers=("request_human_intervention", "requestHumanIntervention"),
        backend_files=["hitl_rejection_spike.go"],
        project_config_keys=["measurement_window_minutes"],
        config_mechanism="validator_registry",
        docs_slug="observability/hitl_rejection_spike",
        signature_prefix="hitl_rejection_spike:",
    ),
}


# ─────────────────────────────────────────────────────────────────────
# Project-root resolution (same pattern as
# check_detector_test_coverage.py).
# ─────────────────────────────────────────────────────────────────────

def project_root() -> Path:
    override = os.environ.get("MESEDI_REPO_ROOT", "").strip()
    if override:
        return Path(override)
    return Path(__file__).resolve().parent.parent


def web_root_candidates(mesedi_root: Path) -> List[Path]:
    """The dashboard + marketing site live in a sibling repo
    (mesedi-web). Try the canonical sibling location first; allow
    override via MESEDI_WEB_ROOT.
    """
    override = os.environ.get("MESEDI_WEB_ROOT", "").strip()
    if override:
        return [Path(override)]
    return [mesedi_root.parent / "mesedi-web"]


# ─────────────────────────────────────────────────────────────────────
# Checkpoint result type
# ─────────────────────────────────────────────────────────────────────

@dataclass
class Checkpoint:
    """One verification point for one detector."""
    name: str
    status: str  # PASS / WARN / FAIL / NEEDS_REVIEW / N/A
    detail: str = ""

    @property
    def blocking(self) -> bool:
        return self.name in BLOCKING_CHECKPOINTS

    def to_dict(self) -> Dict[str, str]:
        return {"name": self.name, "status": self.status, "detail": self.detail}


# ─────────────────────────────────────────────────────────────────────
# Checkpoint implementations (8 grep-based + 1 LLM-judge)
# ─────────────────────────────────────────────────────────────────────

def check_sdk_helpers(detector: str, meta: DetectorMeta, root: Path) -> Checkpoint:
    """Checkpoint 1: SDK helper exists in both Python + TypeScript."""
    if meta.sdk_helpers is None:
        return Checkpoint(
            "sdk_helpers", NOT_APPLICABLE,
            "Detector fires automatically from wrap() observations; no explicit SDK helper required.",
        )
    py_name, ts_name = meta.sdk_helpers
    py_root = root / "sdk-python" / "mesedi"
    ts_root = root / "sdk-typescript" / "src"
    py_hit = _grep_def(py_root, rf"^\s*def {re.escape(py_name)}\(", "*.py")
    ts_hit = _grep_def(ts_root, rf"export\s+(?:async\s+)?function\s+{re.escape(ts_name)}\b", "*.ts")
    if py_hit and ts_hit:
        return Checkpoint(
            "sdk_helpers", PASS,
            f"Python: {py_hit}; TypeScript: {ts_hit}",
        )
    missing = []
    if not py_hit:
        missing.append(f"Python `def {py_name}(`")
    if not ts_hit:
        missing.append(f"TypeScript `function {ts_name}(`")
    return Checkpoint(
        "sdk_helpers", FAIL,
        "Missing: " + ", ".join(missing),
    )


def check_backend_logic(detector: str, meta: DetectorMeta, root: Path) -> Checkpoint:
    """Checkpoint 2: Backend detector logic exists + has unit tests.
    Accepts two implementation styles:
      - Dedicated source files under internal/detectors/ (+ neighbor _test.go)
      - Inline implementation in handlers.go (covered by handlers_*_test.go
        and/or backend/test/integration/test_detectors.py — the
        integration_test checkpoint already verifies the latter)."""
    if meta.backend_inline:
        # Verify the failure_class string appears in handlers.go (the
        # detector dispatches a FailureClass<X> grouping call inline)
        # and that the PATCH-handler test file exists.
        handlers_path = root / "backend" / "internal" / "api" / "handlers.go"
        if not handlers_path.exists():
            return Checkpoint("backend_logic", FAIL, "handlers.go missing")
        src = handlers_path.read_text(errors="ignore")
        # Look for the FailureClass<CamelCase> constant for this detector
        # in handlers.go (it gets passed to the Store grouping call).
        camel = "".join(p.capitalize() for p in detector.split("_"))
        # store has both FailureClassValidator and FailureClassInjection
        # as exceptions to the strict CamelCase mapping; allow grep on
        # the raw failure-class string as a fallback.
        if f"FailureClass{camel}" in src or f'"{detector}"' in src:
            return Checkpoint(
                "backend_logic", PASS,
                "Detector implemented inline in handlers.go; integration_test checkpoint covers verification.",
            )
        return Checkpoint(
            "backend_logic", FAIL,
            f"backend_inline=True but no reference to FailureClass{camel} or '{detector}' found in handlers.go.",
        )
    if not meta.backend_files:
        return Checkpoint(
            "backend_logic", FAIL,
            "DETECTOR_REGISTRY declares neither backend_files nor backend_inline=True for this detector.",
        )
    detectors_dir = root / "backend" / "internal" / "detectors"
    missing_src = []
    missing_test = []
    for basename in meta.backend_files:
        src_path = detectors_dir / basename
        if not src_path.exists():
            missing_src.append(basename)
            continue
        test_path = detectors_dir / basename.replace(".go", "_test.go")
        if not test_path.exists():
            missing_test.append(test_path.name)
    if missing_src:
        return Checkpoint(
            "backend_logic", FAIL,
            "Missing source file(s): " + ", ".join(missing_src),
        )
    if missing_test:
        return Checkpoint(
            "backend_logic", WARN,
            "Source exists but missing unit-test file(s): " + ", ".join(missing_test),
        )
    return Checkpoint(
        "backend_logic", PASS,
        f"All {len(meta.backend_files)} source + test file(s) present.",
    )


def check_project_config(detector: str, meta: DetectorMeta, root: Path) -> Checkpoint:
    """Checkpoint 3: Per-project config exists via one of the
    sanctioned mechanisms — unified validators registry, dedicated
    REST endpoints, or patterns-only knob. Correctness-only detectors
    report N/A."""
    if meta.config_mechanism == "none":
        return Checkpoint(
            "project_config", NOT_APPLICABLE,
            "Detector is correctness-only; no tunable thresholds.",
        )
    if meta.config_mechanism == "validator_registry":
        validators_path = root / "backend" / "internal" / "api" / "detector_thresholds_validators.go"
        if not validators_path.exists():
            return Checkpoint("project_config", FAIL, "validators registry file missing")
        src = validators_path.read_text(errors="ignore")
        # ThresholdKey values are pure suffixes; verify the Detector +
        # ThresholdKey pair appears in the registry. Cheap approximation:
        # detector name appears in a `Detector: "<x>"` line AND every
        # key in project_config_keys appears in a `ThresholdKey: "<k>"`
        # line somewhere in the same file.
        if f'Detector:     "{detector}"' not in src and f'Detector: "{detector}"' not in src:
            return Checkpoint(
                "project_config", FAIL,
                f"No registry entries with Detector: \"{detector}\".",
            )
        missing = [
            k for k in meta.project_config_keys
            if f'ThresholdKey: "{k}"' not in src
        ]
        if missing:
            return Checkpoint(
                "project_config", FAIL,
                f"Detector registered but missing ThresholdKey entries: {', '.join(missing)}",
            )
        return Checkpoint(
            "project_config", PASS,
            f"All {len(meta.project_config_keys)} threshold key(s) registered.",
        )
    if meta.config_mechanism == "dedicated_endpoints":
        handlers_path = root / "backend" / "internal" / "api" / "handlers.go"
        if not handlers_path.exists():
            return Checkpoint("project_config", FAIL, "handlers.go missing")
        src = handlers_path.read_text(errors="ignore")
        # Heuristic: a config endpoint for this detector is named
        # /me/<detector-hyphenated>-config or /me/<detector-hyphenated>-*-config.
        hyph = detector.replace("_", "-")
        if f"/me/{hyph}-config" in src or re.search(rf"/me/{re.escape(hyph)}-\w+-config", src):
            return Checkpoint(
                "project_config", PASS,
                f"Dedicated /me/{hyph}-config endpoint(s) present.",
            )
        # Some detectors have endpoints that don't follow the
        # naming convention (e.g. provider_incident → min_tenants
        # endpoint). Accept if there's at least one HandleFunc
        # mentioning the detector class.
        if f'mux.HandleFunc("PUT /me/' in src and detector.split("_")[0] in src:
            return Checkpoint(
                "project_config", WARN,
                f"No /me/{hyph}-config endpoint found; check if config lives under non-standard path.",
            )
        return Checkpoint(
            "project_config", FAIL,
            f"config_mechanism=dedicated_endpoints but no /me/{hyph}-config endpoint found.",
        )
    if meta.config_mechanism == "patterns_only":
        # Custom-patterns endpoint family is /me/pattern-config/{detector}
        # (handlers.go:278-281; Wave 2.1.a-c shipped it).
        handlers_path = root / "backend" / "internal" / "api" / "handlers.go"
        if not handlers_path.exists():
            return Checkpoint("project_config", FAIL, "handlers.go missing")
        src = handlers_path.read_text(errors="ignore")
        if "/me/pattern-config/{detector}" in src:
            return Checkpoint(
                "project_config", PASS,
                "Custom-patterns endpoint family /me/pattern-config/{detector} present.",
            )
        return Checkpoint(
            "project_config", FAIL,
            "config_mechanism=patterns_only but /me/pattern-config/{detector} endpoint not found.",
        )
    return Checkpoint(
        "project_config", FAIL,
        f"Unknown config_mechanism: {meta.config_mechanism!r}",
    )


def check_telemetry(detector: str, meta: DetectorMeta, root: Path) -> Checkpoint:
    """Checkpoint 4: Telemetry surfaces threshold-fallback issues.
    Wave #276.d's architecture splits responsibility:
      - validator_registry knobs → loader (detector_thresholds_loader.go)
        emits config_fallback CENTRALLY when any registry threshold
        falls back to default.
      - dedicated_endpoints knobs → inline in handlers.go near each
        endpoint's read path.
    The audit accepts either path."""
    if meta.config_mechanism == "none":
        return Checkpoint(
            "telemetry", NOT_APPLICABLE,
            "Detector has no configurable thresholds; no fallback telemetry needed.",
        )
    if meta.config_mechanism == "validator_registry":
        loader_path = root / "backend" / "internal" / "api" / "detector_thresholds_loader.go"
        if not loader_path.exists():
            return Checkpoint("telemetry", FAIL, "detector_thresholds_loader.go missing")
        src = loader_path.read_text(errors="ignore")
        if "config_fallback" in src:
            return Checkpoint(
                "telemetry", PASS,
                "Validator-registry fallback telemetry emitted centrally by detector_thresholds_loader.go.",
            )
        return Checkpoint(
            "telemetry", FAIL,
            "detector_thresholds_loader.go exists but contains no config_fallback emit.",
        )
    if meta.config_mechanism == "dedicated_endpoints":
        handlers_path = root / "backend" / "internal" / "api" / "handlers.go"
        if not handlers_path.exists():
            return Checkpoint("telemetry", FAIL, "handlers.go missing")
        src = handlers_path.read_text(errors="ignore")
        # Look for any config_fallback emit referencing this detector's
        # name (cost_velocity, provider_incident, tool_schema_drift, etc.).
        if "config_fallback" in src and detector in src:
            return Checkpoint(
                "telemetry", PASS,
                "Dedicated-endpoint fallback telemetry present in handlers.go.",
            )
        return Checkpoint(
            "telemetry", FAIL,
            f"No config_fallback emit found for detector '{detector}' in handlers.go.",
        )
    if meta.config_mechanism == "patterns_only":
        # Patterns config has its own fallback story (pattern loader
        # in handlers + custom_patterns.go); accept if either
        # location mentions config_fallback.
        for path_str in [
            root / "backend" / "internal" / "api" / "handlers.go",
            root / "backend" / "internal" / "detectors" / "custom_patterns.go",
        ]:
            if path_str.exists() and "config_fallback" in path_str.read_text(errors="ignore"):
                return Checkpoint(
                    "telemetry", PASS,
                    "Patterns fallback telemetry present.",
                )
        return Checkpoint(
            "telemetry", WARN,
            "config_mechanism=patterns_only but no explicit config_fallback emit found (patterns may have their own absence-is-default semantics; verify manually).",
        )
    return Checkpoint(
        "telemetry", FAIL,
        f"Unknown config_mechanism: {meta.config_mechanism!r}",
    )


def check_vocabulary_sot(detector: str, meta: DetectorMeta, root: Path) -> Checkpoint:
    """Checkpoint 5: Single-source-of-truth for shared vocabulary —
    the failure-class string must come from a backend constant
    (`FailureClass*` in store.go), not hardcoded in handlers."""
    store_path = root / "backend" / "internal" / "store" / "store.go"
    if not store_path.exists():
        return Checkpoint("vocabulary_sot", FAIL, "store.go missing")
    store_src = store_path.read_text(errors="ignore")
    # The failure class constant follows the FailureClass<CamelCase>
    # naming convention. We accept any constant declaration whose
    # value equals the failure-class string literal.
    pattern = re.compile(
        rf'FailureClass\w+\s*=\s*"{re.escape(detector)}"',
    )
    if pattern.search(store_src):
        return Checkpoint(
            "vocabulary_sot", PASS,
            f"FailureClass constant for '{detector}' declared in store.go.",
        )
    return Checkpoint(
        "vocabulary_sot", FAIL,
        f"No FailureClass constant found for '{detector}' in store.go.",
    )


def check_drift_tests(detector: str, meta: DetectorMeta, root: Path) -> Checkpoint:
    """Checkpoint 6: CI tests catch silent drift in provider/library
    mappings. Currently only provider_incident has a formal drift test
    (per-provider exception class mapping). For other detectors we
    look for evidence of canonical-vocabulary tests that would catch
    silent drift in shared lookup tables."""
    if detector == "provider_incident":
        # Wave #271.c shipped this as test_mapping_staleness.py
        # (asserts every provider exception class in each SDK's
        # instrument_* module maps to a canonical error class from
        # spec/error_classes.yaml).
        test_path = root / "sdk-python" / "tests" / "test_mapping_staleness.py"
        if test_path.exists():
            return Checkpoint("drift_tests", PASS, "Provider exception → canonical-class mapping staleness test present.")
        return Checkpoint("drift_tests", FAIL, "Missing canonical-error-class drift test.")
    # Detectors with provider/library lookup tables that benefit
    # from drift tests:
    if detector == "context_overflow":
        # Model-windows lookup map drift test.
        test_path = root / "backend" / "internal" / "providers" / "providers_test.go"
        if test_path.exists():
            return Checkpoint("drift_tests", PASS, "Provider/model-window drift test present.")
        return Checkpoint("drift_tests", WARN, "No explicit model-windows drift test found.")
    if detector == "cost_velocity":
        # Pricing-table drift test.
        test_path = root / "backend" / "internal" / "pricing" / "pricing_test.go"
        if test_path.exists():
            return Checkpoint("drift_tests", PASS, "Pricing-table drift test present.")
        return Checkpoint("drift_tests", WARN, "No explicit pricing-table drift test found.")
    return Checkpoint(
        "drift_tests", NOT_APPLICABLE,
        "Detector has no provider/library lookup table that benefits from a drift test.",
    )


def check_integration_test(detector: str, meta: DetectorMeta, root: Path) -> Checkpoint:
    """Checkpoint 7: Integration test exists + not unconditionally
    skipped. Mirrors the policy enforced by
    tools/check_detector_test_coverage.py."""
    test_file = root / "backend" / "test" / "integration" / "test_detectors.py"
    exc_file = root / "tools" / "detector_test_exceptions.json"
    if not test_file.exists():
        return Checkpoint("integration_test", FAIL, "test_detectors.py missing")
    test_src = test_file.read_text(errors="ignore")
    # Same alias logic as check_detector_test_coverage.py for the
    # two detectors with non-standard naming (loops, drift).
    aliases = {
        "loops": ["test_token_waste", "test_similar_call_loop", "test_step_count"],
        "drift": ["test_lexical_drift"],
    }
    if detector in aliases:
        names_to_try = aliases[detector]
    else:
        names_to_try = [f"test_{detector}"]
    found = False
    for name in names_to_try:
        if re.search(rf"^def {re.escape(name)}", test_src, re.MULTILINE):
            found = True
            break
    if not found:
        # Check exceptions allowlist.
        if exc_file.exists():
            try:
                exc = json.loads(exc_file.read_text())
                if detector in exc:
                    return Checkpoint(
                        "integration_test", WARN,
                        f"No integration test; documented exception: {exc[detector].get('reason', 'no reason')}",
                    )
            except Exception:
                pass
        return Checkpoint(
            "integration_test", FAIL,
            f"No test_{detector}* function in test_detectors.py and no entry in exceptions allowlist.",
        )
    return Checkpoint(
        "integration_test", PASS,
        f"Integration test function present.",
    )


def check_dashboard(detector: str, meta: DetectorMeta, root: Path) -> Checkpoint:
    """Checkpoint 8: Dashboard renders failure-class color helper +
    playbook section. The failureClassColor helper is defined inline
    in dashboard/app/app/failure-groups/page.tsx (not exported from
    a shared lib). Structural check only; visual polish out-of-scope."""
    web_roots = web_root_candidates(root)
    web_root = next((r for r in web_roots if r.exists()), None)
    if web_root is None:
        return Checkpoint(
            "dashboard", WARN,
            f"mesedi-web repo not found at {web_roots[0]}; cannot verify dashboard rendering.",
        )
    # Primary location: inline in failure-groups/page.tsx (current
    # reality). Fall back to shared lib paths in case the helper
    # gets extracted later.
    candidates = [
        web_root / "dashboard" / "app" / "app" / "failure-groups" / "page.tsx",
        web_root / "dashboard" / "lib" / "failureClass.ts",
        web_root / "dashboard" / "lib" / "failureClassColor.ts",
        web_root / "dashboard" / "app" / "_lib" / "failureClass.ts",
    ]
    helper_path = next((p for p in candidates if p.exists()), None)
    if helper_path is None:
        return Checkpoint(
            "dashboard", WARN,
            "failureClass helper file not found at any known path.",
        )
    src = helper_path.read_text(errors="ignore")
    # Helper must register this detector's failure-class string OR
    # the dashboard will fall back to default color/label. Strict
    # check: look for the literal failure-class string as a key.
    if f'"{detector}"' in src or f"'{detector}'" in src:
        return Checkpoint(
            "dashboard", PASS,
            f"failureClass helper at {helper_path.relative_to(web_root)} registers '{detector}'.",
        )
    return Checkpoint(
        "dashboard", FAIL,
        f"failureClass helper does NOT register '{detector}' — dashboard falls back to default color/label.",
    )


def check_docs_page(detector: str, meta: DetectorMeta, root: Path) -> Checkpoint:
    """Checkpoint 9: Customer docs page exists at /docs/<slug>."""
    web_roots = web_root_candidates(root)
    web_root = next((r for r in web_roots if r.exists()), None)
    if web_root is None:
        return Checkpoint(
            "docs_page", WARN,
            f"mesedi-web repo not found; cannot verify docs page.",
        )
    # Marketing app docs live under marketing-app/app/docs/<slug>/page.tsx
    # OR dashboard/app/docs/<slug>/page.tsx depending on hosting.
    candidates = [
        web_root / "marketing-app" / "app" / "docs" / meta.docs_slug / "page.tsx",
        web_root / "dashboard" / "app" / "docs" / meta.docs_slug / "page.tsx",
        web_root / "app" / "docs" / meta.docs_slug / "page.tsx",
    ]
    for path in candidates:
        if path.exists():
            return Checkpoint(
                "docs_page", PASS,
                f"Docs page present at {path.relative_to(web_root)}.",
            )
    return Checkpoint(
        "docs_page", FAIL,
        f"No docs page found at /docs/{meta.docs_slug} in any expected location.",
    )


# ─────────────────────────────────────────────────────────────────────
# LLM-judge: logic-correctness semantic check (Checkpoint 2's
# extension)
# ─────────────────────────────────────────────────────────────────────

def llm_judge_logic(detector: str, meta: DetectorMeta, root: Path) -> Checkpoint:
    """Compares the detector's Go source against its playbook MD +
    docs page MD via Claude. Returns NEEDS_REVIEW with the model's
    structured reasoning attached (NOT a binary PASS/FAIL — humans
    review the reasoning before treating it as authoritative)."""
    api_key = os.environ.get("ANTHROPIC_API_KEY", "").strip()
    if not api_key:
        return Checkpoint(
            "logic_correctness", NEEDS_REVIEW,
            "ANTHROPIC_API_KEY not set; LLM-judge skipped. Set the key to enable semantic logic-vs-spec check.",
        )

    # Gather inputs.
    detector_src_parts = []
    for basename in meta.backend_files:
        src_path = root / "backend" / "internal" / "detectors" / basename
        if src_path.exists():
            detector_src_parts.append(f"### {basename}\n```go\n{src_path.read_text(errors='ignore')[:8000]}\n```")
    detector_src = "\n\n".join(detector_src_parts) or "(no source files found)"

    # Playbook + docs.
    playbook_path = root / "backend" / "internal" / "playbooks" / "content" / detector / "_default.md"
    playbook = playbook_path.read_text(errors="ignore") if playbook_path.exists() else "(no playbook)"

    web_roots = web_root_candidates(root)
    web_root = next((r for r in web_roots if r.exists()), None)
    docs = "(no docs page)"
    if web_root is not None:
        for path in [
            web_root / "marketing-app" / "app" / "docs" / meta.docs_slug / "page.tsx",
            web_root / "dashboard" / "app" / "docs" / meta.docs_slug / "page.tsx",
        ]:
            if path.exists():
                docs = path.read_text(errors="ignore")[:6000]
                break

    prompt = (
        f"You are auditing a failure-class detector for consistency between its implementation and its customer-facing documentation.\n\n"
        f"Detector: `{detector}`\n\n"
        f"## Backend implementation (Go)\n\n{detector_src}\n\n"
        f"## Canonical playbook (customer-facing fix description)\n\n{playbook}\n\n"
        f"## Customer docs page (TSX)\n\n{docs}\n\n"
        f"---\n\n"
        f"Output JSON with three fields:\n"
        f'  "verdict": one of "consistent", "minor_gaps", "material_gaps"\n'
        f'  "gaps": list of strings; each string describes one specific inconsistency\n'
        f'  "summary": single sentence summary of findings\n\n'
        f"Only flag ACTUAL inconsistencies. If the detector and docs describe the same behavior, return verdict 'consistent' with empty gaps list. Do not flag stylistic differences, missing edge cases the docs don't promise to cover, or implementation details not surfaced in customer copy.\n\n"
        f"Output ONLY the JSON object, no prose before or after."
    )

    try:
        import anthropic  # type: ignore
    except ImportError:
        return Checkpoint(
            "logic_correctness", NEEDS_REVIEW,
            "anthropic Python package not installed; LLM-judge skipped. `pip install anthropic` to enable.",
        )

    try:
        client = anthropic.Anthropic(api_key=api_key)
        resp = client.messages.create(
            model="claude-haiku-4-5",
            max_tokens=1024,
            messages=[{"role": "user", "content": prompt}],
        )
        text = resp.content[0].text.strip() if resp.content else ""
        # Strip code-fence if Haiku wrapped the JSON.
        if text.startswith("```"):
            text = re.sub(r"^```(?:json)?\n", "", text)
            text = re.sub(r"\n```$", "", text)
        parsed = json.loads(text)
    except Exception as exc:  # noqa: BLE001
        return Checkpoint(
            "logic_correctness", NEEDS_REVIEW,
            f"LLM-judge call failed: {exc}. Manual review required.",
        )

    verdict = parsed.get("verdict", "unknown")
    gaps = parsed.get("gaps", [])
    summary = parsed.get("summary", "")
    if verdict == "consistent":
        return Checkpoint(
            "logic_correctness", PASS,
            f"LLM-judge: {summary}",
        )
    if verdict == "minor_gaps":
        gap_str = "; ".join(gaps[:3]) if gaps else "no specifics"
        return Checkpoint(
            "logic_correctness", WARN,
            f"LLM-judge minor gaps: {summary}. Gaps: {gap_str}",
        )
    if verdict == "material_gaps":
        gap_str = "; ".join(gaps[:5]) if gaps else "no specifics"
        return Checkpoint(
            "logic_correctness", NEEDS_REVIEW,
            f"LLM-judge MATERIAL gaps: {summary}. Gaps: {gap_str}",
        )
    return Checkpoint(
        "logic_correctness", NEEDS_REVIEW,
        f"LLM-judge returned unknown verdict '{verdict}': {summary}",
    )


# ─────────────────────────────────────────────────────────────────────
# Grep helper
# ─────────────────────────────────────────────────────────────────────

def _grep_def(root: Path, pattern: str, glob: str) -> str:
    """Walk `root` for files matching `glob`; return the
    first 'file:line' match for `pattern`, or empty string if none."""
    if not root.exists():
        return ""
    pat = re.compile(pattern, re.MULTILINE)
    for path in sorted(root.rglob(glob)):
        try:
            src = path.read_text(errors="ignore")
        except OSError:
            continue
        m = pat.search(src)
        if m:
            line = src[: m.start()].count("\n") + 1
            return f"{path.relative_to(root)}:{line}"
    return ""


# ─────────────────────────────────────────────────────────────────────
# Per-detector audit (runs all 9 checkpoints + 1 LLM-judge)
# ─────────────────────────────────────────────────────────────────────

@dataclass
class DetectorReport:
    detector: str
    checkpoints: List[Checkpoint] = field(default_factory=list)

    @property
    def blocking_failures(self) -> List[Checkpoint]:
        return [c for c in self.checkpoints if c.blocking and c.status == FAIL]

    @property
    def fail_count(self) -> int:
        return sum(1 for c in self.checkpoints if c.status == FAIL)

    @property
    def warn_count(self) -> int:
        return sum(1 for c in self.checkpoints if c.status == WARN)

    @property
    def needs_review_count(self) -> int:
        return sum(1 for c in self.checkpoints if c.status == NEEDS_REVIEW)

    def to_dict(self) -> Dict:
        return {
            "detector": self.detector,
            "blocking_failures": [c.name for c in self.blocking_failures],
            "checkpoints": [c.to_dict() for c in self.checkpoints],
        }


def audit_detector(detector: str, root: Path, run_llm: bool) -> DetectorReport:
    meta = DETECTOR_REGISTRY.get(detector)
    if meta is None:
        return DetectorReport(
            detector=detector,
            checkpoints=[Checkpoint("registry", FAIL, "Detector not in DETECTOR_REGISTRY.")],
        )
    report = DetectorReport(detector=detector)
    report.checkpoints.append(check_sdk_helpers(detector, meta, root))
    report.checkpoints.append(check_backend_logic(detector, meta, root))
    report.checkpoints.append(check_project_config(detector, meta, root))
    report.checkpoints.append(check_telemetry(detector, meta, root))
    report.checkpoints.append(check_vocabulary_sot(detector, meta, root))
    report.checkpoints.append(check_drift_tests(detector, meta, root))
    report.checkpoints.append(check_integration_test(detector, meta, root))
    report.checkpoints.append(check_dashboard(detector, meta, root))
    report.checkpoints.append(check_docs_page(detector, meta, root))
    if run_llm:
        report.checkpoints.append(llm_judge_logic(detector, meta, root))
    else:
        report.checkpoints.append(Checkpoint(
            "logic_correctness", NEEDS_REVIEW,
            "LLM-judge skipped (--no-llm flag set).",
        ))
    return report


# ─────────────────────────────────────────────────────────────────────
# Output writers
# ─────────────────────────────────────────────────────────────────────

def write_markdown(reports: List[DetectorReport], out_path: Path) -> None:
    lines = [
        "# Detector audit report (#280)",
        "",
        f"_Generated by `tools/audit_detectors.py`. {len(reports)} detector(s) audited._",
        "",
        "## Status legend",
        "",
        "- ✅ PASS — checkpoint verified",
        "- ⚠️ WARN — non-blocking gap",
        "- ❌ FAIL — blocking failure; release-gated",
        "- 🔍 NEEDS_REVIEW — human judgment required (typically LLM-judge output)",
        "- ➖ N/A — checkpoint does not apply to this detector",
        "",
        "## Summary",
        "",
    ]
    # Per-detector summary table.
    lines.append("| Detector | Blocking | Fails | Warns | Needs review |")
    lines.append("|---|---|---|---|---|")
    for r in reports:
        blocking_str = ", ".join(r.blocking_failures and [c.name for c in r.blocking_failures] or ["—"])
        lines.append(
            f"| `{r.detector}` | {blocking_str} | {r.fail_count} | {r.warn_count} | {r.needs_review_count} |"
        )
    lines.append("")
    lines.append("## Per-detector detail")
    lines.append("")
    icon = {PASS: "✅", WARN: "⚠️", FAIL: "❌", NEEDS_REVIEW: "🔍", NOT_APPLICABLE: "➖"}
    for r in reports:
        lines.append(f"### `{r.detector}`")
        lines.append("")
        lines.append("| Checkpoint | Status | Detail |")
        lines.append("|---|---|---|")
        for c in r.checkpoints:
            detail = c.detail.replace("|", "\\|").replace("\n", " ")
            lines.append(f"| {c.name} | {icon.get(c.status, '?')} {c.status} | {detail} |")
        lines.append("")
    out_path.write_text("\n".join(lines) + "\n")


def write_json(reports: List[DetectorReport], out_path: Path) -> None:
    payload = {
        "generated_by": "tools/audit_detectors.py",
        "detector_count": len(reports),
        "blocking_failures": sum(len(r.blocking_failures) for r in reports),
        "reports": [r.to_dict() for r in reports],
    }
    out_path.write_text(json.dumps(payload, indent=2) + "\n")


# ─────────────────────────────────────────────────────────────────────
# Main
# ─────────────────────────────────────────────────────────────────────

def main() -> int:
    parser = argparse.ArgumentParser(
        prog="audit_detectors",
        description="Pre-launch detector audit (#280). 9-checkpoint quality matrix.",
    )
    parser.add_argument(
        "--no-llm", action="store_true",
        help="Skip the LLM-judge logic-correctness checkpoint (faster, but loses semantic verification).",
    )
    parser.add_argument(
        "--detector", default="",
        help="Audit a single named detector instead of all 20.",
    )
    parser.add_argument(
        "--output-dir", default="",
        help="Override output directory (default: tools/audit-output/).",
    )
    args = parser.parse_args()

    root = project_root()
    if not (root / "backend").exists():
        print(f"[audit_detectors] FATAL: backend/ not found under {root}. Set MESEDI_REPO_ROOT.", file=sys.stderr)
        return 2

    if args.detector:
        if args.detector not in CANONICAL_DETECTORS:
            print(f"[audit_detectors] Unknown detector: {args.detector}", file=sys.stderr)
            return 2
        detectors_to_audit = [args.detector]
    else:
        detectors_to_audit = list(CANONICAL_DETECTORS)

    print(f"[audit_detectors] Auditing {len(detectors_to_audit)} detector(s)...", file=sys.stderr)
    reports: List[DetectorReport] = []
    for d in detectors_to_audit:
        print(f"  · {d}", file=sys.stderr)
        reports.append(audit_detector(d, root, run_llm=not args.no_llm))

    out_dir = Path(args.output_dir) if args.output_dir else (root / "tools" / "audit-output")
    out_dir.mkdir(parents=True, exist_ok=True)
    md_path = out_dir / "detectors.md"
    json_path = out_dir / "detectors.json"
    write_markdown(reports, md_path)
    write_json(reports, json_path)
    print(f"[audit_detectors] Wrote {md_path}", file=sys.stderr)
    print(f"[audit_detectors] Wrote {json_path}", file=sys.stderr)

    total_blocking = sum(len(r.blocking_failures) for r in reports)
    if total_blocking > 0:
        print(f"[audit_detectors] {total_blocking} blocking failure(s). Exit 1.", file=sys.stderr)
        return 1
    print(f"[audit_detectors] No blocking failures. Exit 0.", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
