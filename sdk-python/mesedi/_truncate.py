"""
Soft payload-size cap with smart per-field truncation (#243).

The shipper calls :func:`maybe_truncate` on every event's payload
before batching. Payloads under the configured cap pass through
unchanged. Payloads over the cap have their longest top-level string
field iteratively shortened until the serialized payload fits,
preserving structure so downstream readers can still see which fields
existed.

**Why a soft cap and not a hard reject.** An observability product's
worst failure mode is silently losing events. A hard cap (drop the
event) means the customer never sees that something went wrong; a
soft cap means they see "something happened, here is a useful
fingerprint, the payload was bigger than we kept." Truncation is
the deliberately less-lossy path.

**Why top-level fields only.** A walk-everything approach handles
nested structures cleanly but adds complexity for diminishing
returns: in practice almost all oversized event payloads stuff one or
two big strings at the top level (`response`, `prompt`, `diff`,
`stack_trace`). The SDK README documents that nested deeply nested
strings under heavily structured payloads aren't automatically
truncated; customers with that shape can flatten on their side.

**Markers added on truncation:**

  - ``_truncated``               (bool) — always True when this helper
                                  altered the payload. Downstream
                                  readers (dashboard, alerts) key on
                                  this to render a "truncated" chip.
  - ``_original_payload_bytes``  (int) — serialized byte count of the
                                  original payload before truncation.
                                  Lets the customer see how much got
                                  dropped.
  - ``_truncated_fields``        (list[str]) — names of the top-level
                                  fields whose values got shortened.
                                  Helps debugging without forcing the
                                  reader to diff against the original.

**Fail-open.** Any exception during truncation falls back to the
original payload. Compression-style fail-open posture matches the
rest of the SDK.
"""

from __future__ import annotations

import json
import logging
from typing import Any, Dict, List

logger = logging.getLogger("mesedi.truncate")

#: Default per-event payload cap. Covers ~99% of real-world event
#: shapes (LLM call + response: typically 5-15 KB; tool call: a few
#: KB; error event: 2-3 KB stack + a few KB context). Customers can
#: override via `configure(max_payload_bytes=...)`.
DEFAULT_MAX_PAYLOAD_BYTES = 32 * 1024

#: Minimum characters preserved for any single string before
#: truncation gives up on shrinking it further. Below this floor the
#: surviving prefix is too small to be useful for debugging.
MIN_KEEP_CHARS = 100

#: Safety cap on the iterative shrink loop. 50 iterations of halving
#: would take a 1 GB string below MIN_KEEP_CHARS, so anything legit
#: terminates well under this.
MAX_ITERATIONS = 50

#: Suffix appended to truncated string values so a human reader can
#: tell at a glance that this isn't the full content.
TRUNCATION_SUFFIX = "...[mesedi:truncated]"

# Field names the helper stamps onto a truncated payload. Exposed as
# constants so downstream readers (tests, dashboard) don't have to
# string-match.
MARKER_TRUNCATED = "_truncated"
MARKER_ORIGINAL_BYTES = "_original_payload_bytes"
MARKER_TRUNCATED_FIELDS = "_truncated_fields"


def _serialize(obj: Any) -> bytes:
    """JSON-serialize using the same separators the shipper uses, so the
    byte count we measure matches the byte count the shipper sends."""
    return json.dumps(obj, separators=(",", ":")).encode("utf-8")


def maybe_truncate(
    payload: Dict[str, Any],
    max_bytes: int = DEFAULT_MAX_PAYLOAD_BYTES,
) -> Dict[str, Any]:
    """Truncate top-level string fields if the serialized payload
    exceeds ``max_bytes``.

    :param payload: the event payload dict. Non-dict input passes
        through unchanged (defensive; type hint says dict but
        customers may violate it).
    :param max_bytes: cap on the serialized JSON byte count.
    :returns: the original payload if under cap, otherwise a new dict
        with the longest string fields shortened and marker fields
        stamped at the top level.

    Never raises. On any exception, the original payload is returned
    so an event always ships even if the truncation logic itself
    breaks.
    """
    try:
        if not isinstance(payload, dict):
            return payload  # type: ignore[return-value]
        original = _serialize(payload)
        original_bytes = len(original)
        if original_bytes <= max_bytes:
            return payload

        result: Dict[str, Any] = dict(payload)  # shallow copy
        truncated_fields: List[str] = []

        for _ in range(MAX_ITERATIONS):
            current = len(_serialize(result))
            if current <= max_bytes:
                break
            # Pick the longest top-level string that is not a marker
            # field and still has room to shrink.
            candidates = [
                (k, v)
                for k, v in result.items()
                if isinstance(v, str)
                and not k.startswith("_")
                and len(v) > MIN_KEEP_CHARS
            ]
            if not candidates:
                break
            key, val = max(candidates, key=lambda kv: len(kv[1]))
            new_len = max(MIN_KEEP_CHARS, len(val) // 2)
            result[key] = val[:new_len] + TRUNCATION_SUFFIX
            if key not in truncated_fields:
                truncated_fields.append(key)

        # Stamp markers. Doing this after the loop keeps the marker
        # bytes themselves out of the shrink budget (which is fine,
        # markers are tiny — ~80 bytes worst case).
        result[MARKER_TRUNCATED] = True
        result[MARKER_ORIGINAL_BYTES] = original_bytes
        result[MARKER_TRUNCATED_FIELDS] = truncated_fields
        return result
    except Exception as exc:  # pragma: no cover - defensive
        logger.warning(
            "mesedi: payload truncation failed, shipping original: %s", exc
        )
        return payload
