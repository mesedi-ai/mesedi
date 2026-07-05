/**
 * gzip compression helper for outbound request bodies.
 *
 * The client calls {@link maybeCompress} on every JSON-serialized
 * payload before handing it to `fetch`. Bodies below the threshold
 * (default 1 KB) return unchanged because gzip framing overhead
 * outweighs gains on tiny payloads. Bodies at or above the threshold
 * are encoded and returned with the HTTP headers the backend
 * middleware recognizes.
 *
 * **Why gzip, not zstd.** The Python SDK uses zstd because the
 * `zstandard` package is widely available and adds modest install
 * weight. JavaScript zstd encoders are heavy (~750 KB WASM, async
 * init) and would break this SDK's lightweight install posture. Node
 * ships gzip synchronously in its standard library, so gzip lets us
 * compress with zero runtime dependencies. Backend accepts both
 * Content-Encoding values through the same middleware path.
 *
 * **Fail-open.** Any exception during encoding falls back to the
 * original uncompressed body. Compression is an optimization, not a
 * contract; the request still has to land even if the encoder
 * throws.
 *
 * **Backend negotiation.** The backend looks for
 * `Content-Encoding: gzip` (or `zstd` from the Python SDK) and
 * transparently decompresses before the handler reads the body.
 * Backends without that middleware would see the compressed body as
 * opaque bytes and reject it as malformed JSON, which is why this
 * SDK version (v0.3.0) requires the matching backend release.
 */

import { gzipSync } from "node:zlib";

/**
 * Bodies strictly smaller than this byte count skip compression so
 * the gzip frame overhead does not dominate small payloads. 1 KB is
 * the smallest size at which gzip reliably compresses to under 100%
 * of the original on typical JSON inputs.
 */
export const DEFAULT_THRESHOLD_BYTES = 1024;

/**
 * Content-Encoding value the backend middleware accepts. Must match
 * `EncodingGzip` on the backend side.
 */
export const SUPPORTED_ENCODING = "gzip";

/** Return shape of {@link maybeCompress}. */
export interface MaybeCompressedBody {
  /**
   * The body bytes to send. Equal to the input when no compression
   * was applied, otherwise the gzip-encoded form.
   */
  body: Uint8Array;
  /**
   * Extra headers the caller should merge into the outgoing request.
   * Empty when no compression was applied; otherwise contains exactly
   * `Content-Encoding: gzip`.
   */
  extraHeaders: Record<string, string>;
}

/**
 * Compress `body` with gzip if its size meets the threshold.
 *
 * Never throws. On any encoding exception, the original body is
 * returned with empty headers so the request still ships
 * uncompressed.
 *
 * @param body raw serialized request body
 * @param thresholdBytes minimum size before compression engages
 * @returns the body to send plus any extra headers to merge
 */
export function maybeCompress(
  body: Uint8Array,
  thresholdBytes: number = DEFAULT_THRESHOLD_BYTES,
): MaybeCompressedBody {
  if (body.byteLength < thresholdBytes) {
    return { body, extraHeaders: {} };
  }
  try {
    const compressed = gzipSync(body);
    // Node's gzipSync returns a Buffer, which extends Uint8Array but
    // some downstream type-checkers prefer a plain Uint8Array view.
    // The view shares the underlying ArrayBuffer with the Buffer; no
    // memory copy is performed.
    return {
      body: new Uint8Array(
        compressed.buffer,
        compressed.byteOffset,
        compressed.byteLength,
      ),
      extraHeaders: { "Content-Encoding": SUPPORTED_ENCODING },
    };
  } catch (err) {
    console.warn(
      `mesedi: gzip encode failed, sending uncompressed: ${String(err)}`,
    );
    return { body, extraHeaders: {} };
  }
}
