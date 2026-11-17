"""Report chaining and optional signing (mesedi-report-chain-v1).

Every report the shipper sends carries ``chain_prev``: the fingerprint
of the report it sent before it in the same run, or the run's genesis
value for the first. The backend fingerprints each report exactly as it
arrives and checks that a run's reports form one unbroken path, so a
report removed, changed or slipped in after sending is caught.

The fingerprint covers the payload as the exact bytes sent. The shipper
serializes every request body compactly (``separators=(",", ":")``),
which nests each payload as ``json.dumps(payload, separators=(",", ":"))``,
so those are the bytes hashed here. Field list and test vectors:
spec/report_chain_vectors.json, shared with the backend and the
TypeScript SDK.

Signing is optional: ``configure(report_signer=...)`` takes a callable
that receives the 32-byte fingerprint and returns ``(key_id,
signature_bytes)``, an Ed25519 signature by a key registered to the
project. It is meant for a reporting process the agent cannot reach;
signing proves which process sent a report, not that it is true, and
whoever holds every key can still invent reports.
"""

from __future__ import annotations

import base64
import hashlib
import json
import logging
from collections import OrderedDict
from datetime import datetime, timezone
from typing import Any, Callable, Dict, Optional, Tuple

ALGORITHM = "mesedi-report-chain-v1"

# Runs whose last fingerprint is remembered. A run older than this many
# newer runs that sends again starts a second chain, which the backend
# reports as broken; 10,000 concurrent runs per process is far beyond
# any real agent host.
_MAX_TRACKED_RUNS = 10_000

ReportSigner = Callable[[bytes], Tuple[str, bytes]]

logger = logging.getLogger("mesedi")


def genesis(execution_id: str) -> str:
    """The chain_prev of a run's first report."""
    return hashlib.sha256((ALGORITHM + " genesis\n" + execution_id).encode("utf-8")).hexdigest()


def canonical_timestamp(ts: str) -> str:
    """UTC with exactly six fractional digits, as the backend formats
    the instant it parsed. Raises ValueError on an unparseable value."""
    s = ts.strip()
    if s.endswith("Z") or s.endswith("z"):
        s = s[:-1] + "+00:00"
    if "." in s:
        head, rest = s.split(".", 1)
        digits = ""
        while rest and rest[0].isdigit():
            digits, rest = digits + rest[0], rest[1:]
        s = head + "." + (digits + "000000")[:6] + rest
    dt = datetime.fromisoformat(s)
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return dt.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")


def end_message(execution_id: str, last_fingerprint: str) -> bytes:
    """The 32 bytes a signer signs to declare a run's last report. Not
    the fingerprint itself, so a report's own signature cannot be
    replayed as an end. Vectors: spec/report_chain_vectors.json "end"."""
    return hashlib.sha256(
        (ALGORITHM + " end\n" + execution_id + "\n" + last_fingerprint).encode("utf-8")
    ).digest()


def payload_bytes(payload: Any) -> bytes:
    """The payload exactly as the shipper puts it on the wire."""
    return json.dumps(payload, separators=(",", ":")).encode("utf-8")


def fingerprint(
    *,
    event_id: str,
    execution_id: str,
    event_type: str,
    sequence: int,
    timestamp: str,
    duration_ms: Optional[int],
    payload: bytes,
    chain_prev: str,
) -> str:
    fields = (
        ("algorithm", ALGORITHM),
        ("event_id", event_id),
        ("execution_id", execution_id),
        ("event_type", event_type),
        ("sequence", str(int(sequence))),
        ("timestamp", canonical_timestamp(timestamp)),
        ("duration_ms", str(int(duration_ms or 0))),
        ("payload_sha256", hashlib.sha256(payload).hexdigest()),
        ("chain_prev", chain_prev),
    )
    buf = bytearray()
    for label, value in fields:
        v = value.encode("utf-8")
        buf += label.encode("ascii") + b":" + str(len(v)).encode("ascii") + b":" + v + b"\n"
    return hashlib.sha256(bytes(buf)).hexdigest()


class ReportChainer:
    """Chains events in the order the shipper sends them. Used only
    from the shipper's single worker thread, so it needs no lock."""

    def __init__(self, signer: Optional[ReportSigner] = None) -> None:
        self._last: "OrderedDict[str, str]" = OrderedDict()
        self._signer = signer

    def chain(self, event: Any) -> None:
        """Set event.chain_prev (and event.signature when signing) and
        advance the run's chain. An event whose timestamp or payload
        cannot be encoded is left unchained; the backend accepts it and
        marks the run's chain as broken, which is the truth."""
        prev = self._last.get(event.execution_id) or genesis(event.execution_id)
        try:
            fp = fingerprint(
                event_id=event.event_id,
                execution_id=event.execution_id,
                event_type=event.event_type,
                sequence=event.sequence,
                timestamp=event.timestamp,
                duration_ms=event.duration_ms,
                payload=payload_bytes(event.payload),
                chain_prev=prev,
            )
        except (TypeError, ValueError) as exc:
            logger.warning("mesedi: report %s left unchained: %s", event.event_id, exc)
            return
        event.chain_prev = prev
        if self._signer is not None:
            try:
                key_id, sig = self._signer(bytes.fromhex(fp))
                event.signature = {"key_id": key_id, "sig": base64.b64encode(sig).decode("ascii")}
            except Exception as exc:  # the signer is customer code
                logger.warning("mesedi: report %s could not be signed: %s", event.event_id, exc)
        self._last[event.execution_id] = fp
        self._last.move_to_end(event.execution_id)
        while len(self._last) > _MAX_TRACKED_RUNS:
            self._last.popitem(last=False)

    def end(self, execution_id: str) -> Optional[Dict[str, Any]]:
        """The run's declared end for its run-finished message: the last
        fingerprint sent, signed (over end_message) when signing. None
        when no report of the run was chained, so nothing is declared."""
        last = self._last.get(execution_id)
        if last is None:
            return None
        out: Dict[str, Any] = {"hash": last}
        if self._signer is not None:
            try:
                key_id, sig = self._signer(end_message(execution_id, last))
                out["signature"] = {"key_id": key_id, "sig": base64.b64encode(sig).decode("ascii")}
            except Exception as exc:  # the signer is customer code
                logger.warning("mesedi: the end of run %s could not be signed: %s", execution_id, exc)
        return out


def ed25519_signer(key_id: str, private_key: bytes) -> ReportSigner:
    """A report signer from a raw 32-byte Ed25519 private key, for a
    reporting process kept apart from the agent. Needs the optional
    ``cryptography`` package (``pip install "mesedi[signing]"``).

    Register the matching public key in the dashboard (Settings, Report
    signing keys) under the same key_id; only a signed-in admin can.
    """
    try:
        from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
    except ImportError as exc:  # pragma: no cover - depends on the extra
        raise RuntimeError(
            'report signing needs the "cryptography" package: pip install "mesedi[signing]"'
        ) from exc
    key = Ed25519PrivateKey.from_private_bytes(private_key)

    def sign(message: bytes) -> Tuple[str, bytes]:
        return key_id, key.sign(message)

    return sign
