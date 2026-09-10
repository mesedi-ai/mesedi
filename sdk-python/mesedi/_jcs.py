"""Canonical JSON hashing for declared tool schemas.

Feeds the backend's tool_definition_drift detector (the mcp-pin
class: a tool whose declared input schema changes while its
description stays byte-identical). The backend only ever sees the
hash, so both SDKs must produce the SAME hash for the same schema or
a project mixing Python and TypeScript agents would see phantom
drift.

Canonicalization follows RFC 8785 (JCS) for the JSON that schema
documents are in practice: object keys sorted, no whitespace,
minimal string escaping, UTF-8. Two honest deviations from full JCS,
both irrelevant for real-world JSON Schema documents and both
mirrored exactly by the TypeScript SDK so the hashes still agree:

- Non-integer numbers serialize via the language's shortest-repr
  (Python repr / JS Number#toString), which matches JCS for common
  values but is not proven identical for every float. Schemas are
  strings, booleans and small integers in practice.
- Key ordering is by Unicode code point rather than UTF-16 code
  units, which differs only when keys contain characters outside the
  Basic Multilingual Plane.
"""

from __future__ import annotations

import hashlib
import json
from typing import Any

__all__ = ["input_schema_hash"]


def input_schema_hash(schema: Any) -> str:
    """SHA-256 hex over the canonicalized schema.

    Returns "" for None or anything not JSON-serializable, so callers
    can omit the field entirely, and the backend distinguishes "no
    declared schema" from a real value, the same contract
    tool_description established.
    """
    if schema is None:
        return ""
    try:
        canonical = json.dumps(
            schema,
            sort_keys=True,
            separators=(",", ":"),
            ensure_ascii=False,
        )
    except (TypeError, ValueError):
        return ""
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()
