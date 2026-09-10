/**
 * Canonical JSON hashing for declared tool schemas, feeding the
 * backend's tool_definition_drift detector (the mcp-pin class: a
 * tool whose declared input schema changes while its description
 * stays byte-identical).
 *
 * The backend only ever sees the hash, so this MUST produce the
 * same bytes as the Python SDK's mesedi/_jcs.py for the same
 * schema, or a project mixing SDKs would see phantom drift. Both
 * follow RFC 8785 (JCS) for the JSON that schema documents are in
 * practice: keys sorted, no whitespace, minimal escaping, UTF-8,
 * non-ASCII unescaped. A cross-SDK fixture test pins the agreement
 * with a hash computed by the Python side.
 */

import { createHash } from "node:crypto";

/**
 * Recursively stringify with object keys sorted. Throws on values
 * JSON cannot represent (functions, symbols, undefined, BigInt),
 * mirroring Python's json.dumps raising on unserializable input so
 * both SDKs return "" for the same inputs.
 */
function canonicalize(value: unknown): string {
  if (value === null || typeof value !== "object") {
    const s = JSON.stringify(value);
    if (s === undefined) {
      throw new TypeError("unserializable value in schema");
    }
    return s;
  }
  if (Array.isArray(value)) {
    return "[" + value.map(canonicalize).join(",") + "]";
  }
  const obj = value as Record<string, unknown>;
  const keys = Object.keys(obj).sort();
  const parts = keys.map(
    (k) => JSON.stringify(k) + ":" + canonicalize(obj[k]),
  );
  return "{" + parts.join(",") + "}";
}

/**
 * SHA-256 hex over the canonicalized schema. Returns "" for
 * null/undefined or anything not JSON-serializable, so callers omit
 * the field entirely and the backend distinguishes "no declared
 * schema" from a value, the contract tool_description established.
 */
export function inputSchemaHash(schema: unknown): string {
  if (schema === null || schema === undefined) {
    return "";
  }
  let canonical: string;
  try {
    canonical = canonicalize(schema);
  } catch {
    return "";
  }
  return createHash("sha256").update(canonical, "utf8").digest("hex");
}
