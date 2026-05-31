# Posts captured executions + events to the Mesedi backend.
#
# This module is the only place in the wrapper that talks to Mesedi
# itself. It is built around one ironclad contract: never block, never
# raise, never make the user's command fail. If the backend is
# unreachable or returns 5xx, we print a single warning line to stderr
# (rate-limited via env var) and move on.
#
# The HTTP calls use `requests` because:
#   - Universally installable in every CI image
#   - Connection pooling is automatic (we issue 2 POSTs per invocation
#     so this barely matters, but still)
#   - Timeouts are clearer than urllib's
#
# Endpoint contract (matched against backend/internal/api/handlers.go):
#   POST /executions   body: events.Execution JSON
#   POST /events       body: array of events.Event JSON
#
# Auth: Authorization: Bearer mesedi_sk_...
from __future__ import annotations

import json
import os
import sys
import time
import uuid
from dataclasses import dataclass, field
from typing import Any, Optional

try:
    import requests
except ImportError:  # pragma: no cover, install-time guard
    sys.stderr.write(
        "mesedi-curl: the 'requests' package is required. "
        "Install with: pip install requests\n"
    )
    raise


# Default backend URL. Self-hosted users override via env. Production
# Mesedi cloud lives at api.mesedi.ai.
DEFAULT_BACKEND_URL = "https://api.mesedi.ai"

# Default timeouts in seconds. The shim's whole point is to not block
# user-visible work, so we keep these tight. If Mesedi can't respond
# in 3 seconds, we drop the event and move on.
INGEST_TIMEOUT_SECONDS = 3.0


@dataclass
class MesediConfig:
    """Resolved configuration for one invocation.

    Pulled from env vars by load_from_env(). When api_key is empty,
    the wrapper records nothing (silent pass-through mode), so the
    user can drop `mesedi-curl` into a script with no credentials and
    get plain-curl behavior.
    """

    api_key: str = ""
    backend_url: str = DEFAULT_BACKEND_URL
    project_id: Optional[str] = None  # Optional override; usually inferred from key.
    silent: bool = False  # Suppress warning prints if True.

    @property
    def enabled(self) -> bool:
        return bool(self.api_key)


def load_from_env() -> MesediConfig:
    """Read MESEDI_* environment variables into a MesediConfig."""
    return MesediConfig(
        api_key=os.environ.get("MESEDI_API_KEY", ""),
        backend_url=os.environ.get("MESEDI_BACKEND_URL", DEFAULT_BACKEND_URL).rstrip(
            "/"
        ),
        project_id=os.environ.get("MESEDI_PROJECT_ID") or None,
        silent=os.environ.get("MESEDI_SILENT", "").lower() in ("1", "true", "yes"),
    )


@dataclass
class CapturedCall:
    """All the data the wrapper accumulated about one curl call.

    Built up by the CLI as it executes the underlying request, then
    handed to publish() which converts it into the backend's event
    shape and POSTs it.
    """

    execution_id: str = field(default_factory=lambda: str(uuid.uuid4()))
    event_id: str = field(default_factory=lambda: str(uuid.uuid4()))
    started_at: float = field(default_factory=time.time)
    ended_at: float = 0.0
    method: str = "GET"
    url: str = ""
    status_code: int = 0
    provider: str = "generic"
    model: str = ""
    input_tokens: int = 0
    output_tokens: int = 0
    finish_reason: str = ""
    user_prompt_preview: str = ""
    response_preview: str = ""
    is_streaming: bool = False
    error: str = ""  # Set if the underlying curl call failed.

    @property
    def latency_ms(self) -> int:
        if self.ended_at == 0.0:
            return 0
        return int((self.ended_at - self.started_at) * 1000)

    @property
    def crashed(self) -> bool:
        return bool(self.error) or self.status_code >= 500


def publish(config: MesediConfig, call: CapturedCall) -> None:
    """Send the captured call to Mesedi. Never raises.

    Failure modes (all silent except for a one-line stderr warning):
      - No API key: skip silently. Wrapper acts as plain curl.
      - Network error / timeout: skip, warn once per process.
      - Backend 4xx: warn once per process with the response body
        (likely an auth or config problem the user needs to see).
      - Backend 5xx: skip, warn once per process.
    """
    if not config.enabled:
        return

    headers = {
        "Authorization": f"Bearer {config.api_key}",
        "Content-Type": "application/json",
        "User-Agent": f"mesedi-curl/{_version()}",
    }

    # Step 1: create the execution.
    exec_body = _build_execution_body(config, call)
    if not _post(
        config,
        path="/executions",
        body=exec_body,
        headers=headers,
        action="create execution",
    ):
        return

    # Step 2: post the single LLM-call event under that execution.
    event_body = _build_event_body(call)
    _post(
        config,
        path="/events",
        body=[event_body],
        headers=headers,
        action="ingest event",
    )


def _build_execution_body(config: MesediConfig, call: CapturedCall) -> dict[str, Any]:
    """Build the events.Execution payload.

    The shim creates one Execution per curl invocation. The Mesedi
    convention is that an Execution is the unit a developer cares about
    (one user-facing agent run); for direct curl calls we treat each
    call as its own execution because we have no enclosing context.
    """
    status = "completed"
    crash_signature = ""
    if call.crashed:
        status = "crashed"
        crash_signature = call.error or f"HTTP {call.status_code}"

    body: dict[str, Any] = {
        "execution_id": call.execution_id,
        "status": status,
        "started_at": _iso(call.started_at),
        "ended_at": _iso(call.ended_at) if call.ended_at else None,
        "duration_ms": call.latency_ms,
        "total_tokens_in": call.input_tokens,
        "total_tokens_out": call.output_tokens,
        "sdk_language": "mesedi-curl",
        "sdk_version": _version(),
    }
    if crash_signature:
        body["crash_signature"] = crash_signature[:256]
    if call.user_prompt_preview:
        body["input_summary"] = call.user_prompt_preview[:512]
    if call.response_preview:
        body["output_summary"] = call.response_preview[:512]
    if config.project_id:
        body["project_id"] = config.project_id
    return body


def _build_event_body(call: CapturedCall) -> dict[str, Any]:
    """Build one events.Event row, with an embedded LLMCallPayload."""
    payload = {
        "provider": call.provider,
        "model": call.model,
        "user_prompt": call.user_prompt_preview,
        "response": call.response_preview,
        "input_tokens": call.input_tokens,
        "output_tokens": call.output_tokens,
        "latency_ms": call.latency_ms,
        "finish_reason": call.finish_reason,
    }
    return {
        "event_id": call.event_id,
        "execution_id": call.execution_id,
        "event_type": "llm_call",
        "sequence": 0,
        "timestamp": _iso(call.started_at),
        "duration_ms": call.latency_ms,
        "payload": payload,
    }


def _post(
    config: MesediConfig,
    *,
    path: str,
    body: Any,
    headers: dict[str, str],
    action: str,
) -> bool:
    """POST to the Mesedi backend. Returns True on success, False on any failure."""
    url = config.backend_url + path
    try:
        resp = requests.post(
            url,
            data=json.dumps(body, default=str),
            headers=headers,
            timeout=INGEST_TIMEOUT_SECONDS,
        )
    except requests.RequestException as exc:
        _warn(
            config,
            f"could not {action} (network error: {type(exc).__name__})",
        )
        return False

    if resp.status_code >= 400:
        # 4xx warnings include the body because the user needs to see
        # them (typically an invalid API key, project mismatch, or rate
        # limit). 5xx warnings stay terse since the user can't act.
        if resp.status_code < 500:
            preview = resp.text[:300] if resp.text else ""
            _warn(
                config,
                f"could not {action} ({resp.status_code}): {preview}",
            )
        else:
            _warn(
                config,
                f"could not {action} ({resp.status_code} server error)",
            )
        return False
    return True


# We deliberately warn at most once per (process, message-class) so a
# loop of curl calls doesn't spam stderr.
_warned: set[str] = set()


def _warn(config: MesediConfig, message: str) -> None:
    if config.silent:
        return
    # Dedupe at the action prefix granularity (everything before the
    # parenthesized detail).
    key = message.split("(", 1)[0].strip()
    if key in _warned:
        return
    _warned.add(key)
    sys.stderr.write(f"mesedi-curl: {message}\n")


def _iso(epoch_seconds: float) -> str:
    """RFC 3339 UTC timestamp string for the Mesedi event schema."""
    from datetime import datetime, timezone

    return datetime.fromtimestamp(epoch_seconds, tz=timezone.utc).isoformat().replace(
        "+00:00", "Z"
    )


def _version() -> str:
    from mesedi_curl.version import __version__

    return __version__
