/**
 * Soft payload-size cap with smart per-field truncation.
 *
 * The client calls {@link maybeTruncate} on every event's payload
 * before the event enters the shipper queue. Payloads under the
 * configured cap pass through unchanged. Payloads over the cap have
 * their longest top-level string field iteratively shortened until
 * the serialized payload fits, preserving structure so downstream
 * readers can still see which fields existed.
 *
 * **Why a soft cap, not a hard reject.** An observability product's
 * worst failure mode is silently losing events. A hard cap (drop the
 * event) means the customer never sees that something went wrong; a
 * soft cap means they see "something happened, here is a useful
 * fingerprint, the payload was bigger than we kept." Truncation is
 * the deliberately less-lossy path.
 *
 * **Why top-level fields only.** Walking arbitrarily nested
 * structures adds complexity for diminishing returns: in practice
 * almost all oversized event payloads stuff one or two big strings at
 * the top level (`response`, `prompt`, `diff`, `stack_trace`). The
 * SDK README documents that deeply nested strings under heavily
 * structured payloads are not automatically truncated.
 *
 * **Markers added on truncation:**
 *
 *   - `_truncated`                (boolean) — always `true` when this
 *                                  helper altered the payload.
 *   - `_original_payload_bytes`   (number) — serialized byte count of
 *                                  the original payload, so the
 *                                  customer can see how much got
 *                                  dropped.
 *   - `_truncated_fields`         (string[]) — names of the top-level
 *                                  fields whose values got shortened.
 *
 * **Fail-open.** Any exception during truncation falls back to the
 * original payload. Compression-style fail-open posture matches the
 * rest of the SDK.
 */

/**
 * Default per-event payload cap. Covers ~99% of real-world event
 * shapes. Customers can override via `configure({ maxPayloadBytes:
 * ... })`.
 */
export const DEFAULT_MAX_PAYLOAD_BYTES = 32 * 1024;

/**
 * Minimum characters preserved for any single string before
 * truncation gives up on shrinking it further. Below this floor the
 * surviving prefix is too small to be useful for debugging.
 */
export const MIN_KEEP_CHARS = 100;

/**
 * Safety cap on the iterative shrink loop. 50 iterations of halving
 * would take a 1 GB string below MIN_KEEP_CHARS, so anything
 * legitimate terminates well under this.
 */
export const MAX_ITERATIONS = 50;

/**
 * Suffix appended to truncated string values so a human reader can
 * tell at a glance that the field is not its full content.
 */
export const TRUNCATION_SUFFIX = "...[mesedi:truncated]";

/** Field name stamped onto a truncated payload (boolean flag). */
export const MARKER_TRUNCATED = "_truncated";
/** Field name stamped onto a truncated payload (original byte count). */
export const MARKER_ORIGINAL_BYTES = "_original_payload_bytes";
/** Field name stamped onto a truncated payload (which fields shrank). */
export const MARKER_TRUNCATED_FIELDS = "_truncated_fields";

const ENCODER = new TextEncoder();

function serializeBytes(value: unknown): number {
  return ENCODER.encode(JSON.stringify(value)).byteLength;
}

/**
 * Truncate top-level string fields if the serialized payload exceeds
 * `maxBytes`.
 *
 * Never throws. On any exception, the original payload is returned
 * so an event always ships even if the truncation logic itself
 * breaks.
 *
 * @param payload the event payload object
 * @param maxBytes cap on the serialized JSON byte count
 * @returns the original payload if under cap, otherwise a new object
 *   with the longest string fields shortened and marker fields
 *   stamped at the top level
 */
export function maybeTruncate(
  payload: Record<string, unknown>,
  maxBytes: number = DEFAULT_MAX_PAYLOAD_BYTES,
): Record<string, unknown> {
  try {
    if (payload === null || typeof payload !== "object" || Array.isArray(payload)) {
      return payload;
    }
    const originalBytes = serializeBytes(payload);
    if (originalBytes <= maxBytes) {
      return payload;
    }

    const result: Record<string, unknown> = { ...payload };
    const truncatedFields: string[] = [];

    for (let i = 0; i < MAX_ITERATIONS; i++) {
      if (serializeBytes(result) <= maxBytes) break;

      let longestKey: string | null = null;
      let longestLen = MIN_KEEP_CHARS;
      for (const [k, v] of Object.entries(result)) {
        if (k.startsWith("_")) continue;
        if (typeof v !== "string") continue;
        if (v.length > longestLen) {
          longestLen = v.length;
          longestKey = k;
        }
      }
      if (longestKey === null) break;

      const currentVal = result[longestKey] as string;
      const newLen = Math.max(MIN_KEEP_CHARS, Math.floor(currentVal.length / 2));
      result[longestKey] = currentVal.slice(0, newLen) + TRUNCATION_SUFFIX;
      if (!truncatedFields.includes(longestKey)) {
        truncatedFields.push(longestKey);
      }
    }

    result[MARKER_TRUNCATED] = true;
    result[MARKER_ORIGINAL_BYTES] = originalBytes;
    result[MARKER_TRUNCATED_FIELDS] = truncatedFields;
    return result;
  } catch (err) {
    // Compression-style fail-open. Compression and truncation both
    // serve performance; neither is allowed to drop an event.
    console.warn(
      `mesedi: payload truncation failed, shipping original: ${String(err)}`,
    );
    return payload;
  }
}
