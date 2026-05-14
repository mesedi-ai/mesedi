"""
MesediClient — synchronous HTTP client for the Mesedi backend.

For v0.0.1 every API call is synchronous and blocking. That keeps the
implementation small and dependency-light, at the cost of adding ~2 HTTP
round-trips of latency to each `@wrap`-decorated agent call. The async
event buffer (background flusher thread, batched event POSTs) lands in
the next SDK sub-slice.

Authentication: bearer token, the same `mesedi_sk_...` format the
backend mints. Wire-format version is fixed at "1" today; future
breaking changes bump the X-Mesedi-Schema-Version constant and the
backend tightens enforcement.
"""

from __future__ import annotations

import os
from typing import Any, Dict, List, Optional

import httpx

from mesedi.events import Event, Execution

DEFAULT_BASE_URL = "http://localhost:8080"
DEFAULT_TIMEOUT = 10.0
SCHEMA_VERSION = "1"

# Module-level singleton client. Populated by mesedi.configure() and
# consumed by the @wrap decorator. Most callers need exactly one client
# per process — Mesedi is meant to observe agent code, not to be
# instantiated per-call.
_default_client: Optional["MesediClient"] = None


class MesediClient:
    """Synchronous HTTP client wrapping the Mesedi backend ingest endpoints.

    Thread-safety: the underlying httpx.Client is thread-safe for
    concurrent use, so a single MesediClient can be shared across
    threads. Each request is independent; there is no client-side state
    that mutates per call (today). When the async event buffer lands,
    that buffer will be a separate component with its own locking.
    """

    def __init__(
        self,
        api_key: str,
        base_url: str = DEFAULT_BASE_URL,
        timeout: float = DEFAULT_TIMEOUT,
    ):
        if not api_key:
            raise ValueError("api_key is required")
        if not api_key.startswith("mesedi_sk_"):
            # Match the backend's auth-middleware check — fail loudly on
            # the SDK side rather than letting the backend return 401 on
            # the first call.
            raise ValueError(
                "api_key must start with 'mesedi_sk_' "
                "(received an obviously-malformed key)"
            )

        self.api_key = api_key
        self.base_url = base_url.rstrip("/")
        self._http = httpx.Client(
            base_url=self.base_url,
            headers={
                "Authorization": f"Bearer {api_key}",
                "Content-Type": "application/json",
                "X-Mesedi-Schema-Version": SCHEMA_VERSION,
            },
            timeout=timeout,
        )

    # ── context-manager sugar ─────────────────────────────────────────

    def __enter__(self) -> "MesediClient":
        return self

    def __exit__(self, *exc: Any) -> None:
        self.close()

    def close(self) -> None:
        """Close the underlying HTTP client; safe to call multiple times."""
        self._http.close()

    # ── execution endpoints ───────────────────────────────────────────

    def create_execution(self, execution: Execution) -> Dict[str, Any]:
        """POST /executions. Returns the backend's response body as dict."""
        r = self._http.post("/executions", json=execution.start_payload())
        r.raise_for_status()
        return r.json()  # type: ignore[no-any-return]

    def update_execution(self, execution: Execution) -> Dict[str, Any]:
        """PATCH /executions/{id}. Returns the backend's response body as dict."""
        r = self._http.patch(
            f"/executions/{execution.execution_id}",
            json=execution.end_payload(),
        )
        r.raise_for_status()
        return r.json()  # type: ignore[no-any-return]

    # ── event endpoints ───────────────────────────────────────────────

    def send_events(self, events: List[Event]) -> Dict[str, Any]:
        """POST /events as a JSON array. Empty list is a no-op."""
        if not events:
            return {"ok": True, "accepted": 0, "rejected": 0}
        payload = [e.to_dict() for e in events]
        r = self._http.post("/events", json=payload)
        r.raise_for_status()
        return r.json()  # type: ignore[no-any-return]


def configure(
    api_key: Optional[str] = None,
    base_url: Optional[str] = None,
    timeout: float = DEFAULT_TIMEOUT,
) -> MesediClient:
    """Configure the module-level default client.

    Environment-variable fallbacks:
      - api_key  ← MESEDI_API_KEY
      - base_url ← MESEDI_BASE_URL (defaults to ``http://localhost:8080``)

    Calling configure() again replaces the previous default client; the
    previous client's underlying HTTP session is closed first.
    """
    global _default_client

    if api_key is None:
        api_key = os.environ.get("MESEDI_API_KEY")
    if not api_key:
        raise RuntimeError(
            "Mesedi API key not provided. Pass api_key=... to "
            "mesedi.configure() or set MESEDI_API_KEY in the environment."
        )

    if base_url is None:
        base_url = os.environ.get("MESEDI_BASE_URL", DEFAULT_BASE_URL)

    # Close any previous default client before replacing it.
    if _default_client is not None:
        try:
            _default_client.close()
        except Exception:
            # Closing should never fail in practice, but if it does we'd
            # rather replace the client cleanly than crash configure().
            pass

    _default_client = MesediClient(
        api_key=api_key,
        base_url=base_url,
        timeout=timeout,
    )
    return _default_client


def get_client() -> MesediClient:
    """Return the module-level default client.

    Raises RuntimeError if `mesedi.configure()` has not been called and
    MESEDI_API_KEY is not in the environment.
    """
    if _default_client is None:
        # Auto-configure from env if MESEDI_API_KEY is set — convenience
        # for serverless / container environments where configure() at
        # cold-start can be inconvenient.
        env_key = os.environ.get("MESEDI_API_KEY")
        if env_key:
            return configure()
        raise RuntimeError(
            "Mesedi is not configured. Call mesedi.configure(api_key=...) "
            "or set MESEDI_API_KEY in the environment before using @mesedi.wrap."
        )
    return _default_client
