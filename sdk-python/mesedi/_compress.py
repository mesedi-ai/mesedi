"""
zstd compression helper for outbound payloads.

The shipper calls :func:`maybe_compress` on every JSON-serialized
payload before handing it to httpx. Bodies below the threshold
(default 1 KB) return unchanged because zstd framing overhead
outweighs gains on tiny payloads. Bodies at or above the threshold
are encoded and returned alongside the HTTP headers the backend
needs to recognize the encoding.

**Fail-open.** Any exception during encoding falls back to the
original uncompressed body. Compression is an optimization, not a
contract; the request still has to land even if the encoder is
unavailable or throws.

**Backend negotiation.** The backend looks for
``Content-Encoding: zstd`` and transparently decompresses before the
handler reads the body. Backends without that middleware (pre-#242.a)
will see the compressed body as opaque bytes and reject it as
malformed JSON — which is why this SDK version (v0.3.0) requires a
backend that speaks compression. The companion backend commit shipped
first; you only see this module if you upgraded to a SDK release that
ships after.
"""

from __future__ import annotations

import logging
from typing import Dict, Optional, Tuple

try:
    import zstandard as _zstd
    _ZSTD_AVAILABLE = True
except ImportError:  # pragma: no cover
    _ZSTD_AVAILABLE = False
    _zstd = None  # type: ignore[assignment]

logger = logging.getLogger("mesedi.compress")

#: Bodies strictly smaller than this byte count skip compression so
#: the zstd frame overhead does not dominate small payloads. 1 KB is
#: the smallest size at which zstd reliably compresses to <100% of the
#: original on typical JSON inputs.
DEFAULT_THRESHOLD_BYTES = 1024

#: Content-Encoding value the backend middleware accepts. Must match
#: ``SupportedRequestEncoding`` on the backend.
SUPPORTED_ENCODING = "zstd"

# Single shared encoder. Construction is non-trivial (allocates an
# internal context), so we lazily build one on first use and reuse it
# across calls. ZstdCompressor.compress() is documented as safe for
# concurrent calls.
_ENCODER: Optional["_zstd.ZstdCompressor"] = None  # type: ignore[name-defined]


def _encoder() -> Optional["_zstd.ZstdCompressor"]:  # type: ignore[name-defined]
    """Return the shared encoder, building it on first call.

    Returns ``None`` if the ``zstandard`` package is not importable,
    which makes :func:`maybe_compress` a no-op (fail-open).
    """
    global _ENCODER
    if not _ZSTD_AVAILABLE:
        return None
    if _ENCODER is None:
        _ENCODER = _zstd.ZstdCompressor()
    return _ENCODER


def maybe_compress(
    body: bytes,
    threshold_bytes: int = DEFAULT_THRESHOLD_BYTES,
) -> Tuple[bytes, Dict[str, str]]:
    """Compress ``body`` with zstd if its size meets the threshold.

    :param body: the raw serialized request body.
    :param threshold_bytes: minimum size before compression engages.
        Bodies strictly smaller than this byte count pass through
        unchanged.
    :returns: a ``(body, extra_headers)`` tuple. ``extra_headers`` is
        empty when no compression was applied; otherwise it contains
        exactly ``Content-Encoding: zstd`` for the backend middleware
        to recognize.

    Never raises. On any exception, the original body is returned
    with empty headers so the request still ships uncompressed.
    """
    if len(body) < threshold_bytes:
        return body, {}
    encoder = _encoder()
    if encoder is None:
        return body, {}
    try:
        compressed = encoder.compress(body)
    except Exception as exc:  # pragma: no cover - defensive
        logger.warning("mesedi: zstd encode failed, sending uncompressed: %s", exc)
        return body, {}
    return compressed, {"Content-Encoding": SUPPORTED_ENCODING}
