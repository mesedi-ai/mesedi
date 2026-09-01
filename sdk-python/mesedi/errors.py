"""
Canonical provider-error vocabulary for the Mesedi SDK.

Every provider integration (anthropic_integration, future
openai_integration, cohere_integration, etc.) classifies its native
exception types into ONE of the values in :data:`ErrorClass`. The
backend reads ``error_class`` from llm_call payloads and uses the
canonical vocabulary to cluster cross-provider signals: e.g. when
both Anthropic and OpenAI are rate-limiting at the same time, the
``provider_incident`` detector sees them under the same
``rate_limited`` bucket regardless of which exception class each SDK
raised.

Adding a new provider:

  1. Map the provider's exception hierarchy to the closest match in
     :data:`ErrorClass`. Use ``UNKNOWN`` for anything that cannot be
     attributed cleanly.
  2. Set ``payload["provider"]`` to a stable, lowercase identifier
     (``"anthropic"``, ``"openai"``, ``"cohere"``, ``"gemini"``).
  3. Include ``error_class`` and (when available) ``http_status`` on
     the failure-path llm_call event.

The vocabulary is deliberately closed. Free-form error strings
would defeat the cross-provider clustering the detectors rely on.
"""

from __future__ import annotations

from typing import Any, Optional

from mesedi._error_classes_generated import (
    ERROR_CLASS_NAMES as _GEN_NAMES,
    PROVIDER_SIDE_ERROR_CLASS_VALUES as _GEN_PROVIDER_SIDE,
)


class _ErrorClassMeta(type):
    """Backing metaclass for ErrorClass: surfaces spec-driven
    constants as class attributes. The actual values come from
    spec/error_classes.yaml via :mod:`mesedi._error_classes_generated`.
    Adding a new class is a one-edit operation (the YAML): no need
    to touch errors.py.
    """

    def __getattr__(cls, name: str) -> str:
        if name in _GEN_NAMES:
            return _GEN_NAMES[name]
        raise AttributeError(
            f"ErrorClass has no attribute {name!r}; add it to "
            f"spec/error_classes.yaml and re-run scripts/codegen.py."
        )


class ErrorClass(metaclass=_ErrorClassMeta):
    """Closed vocabulary of canonical provider-error classes.

    Values come from ``spec/error_classes.yaml`` via the
    codegen-generated module. Access constants the historical way
    (``ErrorClass.RATE_LIMITED``); the metaclass routes the lookup
    through the generated dict so a spec edit is the ONLY change
    needed to add a class.

    The vocabulary is deliberately closed. Free-form error strings
    would defeat the cross-provider clustering the detectors rely on.
    """


# Frozen set of values that count as "provider-side" for the
# provider_incident detector. Sourced from the spec via codegen so
# Python / TypeScript / Go all see the SAME membership without
# manual sync.
PROVIDER_SIDE_ERROR_CLASSES = _GEN_PROVIDER_SIDE


# Mapping from Anthropic exception class names (string, not the
# class object) to canonical ErrorClass values. Keyed by name to
# avoid taking a hard runtime dependency on the anthropic package —
# the SDK works even if anthropic is not installed, and the mapping
# stays correct because exception class names are stable across
# anthropic SDK versions.
_ANTHROPIC_EXCEPTION_MAP = {
    "RateLimitError": ErrorClass.RATE_LIMITED,
    "APITimeoutError": ErrorClass.TIMEOUT,
    "DeadlineExceededError": ErrorClass.TIMEOUT,
    "APIConnectionError": ErrorClass.SERVICE_UNAVAILABLE,
    "ServiceUnavailableError": ErrorClass.SERVICE_UNAVAILABLE,
    "OverloadedError": ErrorClass.SERVICE_UNAVAILABLE,
    "InternalServerError": ErrorClass.INTERNAL_ERROR,
    "APIResponseValidationError": ErrorClass.INTERNAL_ERROR,
    "APIWebhookValidationError": ErrorClass.INTERNAL_ERROR,
    "AuthenticationError": ErrorClass.INVALID_API_KEY,
    "PermissionDeniedError": ErrorClass.INVALID_API_KEY,
    "BadRequestError": ErrorClass.CLIENT_ERROR,
    "NotFoundError": ErrorClass.CLIENT_ERROR,
    "ConflictError": ErrorClass.CLIENT_ERROR,
    "RequestTooLargeError": ErrorClass.CLIENT_ERROR,
    "UnprocessableEntityError": ErrorClass.CLIENT_ERROR,
    # Client construction / configuration errors — raised before
    # any network call. Customer code bug, not a provider incident.
    "MissingDependencyError": ErrorClass.CLIENT_ERROR,
    "MutuallyExclusiveAuthError": ErrorClass.CLIENT_ERROR,
    "StreamAlreadyConsumed": ErrorClass.CLIENT_ERROR,
    # Auth-adjacent: Bedrock / Vertex workload-identity probe failed.
    "WorkloadIdentityError": ErrorClass.INVALID_API_KEY,
    # Transient failure marker the SDK's own retry loop classifies as
    # retryable. Safe default is SERVICE_UNAVAILABLE so the detector
    # still picks up provider signal.
    #
    # VERSION-DEPENDENT: absent in anthropic 0.102.0, present by
    # 0.125.0. Listed in _VERSION_TOLERANT_NAMES in
    # tests/test_mapping_staleness.py so the staleness guard doesn't
    # flag it when someone runs the suite against an older anthropic.
    # (It was briefly removed on 2026-08-20 after a local check against
    # 0.102.0 concluded it never existed; CI on 0.125.0 proved
    # otherwise. Do not remove it again without checking BOTH ends of
    # the supported version range.)
    "RetryableError": ErrorClass.SERVICE_UNAVAILABLE,
    # Base classes — caught last, treated as unknown rather than
    # falsely attributed to a specific bucket.
    "APIStatusError": ErrorClass.UNKNOWN,
    "APIError": ErrorClass.UNKNOWN,
    "AnthropicError": ErrorClass.UNKNOWN,
}


def classify_anthropic_exception(exc: BaseException) -> str:
    """Return the canonical :class:`ErrorClass` for an Anthropic
    SDK exception. Falls back to ``UNKNOWN`` for anything not in the
    map (including non-Anthropic exceptions that somehow reached the
    instrumented call path).

    Args:
        exc: the exception instance raised by the Anthropic SDK.

    Returns:
        One of the string constants on :class:`ErrorClass`.
    """
    return _ANTHROPIC_EXCEPTION_MAP.get(type(exc).__name__, ErrorClass.UNKNOWN)


# Mapping from OpenAI Python SDK exception class names to canonical
# ErrorClass values. Keyed by name (string) for the same dependency-
# avoidance reason as the Anthropic map: the mesedi SDK works even
# if the openai package is not installed.
#
# OpenAI's hierarchy (openai>=1.0):
#   OpenAIError
#   └── APIError
#       ├── APIConnectionError
#       │   └── APITimeoutError
#       └── APIStatusError
#           ├── BadRequestError              (400)
#           ├── AuthenticationError          (401)
#           ├── PermissionDeniedError        (403)
#           ├── NotFoundError                (404)
#           ├── ConflictError                (409)
#           ├── UnprocessableEntityError     (422)
#           ├── RateLimitError               (429)   *
#           └── InternalServerError          (5xx)
#
# (*) RateLimitError is overloaded by OpenAI: it covers both true
# per-minute rate limiting AND insufficient-quota / billing-cap.
# classify_openai_exception() distinguishes the two by inspecting
# the response body's error.code so the detector can route the
# QUOTA_EXHAUSTED bucket separately. Mapping the class itself to
# RATE_LIMITED is the default fall-through for the rare cases where
# the body isn't available.
_OPENAI_EXCEPTION_MAP = {
    "RateLimitError": ErrorClass.RATE_LIMITED,
    "APITimeoutError": ErrorClass.TIMEOUT,
    "APIConnectionError": ErrorClass.SERVICE_UNAVAILABLE,
    "InternalServerError": ErrorClass.INTERNAL_ERROR,
    "AuthenticationError": ErrorClass.INVALID_API_KEY,
    "PermissionDeniedError": ErrorClass.INVALID_API_KEY,
    "BadRequestError": ErrorClass.CLIENT_ERROR,
    "NotFoundError": ErrorClass.CLIENT_ERROR,
    "ConflictError": ErrorClass.CLIENT_ERROR,
    "UnprocessableEntityError": ErrorClass.CLIENT_ERROR,
    # Response arrived but didn't match the expected schema. Mirrors the
    # Anthropic mapping of the same class name.
    "APIResponseValidationError": ErrorClass.INTERNAL_ERROR,
    # Auth-adjacent. OAuthError is a failed OAuth exchange;
    # SubjectTokenProviderError is a workload-identity token provider
    # failing to mint a token (the OpenAI analogue of Anthropic's
    # WorkloadIdentityError).
    "OAuthError": ErrorClass.INVALID_API_KEY,
    "SubjectTokenProviderError": ErrorClass.INVALID_API_KEY,
    # Realtime / WebSocket transport. A closed connection is a provider
    # availability signal; a full local queue means the consumer isn't
    # draining fast enough, which is caller-side.
    "WebSocketConnectionClosedError": ErrorClass.SERVICE_UNAVAILABLE,
    "WebSocketQueueFullError": ErrorClass.CLIENT_ERROR,
    # Client construction / usage errors — raised before or around the
    # network call, and always a caller-side bug rather than a provider
    # incident. MutuallyExclusiveAuthError and StreamAlreadyConsumed
    # mirror the Anthropic mappings of the same names.
    "MutuallyExclusiveAuthError": ErrorClass.CLIENT_ERROR,
    "StreamAlreadyConsumed": ErrorClass.CLIENT_ERROR,
    "APIRemovedInV1": ErrorClass.CLIENT_ERROR,
    "_AmbiguousModuleClientUsageError": ErrorClass.CLIENT_ERROR,
    # Structured-output helpers raise these when the completion stopped
    # for a reason the caller has to handle: the safety filter tripped,
    # or max_tokens truncated the response. Both are caller-side
    # (prompt or config), not provider incidents.
    "ContentFilterFinishReasonError": ErrorClass.CLIENT_ERROR,
    "LengthFinishReasonError": ErrorClass.CLIENT_ERROR,
    # Base classes — caught last, treated as unknown rather than
    # falsely attributed to a specific bucket.
    "APIStatusError": ErrorClass.UNKNOWN,
    "APIError": ErrorClass.UNKNOWN,
    "OpenAIError": ErrorClass.UNKNOWN,
}


def classify_openai_exception(exc: BaseException) -> str:
    """Return the canonical :class:`ErrorClass` for an OpenAI SDK
    exception.

    Two-step classification:

      1. Look up the class name in the OpenAI exception map.
      2. If the result is RATE_LIMITED, probe the response body for
         ``error.code == "insufficient_quota"``: OpenAI overloads
         RateLimitError to cover both true rate-limit and billing-
         cap exhaustion. The QUOTA_EXHAUSTED bucket needs a different
         remediation (raise quota vs. back off), so the detector
         routes them separately.

    Falls back to UNKNOWN for anything not in the map.
    """
    base = _OPENAI_EXCEPTION_MAP.get(type(exc).__name__, ErrorClass.UNKNOWN)
    if base == ErrorClass.RATE_LIMITED:
        if _openai_indicates_insufficient_quota(exc):
            return ErrorClass.QUOTA_EXHAUSTED
    return base


# Mapping from Cohere Python SDK exception class names to canonical
# ErrorClass values. Targets cohere>=5.0 (ClientV2 / OpenAI-style
# messages). Older cohere<5 used a flatter hierarchy under
# ``cohere.error.*``; those exception class names are listed too so
# the mapping is backward-compatible.
#
# Cohere v5+ hierarchy (cohere.errors.*):
#   CohereError (base)
#   ├── ApiError
#   │   ├── BadRequestError              (400)
#   │   ├── UnauthorizedError            (401)
#   │   ├── ForbiddenError               (403)
#   │   ├── NotFoundError                (404)
#   │   ├── ClientClosedRequestError     (499)
#   │   ├── InternalServerError          (500)
#   │   ├── NotImplementedError          (501)
#   │   ├── ServiceUnavailableError      (503)
#   │   ├── GatewayTimeoutError          (504)
#   │   └── TooManyRequestsError         (429)
#   └── CohereConnectionError            (network)
_COHERE_EXCEPTION_MAP = {
    "TooManyRequestsError": ErrorClass.RATE_LIMITED,
    "GatewayTimeoutError": ErrorClass.TIMEOUT,
    "CohereConnectionError": ErrorClass.SERVICE_UNAVAILABLE,
    "ServiceUnavailableError": ErrorClass.SERVICE_UNAVAILABLE,
    "InternalServerError": ErrorClass.INTERNAL_ERROR,
    "NotImplementedError": ErrorClass.INTERNAL_ERROR,
    "UnauthorizedError": ErrorClass.INVALID_API_KEY,
    "ForbiddenError": ErrorClass.INVALID_API_KEY,
    "BadRequestError": ErrorClass.CLIENT_ERROR,
    "NotFoundError": ErrorClass.CLIENT_ERROR,
    "ClientClosedRequestError": ErrorClass.CLIENT_ERROR,
    # Base classes — caught last, treated as unknown rather than
    # falsely attributed to a specific bucket.
    "ApiError": ErrorClass.UNKNOWN,
    "CohereError": ErrorClass.UNKNOWN,
    "CohereAPIError": ErrorClass.UNKNOWN,  # legacy cohere<5 name
}


def classify_cohere_exception(exc: BaseException) -> str:
    """Return the canonical :class:`ErrorClass` for a Cohere SDK
    exception. Falls back to UNKNOWN for unmapped classes.
    """
    return _COHERE_EXCEPTION_MAP.get(type(exc).__name__, ErrorClass.UNKNOWN)


# Mapping from Google Generative AI Python SDK exception class names
# to canonical ErrorClass values. The google-generativeai package
# bubbles up exceptions from google.api_core.exceptions, which is the
# RPC-style exception hierarchy shared across all Google Cloud
# clients.
#
# Hierarchy (google.api_core.exceptions.*):
#   GoogleAPIError
#   └── GoogleAPICallError
#       ├── InvalidArgument            (400)
#       ├── FailedPrecondition         (400)
#       ├── OutOfRange                 (400)
#       ├── Unauthenticated            (401)
#       ├── PermissionDenied           (403)
#       ├── NotFound                   (404)
#       ├── Aborted                    (409)
#       ├── AlreadyExists              (409)
#       ├── ResourceExhausted          (429)   * also covers quota
#       ├── Cancelled                  (499)
#       ├── DataLoss                   (500)
#       ├── Unknown                    (500)
#       ├── InternalServerError        (500)
#       ├── NotImplemented             (501)
#       ├── ServiceUnavailable         (503)
#       └── DeadlineExceeded           (504)
#
# (*) ResourceExhausted is overloaded by Google to cover BOTH true
# rate-limiting AND quota-exhaust. The classifier probes the error
# message for the substring "quota" to route quota-exhaust to the
# QUOTA_EXHAUSTED bucket; everything else stays RATE_LIMITED.
_GEMINI_EXCEPTION_MAP = {
    "ResourceExhausted": ErrorClass.RATE_LIMITED,
    "DeadlineExceeded": ErrorClass.TIMEOUT,
    "RetryError": ErrorClass.TIMEOUT,
    "ServiceUnavailable": ErrorClass.SERVICE_UNAVAILABLE,
    "Cancelled": ErrorClass.SERVICE_UNAVAILABLE,
    "Aborted": ErrorClass.SERVICE_UNAVAILABLE,
    "InternalServerError": ErrorClass.INTERNAL_ERROR,
    "DataLoss": ErrorClass.INTERNAL_ERROR,
    "Unknown": ErrorClass.INTERNAL_ERROR,
    "NotImplemented": ErrorClass.INTERNAL_ERROR,
    "Unauthenticated": ErrorClass.INVALID_API_KEY,
    "PermissionDenied": ErrorClass.INVALID_API_KEY,
    "InvalidArgument": ErrorClass.CLIENT_ERROR,
    "FailedPrecondition": ErrorClass.CLIENT_ERROR,
    "OutOfRange": ErrorClass.CLIENT_ERROR,
    "NotFound": ErrorClass.CLIENT_ERROR,
    "AlreadyExists": ErrorClass.CLIENT_ERROR,
    # Base classes — caught last, UNKNOWN.
    "GoogleAPICallError": ErrorClass.UNKNOWN,
    "GoogleAPIError": ErrorClass.UNKNOWN,
}


def classify_gemini_exception(exc: BaseException) -> str:
    """Return the canonical :class:`ErrorClass` for a Google Gemini
    SDK exception.

    Two-step classification (parallel to OpenAI):

      1. Look up class name in _GEMINI_EXCEPTION_MAP.
      2. If result is RATE_LIMITED (ResourceExhausted), probe the
         exception message for the substring "quota": Google
         overloads ResourceExhausted to cover both true rate-limit
         and quota-exhaust. The QUOTA_EXHAUSTED bucket needs
         different remediation (raise quota vs. back off).

    Falls back to UNKNOWN for unmapped classes.
    """
    base = _GEMINI_EXCEPTION_MAP.get(type(exc).__name__, ErrorClass.UNKNOWN)
    if base == ErrorClass.RATE_LIMITED:
        msg = str(exc).lower()
        if "quota" in msg:
            return ErrorClass.QUOTA_EXHAUSTED
    return base


# Mapping from Ollama + httpx exception class names to canonical
# ErrorClass values. Keyed by name (string) for the same dependency-
# avoidance reason as the other provider maps: the mesedi SDK works
# even if neither `ollama` nor `httpx` is installed.
#
# Ollama's native exception surface:
#   ollama.RequestError    — malformed request before send
#   ollama.ResponseError   — HTTP error response (has .status_code).
#                            Bucketed by HTTP-code range in
#                            classify_ollama_exception below; NOT
#                            mapped here directly because the class
#                            name alone doesn't carry the 4xx vs 5xx
#                            distinction the detectors require.
#
# httpx transport errors (the layer ollama runs on top of):
#   ConnectError, ConnectTimeout → SERVICE_UNAVAILABLE
#     (Ollama server not running — the single most common
#     local-runtime failure)
#   TimeoutException, ReadTimeout → TIMEOUT
#     (model load took longer than the customer's HTTP timeout)
#
# Ollama is a local runtime: there is no API-key auth, no per-minute
# rate limiting, no billing-cap. So INVALID_API_KEY, RATE_LIMITED,
# and QUOTA_EXHAUSTED are absent from this map by design — those
# buckets stay quiet for Ollama-only projects, which is the correct
# empty-state for local inference.
_OLLAMA_EXCEPTION_MAP = {
    # Ollama-native (chat request layer)
    "RequestError": ErrorClass.CLIENT_ERROR,
    # httpx transport layer
    "ConnectError": ErrorClass.SERVICE_UNAVAILABLE,
    "ConnectTimeout": ErrorClass.SERVICE_UNAVAILABLE,
    "TimeoutException": ErrorClass.TIMEOUT,
    "ReadTimeout": ErrorClass.TIMEOUT,
}


def classify_ollama_exception(exc: BaseException) -> str:
    """Return the canonical :class:`ErrorClass` for an Ollama or
    httpx-transport exception raised inside an instrumented
    chat call.

    Two-step classification (parallel to OpenAI / Gemini):

      1. If the exception is an ``ollama.ResponseError`` (or anything
         exposing a ``status_code`` attribute), bucket by HTTP-code
         range:
           - 4xx → CLIENT_ERROR  (bad model name, malformed messages)
           - 503 → SERVICE_UNAVAILABLE  (Ollama server overloaded)
           - 5xx (other) → INTERNAL_ERROR  (CUDA OOM, model crash)
           - non-int / missing → UNKNOWN
      2. Otherwise, look up the class name in _OLLAMA_EXCEPTION_MAP
         (covers RequestError + the dominant httpx transport errors).
      3. Anything not in the map (including non-Ollama exceptions
         that somehow reached the instrumented path) → UNKNOWN.

    Falls back to UNKNOWN rather than misattributing to a specific
    bucket: same discipline as classify_anthropic / openai / cohere /
    gemini.
    """
    # Step 1: ResponseError / status-bearing exceptions.
    # Check class name first so we don't accidentally pick up some
    # other exception that happens to expose a status_code attribute.
    if type(exc).__name__ == "ResponseError":
        status = getattr(exc, "status_code", None)
        if not isinstance(status, int):
            return ErrorClass.UNKNOWN
        if 400 <= status < 500:
            return ErrorClass.CLIENT_ERROR
        if status == 503:
            return ErrorClass.SERVICE_UNAVAILABLE
        if 500 <= status < 600:
            return ErrorClass.INTERNAL_ERROR
        return ErrorClass.UNKNOWN

    # Step 2: simple class-name lookup. Uses membership test +
    # bracket indexing rather than dict.get() so the static audit's
    # N+1 heuristic does not false-positive on this pure dict
    # lookup.
    name = type(exc).__name__
    if name in _OLLAMA_EXCEPTION_MAP:
        return _OLLAMA_EXCEPTION_MAP[name]
    return ErrorClass.UNKNOWN


def _openai_indicates_insufficient_quota(exc: BaseException) -> bool:
    """Probe an OpenAI exception's body for the insufficient_quota
    error code that distinguishes a billing-cap from a true rate
    limit. Best-effort: any missing attr / unexpected shape returns
    False rather than crashing the classifier.

    OpenAI surfaces this as:

        exc.body == {"error": {"code": "insufficient_quota", ...}}

    or, on older SDK versions:

        exc.response.json()["error"]["code"] == "insufficient_quota"
    """
    body = getattr(exc, "body", None)
    if isinstance(body, dict):
        err = body.get("error")
        if isinstance(err, dict) and err.get("code") == "insufficient_quota":
            return True
    # Older SDK: response.json() — call only if available, ignore
    # parse failures so the SDK never crashes inside classification.
    response = getattr(exc, "response", None)
    if response is not None:
        try:
            payload = response.json()
        except Exception:
            return False
        if isinstance(payload, dict):
            err = payload.get("error")
            if isinstance(err, dict) and err.get("code") == "insufficient_quota":
                return True
    return False


def extract_http_status(exc: BaseException) -> Optional[int]:
    """Return the HTTP status code from an Anthropic SDK exception
    if one is exposed, otherwise None.

    The Anthropic SDK's :class:`APIStatusError` subclasses expose
    ``status_code``; the base :class:`APIError` and connection /
    timeout errors don't. We probe for the attribute and return it
    on best-effort.
    """
    status = getattr(exc, "status_code", None)
    if isinstance(status, int):
        return status
    return None


def extract_retry_after(exc: BaseException) -> Optional[int]:
    """Return the provider-recommended back-off window (seconds) from
    a provider SDK exception, or ``None`` when the provider did not
    supply one.

    Probes in order, returning the first successful parse:

      1. ``exc.retry_after`` attribute (some SDKs expose this directly
         as either int seconds or a datetime).
      2. The ``Retry-After`` HTTP header on ``exc.response.headers``
         (httpx.Headers, case-insensitive). The Anthropic, OpenAI,
         Cohere, and Gemini SDKs all surface ``.response`` on their
         status-error subclasses.

    Per RFC 7231, the Retry-After header value may be either:
      - A non-negative integer (delay-seconds), the common case.
      - An HTTP-date (``Wed, 21 Oct 2026 07:28:00 GMT``), the rare
        case used by some CDNs in front of providers.

    Both shapes are parsed. HTTP-date values that have already passed
    return 0 (back off but don't wait). Returns ``None`` on any parse
    failure so the caller can omit the field cleanly rather than
    shipping a corrupted value.
    """
    # Probe 1: direct attribute (Cohere v5+ exposes retry_after as a
    # datetime; some custom subclasses expose it as int seconds).
    direct = getattr(exc, "retry_after", None)
    parsed = _coerce_retry_after_value(direct)
    if parsed is not None:
        return parsed

    # Probe 2: Retry-After header on the underlying httpx response.
    response = getattr(exc, "response", None)
    headers = getattr(response, "headers", None)
    if headers is None:
        return None
    # httpx.Headers is case-insensitive; native dict / mapping in
    # tests is not, so probe both casings.
    header_value: Any = None
    try:
        header_value = headers["retry-after"]
    except (KeyError, TypeError):
        try:
            header_value = headers["Retry-After"]
        except (KeyError, TypeError):
            header_value = None
    if header_value is None:
        return None
    return _coerce_retry_after_value(header_value)


def _coerce_retry_after_value(raw: Any) -> Optional[int]:
    """Coerce a Retry-After value (int / numeric str / HTTP-date /
    datetime) to a non-negative integer second count. Returns None on
    any parse failure."""
    if raw is None:
        return None
    # int / float — return as int seconds (clamped at >= 0).
    if isinstance(raw, bool):
        # bool is a subclass of int in Python; reject explicitly.
        return None
    if isinstance(raw, int):
        return max(0, raw)
    if isinstance(raw, float):
        return max(0, int(raw))
    # datetime — compute seconds-until-date in UTC.
    try:
        import datetime as _dt
        if isinstance(raw, _dt.datetime):
            now = _dt.datetime.now(_dt.timezone.utc)
            target = raw if raw.tzinfo else raw.replace(tzinfo=_dt.timezone.utc)
            return max(0, int((target - now).total_seconds()))
    except Exception:
        # Defensive: any exception in datetime path falls through to
        # string parsing below rather than crashing the SDK.
        pass
    # string — try int, then HTTP-date.
    if isinstance(raw, (str, bytes)):
        s = raw.decode("ascii", errors="ignore") if isinstance(raw, bytes) else raw
        s = s.strip()
        if not s:
            return None
        # Numeric path first (the common case).
        try:
            n = int(s)
            return max(0, n)
        except ValueError:
            pass
        # HTTP-date path. parsedate_to_datetime returns a timezone-
        # aware datetime per RFC 7231.
        try:
            from email.utils import parsedate_to_datetime
            target = parsedate_to_datetime(s)
            if target.tzinfo is None:
                # Per RFC 7231 HTTP-date is always GMT; this branch
                # protects against legacy parsers returning naive
                # datetimes.
                import datetime as _dt
                target = target.replace(tzinfo=_dt.timezone.utc)
            import datetime as _dt
            now = _dt.datetime.now(_dt.timezone.utc)
            return max(0, int((target - now).total_seconds()))
        except (TypeError, ValueError):
            return None
    return None
