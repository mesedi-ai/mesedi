"""
Event types and execution records that mirror the Mesedi backend schema.

Source-of-truth lives in the Go backend at
`backend/internal/events/types.go`. Any new event_type or status value
added there MUST be added here (and vice versa); the strict JSON
decoder on the backend will reject events whose fields it does not
recognize.

The wire format uses RFC 3339 UTC timestamps for every time field.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any, ClassVar, Dict, Optional


def utcnow_rfc3339() -> str:
    """Return current UTC time as an RFC 3339 string with microseconds.

    Matches the format the Go backend's time.Time RFC3339 marshaller
    accepts on POST /executions / POST /events. Example output:
    ``2026-05-14T17:33:42.123456Z``.
    """
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")


class EventType:
    """Seven event types the backend understands today.

    Match exactly the EventType constants in
    ``backend/internal/events/types.go``. Phase 3+ detectors are keyed on
    these values, so adding a new type means coordinating SDK and backend
    in lockstep.
    """

    LLM_CALL:         ClassVar[str] = "llm_call"
    TOOL_CALL:        ClassVar[str] = "tool_call"
    CHECKPOINT:       ClassVar[str] = "checkpoint"
    EXCEPTION:        ClassVar[str] = "exception"
    VALIDATOR_RESULT: ClassVar[str] = "validator_result"
    DRIFT_SIGNAL:     ClassVar[str] = "drift_signal"
    INJECTION_ALERT:  ClassVar[str] = "injection_alert"


class Status:
    """Execution lifecycle states.

    Match exactly the ExecutionStatus constants in
    ``backend/internal/events/types.go``.
    """

    STARTED:           ClassVar[str] = "started"
    COMPLETED:         ClassVar[str] = "completed"
    CRASHED:           ClassVar[str] = "crashed"
    HALTED:            ClassVar[str] = "halted"
    TIMEOUT:           ClassVar[str] = "timeout"
    VALIDATION_FAILED: ClassVar[str] = "validation_failed"


@dataclass
class Execution:
    """An agent run as the Mesedi backend records it.

    Used by `@wrap` internally and also exposed publicly so advanced
    callers can construct executions manually. The two `*_payload`
    methods return exactly the JSON shapes the backend expects on POST
    and PATCH respectively.
    """

    execution_id: str
    status: str = Status.STARTED
    started_at: str = field(default_factory=utcnow_rfc3339)
    ended_at: Optional[str] = None
    duration_ms: Optional[int] = None
    total_tokens_in: Optional[int] = None
    total_tokens_out: Optional[int] = None
    estimated_cost_usd: Optional[float] = None
    sdk_language: str = "python"
    sdk_version: str = "0.0.1"
    crash_signature: Optional[str] = None

    def start_payload(self) -> Dict[str, Any]:
        """Body for POST /executions (only the fields valid at start)."""
        return {
            "execution_id": self.execution_id,
            "status": self.status,
            "started_at": self.started_at,
            "sdk_language": self.sdk_language,
            "sdk_version": self.sdk_version,
        }

    def end_payload(self) -> Dict[str, Any]:
        """Body for PATCH /executions/{id} (only the fields set at end).

        Omits any None fields so the backend doesn't reject them via its
        strict-decode policy when they're absent.
        """
        out: Dict[str, Any] = {"status": self.status}
        if self.ended_at is not None:
            out["ended_at"] = self.ended_at
        if self.duration_ms is not None:
            out["duration_ms"] = self.duration_ms
        if self.total_tokens_in is not None:
            out["total_tokens_in"] = self.total_tokens_in
        if self.total_tokens_out is not None:
            out["total_tokens_out"] = self.total_tokens_out
        if self.estimated_cost_usd is not None:
            out["estimated_cost_usd"] = self.estimated_cost_usd
        if self.crash_signature is not None:
            out["crash_signature"] = self.crash_signature
        return out


@dataclass
class Event:
    """A single observation within an execution.

    The `payload` field is intentionally opaque JSON, the backend stores
    it as a raw blob and doesn't validate shape, so per-event-type
    payload schemas live SDK-side (Phase 3+) rather than as backend
    contracts. For v0.0.1, callers pass whatever dict makes sense for
    their event type.
    """

    event_id: str
    execution_id: str
    event_type: str
    sequence: int
    timestamp: str = field(default_factory=utcnow_rfc3339)
    duration_ms: Optional[int] = None
    payload: Dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> Dict[str, Any]:
        """Wire-format dict, drop None duration_ms to keep the body lean."""
        out: Dict[str, Any] = {
            "event_id": self.event_id,
            "execution_id": self.execution_id,
            "event_type": self.event_type,
            "sequence": self.sequence,
            "timestamp": self.timestamp,
            "payload": self.payload,
        }
        if self.duration_ms is not None:
            out["duration_ms"] = self.duration_ms
        return out
