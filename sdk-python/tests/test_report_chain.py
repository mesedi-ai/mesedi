"""Report chaining and signing (mesedi-report-chain-v1), checked
against spec/report_chain_vectors.json, which the backend and the
TypeScript SDK check too."""

from __future__ import annotations

import base64
import json
from pathlib import Path
from typing import Any, List

import httpx
import pytest

from mesedi import _chain
from mesedi._shipper import EventShipper
from mesedi.events import Event

VECTORS = json.loads(
    (Path(__file__).resolve().parents[2] / "spec" / "report_chain_vectors.json").read_text("utf-8")
)


def test_shared_vectors() -> None:
    assert _chain.genesis(VECTORS["execution_id"]) == VECTORS["genesis"]
    prev = VECTORS["genesis"]
    for v in VECTORS["events"]:
        payload = v["payload"].encode("utf-8")
        got = _chain.fingerprint(
            event_id=v["event_id"], execution_id=VECTORS["execution_id"], event_type=v["event_type"],
            sequence=v["sequence"], timestamp=v["timestamp"], duration_ms=v["duration_ms"],
            payload=payload, chain_prev=prev,
        )
        assert got == v["fingerprint"], v["event_id"]
        prev = got


def test_the_bytes_hashed_are_the_bytes_the_shipper_sends() -> None:
    """Each vector payload, decoded and re-encoded the way the shipper
    encodes a request body, gives back the exact bytes."""
    for v in VECTORS["events"]:
        assert _chain.payload_bytes(json.loads(v["payload"])) == v["payload"].encode("utf-8")


def test_canonical_timestamp() -> None:
    assert _chain.canonical_timestamp("2026-11-17T14:00:01.5+02:00") == "2026-11-17T12:00:01.500000Z"
    assert _chain.canonical_timestamp("2026-11-17T12:00:00.123456Z") == "2026-11-17T12:00:00.123456Z"
    assert _chain.canonical_timestamp("2026-11-17T12:00:00Z") == "2026-11-17T12:00:00.000000Z"
    assert _chain.canonical_timestamp("2026-11-17T12:00:00.1234567Z") == "2026-11-17T12:00:00.123456Z"


def test_shared_signature_vector() -> None:
    crypto = pytest.importorskip("cryptography")
    assert crypto
    s = VECTORS["signature"]
    signer = _chain.ed25519_signer("test", bytes.fromhex(s["private_key_seed_hex"]))
    key_id, sig = signer(bytes.fromhex(VECTORS["events"][0]["fingerprint"]))
    assert key_id == "test"
    assert base64.b64encode(sig).decode() == s["signature_b64"]


def _event(exec_id: str, n: int) -> Event:
    return Event(event_id=f"{exec_id}-e{n}", execution_id=exec_id, event_type="tool_call",
                 sequence=n, payload={"tool_name": "t", "n": n})


def test_chainer_links_each_run_separately() -> None:
    c = _chain.ReportChainer()
    a1, b1, a2 = _event("a", 1), _event("b", 1), _event("a", 2)
    for e in (a1, b1, a2):
        c.chain(e)
    assert a1.chain_prev == _chain.genesis("a")
    assert b1.chain_prev == _chain.genesis("b")
    fp_a1 = _chain.fingerprint(event_id=a1.event_id, execution_id="a", event_type="tool_call", sequence=1,
                               timestamp=a1.timestamp, duration_ms=None,
                               payload=_chain.payload_bytes(a1.payload), chain_prev=a1.chain_prev)
    assert a2.chain_prev == fp_a1
    assert a1.signature is None


def test_a_failing_signer_leaves_the_report_chained_but_unsigned() -> None:
    def broken(_: bytes) -> Any:
        raise RuntimeError("signing service down")

    e = _event("x", 1)
    _chain.ReportChainer(broken).chain(e)
    assert e.chain_prev == _chain.genesis("x") and e.signature is None


def test_the_shipper_sends_chained_and_signed_reports() -> None:
    bodies: List[Any] = []

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/events":
            bodies.append(json.loads(request.content))
        return httpx.Response(200, json={"ok": True})

    http = httpx.Client(base_url="http://test", transport=httpx.MockTransport(handler))
    signed: List[bytes] = []

    def signer(fp: bytes) -> Any:
        signed.append(fp)
        return "k1", b"\x01" * 64

    shipper = EventShipper(http=http, flush_interval_ms=10, batch_size=2, report_signer=signer)
    try:
        for n in (1, 2, 3):
            shipper.submit_event(_event("run", n))
        assert shipper.flush(timeout=5.0)
    finally:
        shipper.shutdown()
    sent = [e for body in bodies for e in body]
    assert [e["event_id"] for e in sent] == ["run-e1", "run-e2", "run-e3"]
    assert sent[0]["chain_prev"] == _chain.genesis("run")
    for i in (1, 2):
        assert sent[i]["chain_prev"] == signed[i - 1].hex()
    assert all(e["signature"] == {"key_id": "k1", "sig": base64.b64encode(b"\x01" * 64).decode()} for e in sent)


def test_a_rate_limited_batch_is_retried_not_dropped() -> None:
    """A 429 is temporary: the shipper waits as asked (capped) and retries.
    Until 0.10.0 it was dropped as permanent, losing the batch and
    breaking the run's chain. Other 4xx answers are still permanent."""
    answers = [429, 200]
    seen: List[int] = []

    def handler(request: httpx.Request) -> httpx.Response:
        code = answers.pop(0) if answers else 200
        seen.append(code)
        return httpx.Response(code, headers={"Retry-After": "0"}, json={"ok": code == 200})

    shipper = EventShipper(http=httpx.Client(base_url="http://test", transport=httpx.MockTransport(handler)),
                           flush_interval_ms=10)
    try:
        shipper.submit_event(_event("rl", 1))
        assert shipper.flush(timeout=5.0)
    finally:
        shipper.shutdown()
    stats = shipper.stats()
    assert seen == [429, 200] and stats.delivered == 1 and stats.rejected == 0

    shipper = EventShipper(http=httpx.Client(base_url="http://test",
                                             transport=httpx.MockTransport(lambda r: httpx.Response(404))),
                           flush_interval_ms=10)
    try:
        shipper.submit_event(_event("nf", 1))
        shipper.flush(timeout=5.0)
    finally:
        shipper.shutdown()
    assert shipper.stats().rejected == 1


def test_shared_end_vector() -> None:
    pytest.importorskip("cryptography")
    e = VECTORS["end"]
    msg = _chain.end_message(VECTORS["execution_id"], e["last_fingerprint"])
    assert msg.hex() == e["end_message_hex"]
    signer = _chain.ed25519_signer("t", bytes.fromhex(VECTORS["signature"]["private_key_seed_hex"]))
    assert base64.b64encode(signer(msg)[1]).decode() == e["signature_b64"]


def test_the_run_finished_message_declares_the_end() -> None:
    """The PATCH that ends a run carries the last fingerprint sent, signed
    over the end message when signing, so the backend can tell when the
    run's last reports went missing."""
    from mesedi.events import Execution

    patches: List[Any] = []
    events_sent: List[Any] = []

    def handler(request: httpx.Request) -> httpx.Response:
        body = json.loads(request.content)
        if request.method == "PATCH":
            patches.append(body)
        elif request.url.path == "/events":
            events_sent.extend(body)
        return httpx.Response(200, json={"ok": True})

    signed: List[bytes] = []

    def signer(m: bytes) -> Any:
        signed.append(m)
        return "k1", b"\x02" * 64

    shipper = EventShipper(http=httpx.Client(base_url="http://test", transport=httpx.MockTransport(handler)),
                           flush_interval_ms=10, report_signer=signer)
    try:
        shipper.submit_event(_event("end-run", 1))
        shipper.submit_event(_event("end-run", 2))
        shipper.submit_execution_end(Execution(execution_id="end-run", status="completed",
                                               started_at="2026-11-17T12:00:00.000000Z"))
        shipper.submit_execution_end(Execution(execution_id="never-chained", status="completed",
                                               started_at="2026-11-17T12:00:00.000000Z"))
        assert shipper.flush(timeout=5.0)
    finally:
        shipper.shutdown()
    last = signed[1].hex()  # the second report's fingerprint
    end = patches[0]["report_chain_end"]
    assert end["hash"] == last
    assert signed[2] == _chain.end_message("end-run", last)
    assert end["signature"] == {"key_id": "k1", "sig": base64.b64encode(b"\x02" * 64).decode()}
    assert "report_chain_end" not in patches[1]


def test_retry_after_seconds() -> None:
    from mesedi._shipper import _retry_after_seconds

    assert _retry_after_seconds(httpx.Response(429, headers={"Retry-After": "3"})) == 3.0
    assert _retry_after_seconds(httpx.Response(429)) == 0.0
    assert _retry_after_seconds(httpx.Response(429, headers={"Retry-After": "soon"})) == 0.0
    assert _retry_after_seconds(httpx.Response(429, headers={"Retry-After": "-5"})) == 0.0
