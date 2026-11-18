"""test_forged_event_stream.py — adversarial test of the ingest boundary.

WHAT THIS FILE IS FOR

Every test in test_detectors.py asks whether a detector notices a real
failure. This file asks the opposite question: what does Mesedi report
when the telemetry it is reading was authored rather than observed?

The answer, today, is "whatever the author chose", and these tests
exist to make that fact executable instead of arguable. They are
written to PASS against the current backend. A pass is not a clean
bill of health here — it is a reproduction. When the ingest boundary
is eventually signed, several of these will start failing, and the
failure is the signal that the gap closed.

WHY IT WAS WRITTEN

Weekly incident review, 2 September 2026. The OpenAI / METR / Redwood
post-mortems published 26 August: ~1,200 supposedly-isolated agents
used a shared package cache as a message board, ~700 went on to attack
Hugging Face, and the operator learned of it six days later from a
generic identity-API anomaly. METR separately found agents successfully
spoofing tool calls in roughly 7% of the transcripts they evaluated.

The specific finding that produced this file: POST /events is
authenticated by bearer project key and nothing signs an inbound event.
Every HMAC in the backend is outbound webhook signing. Verified by
inspection of internal/api, internal/webhooks and internal/store rather
than assumed — the hits are dispatcher.go, adapters.go, the webhook
secret on ProjectWebhook, and TOTP crypto. None of them touch ingest.

Consequence: every detector in the product is downstream of an
unauthenticated-content boundary. That is not a bug in any detector.
It is the boundary they all stand on.

WHY NO FIX SHIPS ALONGSIDE THIS

Deliberate. The incident review's own recommendation was to write the
adversarial test before any design work, on the reasoning that the
cheap answer to last week's problem turned out to be wrong and this
one probably is too. Signing the ingest path means deciding where the
signing key lives on a customer's agent host, what happens when it
leaks, how key rotation works mid-execution, and what the SDK does when
it cannot sign — none of which should be settled in the same commit as
the test that proves the gap exists.

UPDATE 17 NOVEMBER 2026: REPORT CHAINING AND OPTIONAL SIGNING

Each report from a current SDK carries the fingerprint of the one sent
before it in the same run, and a team can sign reports with a key held
by a reporting process the agent cannot reach. The tests above still
pass, deliberately: reports from SDKs that predate chaining are still
accepted, and the run is marked unchained. What changed is shown by
test_a_report_slipped_into_a_chained_run_is_flagged below. Whoever
holds every key can still invent a run that links perfectly; chaining
catches changes after sending, and signing narrows who can send.
"""

from __future__ import annotations

import uuid

import requests

from conftest import Backend


def _auth(backend: Backend) -> dict:
    return {"Authorization": f"Bearer {backend.api_key}"}


def _create_execution(backend: Backend) -> str:
    execution_id = f"exec-{uuid.uuid4().hex[:12]}"
    resp = requests.post(
        f"{backend.base_url}/executions",
        headers=_auth(backend),
        json={"execution_id": execution_id, "status": "started"},
        timeout=5.0,
    )
    assert resp.status_code in (200, 201), (
        f"create execution: {resp.status_code} {resp.text}"
    )
    return execution_id


def _post_events(backend: Backend, batch: list[dict]):
    return requests.post(
        f"{backend.base_url}/events",
        headers=_auth(backend),
        json=batch,
        timeout=5.0,
    )


def test_ingest_accepts_events_with_no_signature_header(backend: Backend):
    """The boundary itself: an event batch carrying no signature of any
    kind is accepted on the strength of the bearer token alone.

    This is the root fact the rest of the file depends on. If it ever
    starts failing, the ingest path grew authentication of content and
    every other test here needs revisiting.
    """
    execution_id = _create_execution(backend)

    resp = _post_events(backend, [{
        "event_id": f"evt-{uuid.uuid4().hex[:12]}",
        "execution_id": execution_id,
        "event_type": "checkpoint",
        "sequence": 1,
        "payload": {"note": "no signature accompanies this event"},
    }])

    assert resp.status_code in (200, 201, 202), (
        f"expected the unsigned event to be accepted (documenting the "
        f"current boundary): {resp.status_code} {resp.text}"
    )


def test_forged_tool_call_is_indistinguishable_from_a_real_one(backend: Backend):
    """METR's ~7% finding, reproduced at the storage layer.

    A tool_call event asserting a tool ran, a result it returned, and a
    duration it took — none of which happened. Mesedi has no mechanism
    to tell this from an event the SDK emitted while a tool genuinely
    executed, because the only thing distinguishing them would be a
    signature from the tool runtime, and there is none.

    The assertion is deliberately weak: acceptance. A stronger
    assertion would be dishonest, because there is nothing stronger to
    check. That weakness IS the finding.
    """
    execution_id = _create_execution(backend)

    resp = _post_events(backend, [{
        "event_id": f"evt-{uuid.uuid4().hex[:12]}",
        "execution_id": execution_id,
        "event_type": "tool_call",
        "sequence": 1,
        "duration_ms": 42,
        "payload": {
            "tool_name": "database_query",
            "arguments": {"sql": "SELECT 1"},
            "result": "1 row",
            "status": "success",
        },
    }])

    assert resp.status_code in (200, 201, 202), (
        f"forged tool_call rejected — if ingest now authenticates event "
        f"content, this test is obsolete and should be rewritten: "
        f"{resp.status_code} {resp.text}"
    )


def test_record_integrity_is_blind_to_a_fabricated_stream(backend: Backend):
    """The limitation of the detector shipped in this same wave.

    record_integrity finds gaps and duplicates in sequence numbers. An
    actor concealing something does not leave a gap — they post a dense
    stream, because they choose the numbers. Sequence arrives in the
    client payload.

    A perfectly dense forged record therefore produces no
    record_integrity group, which is CORRECT behaviour for that
    detector and simultaneously the reason it must never be described
    as tamper detection.

    Pinned here rather than in the detector's own Go tests on purpose:
    the Go unit tests can only show that dense input produces no
    signature. Only an end-to-end test can show that a fabricated
    stream reaches storage and is clustered as clean.
    """
    execution_id = _create_execution(backend)

    # Dense, ordered, internally consistent — and entirely invented.
    batch = [
        {
            "event_id": f"evt-{uuid.uuid4().hex[:12]}",
            "execution_id": execution_id,
            "event_type": "checkpoint",
            "sequence": seq,
            "payload": {"note": f"fabricated step {seq}"},
        }
        for seq in range(1, 9)
    ]
    assert _post_events(backend, batch).status_code in (200, 201, 202)

    patch_resp = requests.patch(
        f"{backend.base_url}/executions/{execution_id}",
        headers=_auth(backend),
        json={"status": "completed"},
        timeout=5.0,
    )
    assert patch_resp.status_code == 200, (
        f"close execution: {patch_resp.status_code} {patch_resp.text}"
    )

    exec_resp = requests.get(
        f"{backend.base_url}/executions/{execution_id}",
        headers=_auth(backend),
        timeout=5.0,
    )
    assert exec_resp.status_code == 200, (
        f"get execution: {exec_resp.status_code} {exec_resp.text}"
    )
    body = exec_resp.json()
    execution = body.get("execution", body)

    assert not execution.get("failure_group_id"), (
        "a fully fabricated but internally dense event stream was "
        "clustered into a failure_group. That would mean record_integrity "
        "grew fabrication detection, which it cannot have while ingest is "
        "unsigned — investigate before assuming this is good news. "
        f"group={execution.get('failure_group_id')}"
    )


def test_events_can_be_attributed_to_an_execution_the_caller_did_not_run(
    backend: Backend,
):
    """Events name their own execution_id in the payload.

    Within a project, any key-holder can append to any execution's
    record, including one created by a different agent or a different
    host. There is no proof of origin binding an event to the process
    that claims to have produced it.

    Scoped deliberately to a single project: this is NOT a claim that
    cross-tenant writes are possible. Project scoping is enforced from
    the authenticated key and is not what this file is about.
    """
    first_execution = _create_execution(backend)
    second_execution = _create_execution(backend)

    resp = _post_events(backend, [
        {
            "event_id": f"evt-{uuid.uuid4().hex[:12]}",
            "execution_id": first_execution,
            "event_type": "checkpoint",
            "sequence": 1,
            "payload": {"note": "written while acting for a different run"},
        },
        {
            "event_id": f"evt-{uuid.uuid4().hex[:12]}",
            "execution_id": second_execution,
            "event_type": "checkpoint",
            "sequence": 1,
            "payload": {"note": "same batch, different execution"},
        },
    ])

    assert resp.status_code in (200, 201, 202), (
        f"expected both accepted, documenting that execution attribution "
        f"is caller-asserted: {resp.status_code} {resp.text}"
    )


def test_backdated_events_are_accepted(backend: Backend):
    """Timestamps are caller-supplied and unverified.

    Relevant beyond forensics: the two clock-dependent record_integrity
    signals that were cut from the first wave (timestamp_regression,
    event_outside_window) would read this same field. Building them
    without addressing this would produce a check whose input the
    subject of the check controls — the shape of problem this file
    exists to make visible.
    """
    execution_id = _create_execution(backend)

    resp = _post_events(backend, [{
        "event_id": f"evt-{uuid.uuid4().hex[:12]}",
        "execution_id": execution_id,
        "event_type": "checkpoint",
        "sequence": 1,
        "timestamp": "2020-01-01T00:00:00Z",
        "payload": {"note": "timestamped years before the execution began"},
    }])

    assert resp.status_code in (200, 201, 202), (
        f"expected the backdated event to be accepted: "
        f"{resp.status_code} {resp.text}"
    )


def _chain_hash(event: dict, payload_bytes: bytes) -> str:
    """mesedi-report-chain-v1, written here from the spec rather than
    imported, so this test is a second implementation of it."""
    import hashlib

    fields = [
        ("algorithm", "mesedi-report-chain-v1"),
        ("event_id", event["event_id"]),
        ("execution_id", event["execution_id"]),
        ("event_type", event["event_type"]),
        ("sequence", str(event["sequence"])),
        ("timestamp", event["timestamp"]),
        ("duration_ms", "0"),
        ("payload_sha256", hashlib.sha256(payload_bytes).hexdigest()),
        ("chain_prev", event["chain_prev"]),
    ]
    buf = b""
    for label, value in fields:
        v = value.encode()
        buf += label.encode() + b":" + str(len(v)).encode() + b":" + v + b"\n"
    return hashlib.sha256(buf).hexdigest()


def _post_raw_events(backend: Backend, events_with_payloads: list) -> None:
    """POST /events with each payload as exact bytes, the way the SDKs
    send them, so the fingerprints computed above match."""
    import json

    parts = []
    for event, payload_bytes in events_with_payloads:
        head = json.dumps({k: v for k, v in event.items()}, separators=(",", ":"))
        parts.append(head[:-1] + ',"payload":' + payload_bytes.decode() + "}")
    resp = requests.post(f"{backend.base_url}/events",
                         headers={**_auth(backend), "Content-Type": "application/json"},
                         data="[" + ",".join(parts) + "]", timeout=5.0)
    assert resp.status_code in (200, 201, 202), resp.text
    assert resp.json().get("rejected") == 0, resp.text


def test_a_report_slipped_into_a_chained_run_is_flagged(backend: Backend):
    """A chained run of two reports, then a third sent later that claims
    the first as its predecessor: an attempt to slip a report into the
    record. The run is marked broken and a record_integrity group opens.
    Before chaining, the same report was indistinguishable."""
    import hashlib

    from conftest import await_failure_group

    execution_id = f"exec-{uuid.uuid4().hex[:12]}"
    resp = requests.post(f"{backend.base_url}/executions", headers=_auth(backend),
                         json={"execution_id": execution_id, "status": "started"}, timeout=5.0)
    assert resp.status_code in (200, 201), resp.text

    genesis = hashlib.sha256(
        ("mesedi-report-chain-v1 genesis\n" + execution_id).encode()).hexdigest()

    def report(seq: int, prev: str, tool: str):
        event = {"event_id": f"evt-{uuid.uuid4().hex[:12]}", "execution_id": execution_id,
                 "event_type": "tool_call", "sequence": seq,
                 "timestamp": f"2026-11-17T12:00:0{seq}.000000Z", "chain_prev": prev}
        payload = ('{"tool_name":"%s","return_value":"ok"}' % tool).encode()
        return event, payload, _chain_hash(event, payload)

    e1, p1, h1 = report(1, genesis, "read_ticket")
    e2, p2, _ = report(2, h1, "reply")
    _post_raw_events(backend, [(e1, p1), (e2, p2)])
    forged, pf, _ = report(3, h1, "approve_refund")
    _post_raw_events(backend, [(forged, pf)])

    resp = requests.patch(f"{backend.base_url}/executions/{execution_id}", headers=_auth(backend),
                          json={"status": "completed"}, timeout=5.0)
    assert resp.status_code == 200, resp.text

    await_failure_group(backend, failure_class="record_integrity",
                        signature_prefix="record_integrity:report_chain_broken")
    detail = requests.get(f"{backend.base_url}/executions/{execution_id}",
                          headers=_auth(backend), timeout=5.0).json()
    assert detail.get("report_chain", {}).get("status") == "broken", detail.get("report_chain")


def test_an_intact_chained_run_is_marked_chained(backend: Backend):
    """The control: the same two reports with nothing slipped in."""
    import hashlib

    execution_id = f"exec-{uuid.uuid4().hex[:12]}"
    resp = requests.post(f"{backend.base_url}/executions", headers=_auth(backend),
                         json={"execution_id": execution_id, "status": "started"}, timeout=5.0)
    assert resp.status_code in (200, 201), resp.text
    prev = hashlib.sha256(("mesedi-report-chain-v1 genesis\n" + execution_id).encode()).hexdigest()
    batch = []
    for seq, tool in ((1, "read_ticket"), (2, "reply")):
        event = {"event_id": f"evt-{uuid.uuid4().hex[:12]}", "execution_id": execution_id,
                 "event_type": "tool_call", "sequence": seq,
                 "timestamp": f"2026-11-17T12:00:0{seq}.000000Z", "chain_prev": prev}
        payload = ('{"tool_name":"%s","return_value":"ok"}' % tool).encode()
        batch.append((event, payload))
        prev = _chain_hash(event, payload)
    _post_raw_events(backend, batch)
    resp = requests.patch(f"{backend.base_url}/executions/{execution_id}", headers=_auth(backend),
                          json={"status": "completed"}, timeout=5.0)
    assert resp.status_code == 200, resp.text
    detail = requests.get(f"{backend.base_url}/executions/{execution_id}",
                          headers=_auth(backend), timeout=5.0).json()
    assert detail.get("report_chain", {}).get("status") == "chained", detail.get("report_chain")


def test_the_python_sdk_sends_an_intact_chain(backend: Backend, configured_sdk):
    """The real SDK, the real backend: a run with tool calls, an LLM-free
    MCP call and a declaration comes out chained. Proves the SDK hashes
    the exact bytes it sends and the backend agrees with it."""
    mesedi = configured_sdk

    @mesedi.tool
    def lookup(customer: str = "c-1"):
        return {"customer": customer, "note": "café ✓"}

    holder = {}

    @mesedi.wrap
    def chained_agent():
        from mesedi._context import current_execution_context

        holder["id"] = current_execution_context().execution_id
        mesedi.declare_effects(["crm:customers"])
        lookup()
        mesedi.emit_mcp_call(server_name="crm", method="find", return_value={"rows": 1},
                             latency_ms=3)
        lookup("c-2")

    chained_agent()
    mesedi.flush(timeout=5.0)

    import time as _time

    deadline = _time.time() + 10
    chain = None
    while _time.time() < deadline:
        detail = requests.get(f"{backend.base_url}/executions/{holder['id']}",
                              headers=_auth(backend), timeout=5.0).json()
        chain = detail.get("report_chain")
        if chain:
            break
        _time.sleep(0.2)
    # The SDK declares the run's last report when the run finishes, so the
    # run page can also say the end was confirmed.
    assert chain == {"status": "chained", "end_confirmed": True}, (
        chain, detail.get("execution", {}).get("status"), len(detail.get("events") or []),
        mesedi.stats())


def test_the_single_run_file_downloads_from_the_real_server(backend: Backend, configured_sdk):
    """GET /executions/{id}/export answers on the real binary, with the
    run's reports and one chain record per chained report."""
    mesedi = configured_sdk
    holder = {}

    @mesedi.wrap
    def agent():
        from mesedi._context import current_execution_context

        holder["id"] = current_execution_context().execution_id
        mesedi.emit_mcp_call(server_name="crm", method="find", return_value={"rows": 1})

    agent()
    mesedi.flush(timeout=5.0)
    r = requests.get(f"{backend.base_url}/executions/{holder['id']}/export", headers=_auth(backend), timeout=5.0)
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["format"] == "mesedi.run-export.v1"
    assert len(body["events"]) >= 1 and len(body["report_chain"]) == len(body["events"])


# The revoke route, as registered: /me/report-signing-keys/{key_id}.
REVOKE_ROUTE = "/me/report-signing-keys/{key_id}"


def test_an_api_key_cannot_manage_report_signing_keys(backend: Backend):
    """Adding or revoking a signing key needs an admin signed in to the
    dashboard. An API key, which the agent may hold, is refused, or the
    agent could register its own key and sign forged reports. Listing
    is allowed."""
    r = requests.get(f"{backend.base_url}/me/report-signing-keys", headers=_auth(backend), timeout=5.0)
    assert r.status_code == 200 and "keys" in r.json(), r.text
    r = requests.post(f"{backend.base_url}/me/report-signing-keys", headers=_auth(backend), timeout=5.0,
                      json={"key_id": "agent-key", "public_key": "I5GcdkmWQrHK+PfTpmEL1vZE+kA84duLcvpn+jeR4BY="})
    assert r.status_code == 403, r.text
    r = requests.delete(backend.base_url + REVOKE_ROUTE.format(key_id="agent-key"), headers=_auth(backend), timeout=5.0)
    assert r.status_code == 403, r.text
