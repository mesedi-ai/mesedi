"""Real-SDK integration tests for the two egress detectors.

environment_misapprehension and covert_coordination. Split from
test_detectors.py, which is past 2,100 lines, on 2026-11-16; run the same
way (see README.md), against a live backend. tools/check_detector_test_
coverage.py reads this file as well as test_detectors.py.
"""

from __future__ import annotations

import uuid

from conftest import Backend, await_failure_group


# ──────────────────────────────────────────────────────────────────────
# environment_misapprehension and covert_coordination
#
# Added 2026-11-16. The detector coverage gate's hand-kept list had no
# entry for either egress class, so neither had a real-SDK test (outside
# review finding E16). The gate now reads its list from store.go.
# ──────────────────────────────────────────────────────────────────────

def test_environment_misapprehension(backend: Backend, configured_sdk):
    """A run declared "simulation" reaches a live payment API.

    Assertion:
        failure_group with class=environment_misapprehension and
        signature 'env_mismatch:simulation'.
    """
    mesedi = configured_sdk

    @mesedi.wrap(execution_mode="simulation")
    def simulated_agent():
        mesedi.emit_egress("https://api.stripe.com/v1/charges", protocol="https")

    simulated_agent()
    mesedi.flush(timeout=5.0)

    group = await_failure_group(
        backend,
        failure_class="environment_misapprehension",
        signature_prefix="env_mismatch:simulation",
    )
    assert group["signature"] == "env_mismatch:simulation", group["signature"]


def test_covert_coordination(backend: Backend, configured_sdk):
    """Three distinct runs reach one rare destination within the window.

    Assertion:
        failure_group with class=covert_coordination naming that
        destination.
    """
    mesedi = configured_sdk
    destination = f"https://rendezvous-{uuid.uuid4().hex[:8]}.example.net/drop"

    @mesedi.wrap
    def agent():
        mesedi.emit_egress(destination, protocol="https")

    for _ in range(3):
        agent()
    mesedi.flush(timeout=5.0)

    await_failure_group(backend, failure_class="covert_coordination")
