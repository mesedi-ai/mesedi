/**
 * Canonical provider-error vocabulary for the Mesedi SDK.
 *
 * Every provider integration (anthropic_integration, future
 * openai_integration, cohere_integration, etc.) classifies its
 * native exception types into ONE of the values in ErrorClass. The
 * backend reads `error_class` from llm_call payloads and uses the
 * canonical vocabulary to cluster cross-provider signals — e.g.
 * when both Anthropic and OpenAI are rate-limiting at the same
 * time, the `provider_incident` detector sees them under the same
 * `rate_limited` bucket regardless of which exception class each
 * SDK raised.
 *
 * Adding a new provider:
 *   1. Map the provider's exception hierarchy to the closest match
 *      in ErrorClass. Use UNKNOWN for anything that cannot be
 *      attributed cleanly.
 *   2. Set payload.provider to a stable lowercase identifier
 *      ("anthropic", "openai", "cohere", "gemini").
 *   3. Include error_class and (when available) http_status on the
 *      failure-path llm_call event.
 *
 * The vocabulary is deliberately closed. Free-form error strings
 * would defeat the cross-provider clustering the detectors rely on.
 */

import {
  ERROR_CLASS_NAMES,
  PROVIDER_SIDE_ERROR_CLASS_VALUES,
} from "./_errorClassesGenerated.js";

/** Closed vocabulary of canonical provider-error classes. Values
 * come from `spec/error_classes.yaml` via the codegen-generated
 * `_errorClassesGenerated.ts`. Re-export here under the historical
 * `ErrorClass` name so all consumers keep working unchanged when a
 * new class is added to the spec.
 *
 * The vocabulary is deliberately closed. Free-form error strings
 * would defeat the cross-provider clustering the detectors rely on. */
export const ErrorClass = ERROR_CLASS_NAMES;

export type ErrorClassValue = (typeof ErrorClass)[keyof typeof ErrorClass];

/**
 * Frozen set of values that count as "provider-side" for the
 * provider_incident detector. Sourced from the spec so Python /
 * TypeScript / Go all see the SAME membership without manual sync.
 */
export const PROVIDER_SIDE_ERROR_CLASSES: ReadonlySet<ErrorClassValue> =
  PROVIDER_SIDE_ERROR_CLASS_VALUES as ReadonlySet<ErrorClassValue>;

/**
 * Mapping from Anthropic exception class names (string, not the
 * class object) to canonical ErrorClass values. Keyed by name to
 * avoid taking a hard runtime dependency on @anthropic-ai/sdk —
 * the mesedi SDK works even if the anthropic package is not
 * installed, and the mapping stays correct because exception class
 * names are stable across anthropic SDK versions.
 */
const ANTHROPIC_EXCEPTION_MAP: Record<string, ErrorClassValue> = {
  RateLimitError: ErrorClass.RATE_LIMITED,
  APITimeoutError: ErrorClass.TIMEOUT,
  DeadlineExceededError: ErrorClass.TIMEOUT,
  APIConnectionError: ErrorClass.SERVICE_UNAVAILABLE,
  ServiceUnavailableError: ErrorClass.SERVICE_UNAVAILABLE,
  OverloadedError: ErrorClass.SERVICE_UNAVAILABLE,
  InternalServerError: ErrorClass.INTERNAL_ERROR,
  APIResponseValidationError: ErrorClass.INTERNAL_ERROR,
  APIWebhookValidationError: ErrorClass.INTERNAL_ERROR,
  AuthenticationError: ErrorClass.INVALID_API_KEY,
  PermissionDeniedError: ErrorClass.INVALID_API_KEY,
  BadRequestError: ErrorClass.CLIENT_ERROR,
  NotFoundError: ErrorClass.CLIENT_ERROR,
  ConflictError: ErrorClass.CLIENT_ERROR,
  RequestTooLargeError: ErrorClass.CLIENT_ERROR,
  UnprocessableEntityError: ErrorClass.CLIENT_ERROR,
  // Client construction / configuration errors — raised before any
  // network call. Customer code bug, not a provider incident.
  MissingDependencyError: ErrorClass.CLIENT_ERROR,
  MutuallyExclusiveAuthError: ErrorClass.CLIENT_ERROR,
  StreamAlreadyConsumed: ErrorClass.CLIENT_ERROR,
  // Auth-adjacent: Bedrock / Vertex workload-identity probe failed.
  WorkloadIdentityError: ErrorClass.INVALID_API_KEY,
  // Internal SDK marker for transient failures the SDK's own retry
  // loop classifies as retryable. Safe default is SERVICE_UNAVAILABLE.
  RetryableError: ErrorClass.SERVICE_UNAVAILABLE,
  // Base classes — caught last, treated as unknown rather than
  // falsely attributed to a specific bucket.
  APIStatusError: ErrorClass.UNKNOWN,
  APIError: ErrorClass.UNKNOWN,
  AnthropicError: ErrorClass.UNKNOWN,
};

/**
 * Return the canonical ErrorClass for an Anthropic SDK exception.
 * Falls back to UNKNOWN for anything not in the map (including
 * non-Anthropic exceptions that somehow reached the instrumented
 * call path).
 */
export function classifyAnthropicException(err: unknown): ErrorClassValue {
  if (err instanceof Error && err.constructor.name) {
    return ANTHROPIC_EXCEPTION_MAP[err.constructor.name] ?? ErrorClass.UNKNOWN;
  }
  return ErrorClass.UNKNOWN;
}

/**
 * Mapping from OpenAI SDK exception class names to canonical
 * ErrorClass values. Keyed by name (string) for the same
 * dependency-avoidance reason as the Anthropic map: the mesedi
 * SDK works even if the openai package is not installed.
 *
 * OpenAI's hierarchy (openai>=4.0):
 *   OpenAIError
 *   └── APIError
 *       ├── APIConnectionError
 *       │   └── APIConnectionTimeoutError
 *       └── APIStatusError → subclasses for 4xx/5xx
 *
 * NOTE: TypeScript SDK uses `APIConnectionTimeoutError` (different
 * from Python's `APITimeoutError`). Both are listed below so the
 * mapping is stable regardless of which casing the runtime presents.
 *
 * RateLimitError is overloaded by OpenAI to cover both true rate
 * limiting AND insufficient_quota — classifyOpenAIException probes
 * the body to distinguish, returning QUOTA_EXHAUSTED for the latter.
 */
const OPENAI_EXCEPTION_MAP: Record<string, ErrorClassValue> = {
  RateLimitError: ErrorClass.RATE_LIMITED,
  APIConnectionTimeoutError: ErrorClass.TIMEOUT,
  APITimeoutError: ErrorClass.TIMEOUT,
  APIConnectionError: ErrorClass.SERVICE_UNAVAILABLE,
  InternalServerError: ErrorClass.INTERNAL_ERROR,
  AuthenticationError: ErrorClass.INVALID_API_KEY,
  PermissionDeniedError: ErrorClass.INVALID_API_KEY,
  BadRequestError: ErrorClass.CLIENT_ERROR,
  NotFoundError: ErrorClass.CLIENT_ERROR,
  ConflictError: ErrorClass.CLIENT_ERROR,
  UnprocessableEntityError: ErrorClass.CLIENT_ERROR,
  // Base classes — caught last, treated as unknown rather than
  // falsely attributed to a specific bucket.
  APIStatusError: ErrorClass.UNKNOWN,
  APIError: ErrorClass.UNKNOWN,
  OpenAIError: ErrorClass.UNKNOWN,
};

/**
 * Return the canonical ErrorClass for an OpenAI SDK exception.
 *
 * Two-step classification:
 *   1. Look up class name in OPENAI_EXCEPTION_MAP.
 *   2. If result is RATE_LIMITED, probe the body for
 *      `error.code === "insufficient_quota"` — OpenAI overloads
 *      RateLimitError to cover both true rate limit and billing-
 *      cap exhaustion. The QUOTA_EXHAUSTED bucket needs different
 *      remediation (raise quota vs. back off), so the detector
 *      routes them separately.
 *
 * Falls back to UNKNOWN for anything not in the map.
 */
export function classifyOpenAIException(err: unknown): ErrorClassValue {
  if (!(err instanceof Error) || !err.constructor.name) {
    return ErrorClass.UNKNOWN;
  }
  const base = OPENAI_EXCEPTION_MAP[err.constructor.name] ?? ErrorClass.UNKNOWN;
  if (base === ErrorClass.RATE_LIMITED && openaiIndicatesInsufficientQuota(err)) {
    return ErrorClass.QUOTA_EXHAUSTED;
  }
  return base;
}

/** Mapping from Cohere TypeScript SDK exception class names to
 * canonical ErrorClass values. cohere-ai>=7.x exposes typed errors
 * under cohere.errors.*; older versions use a flatter hierarchy.
 * Both are listed so the mapping is backward-compatible. */
const COHERE_EXCEPTION_MAP: Record<string, ErrorClassValue> = {
  TooManyRequestsError: ErrorClass.RATE_LIMITED,
  GatewayTimeoutError: ErrorClass.TIMEOUT,
  CohereConnectionError: ErrorClass.SERVICE_UNAVAILABLE,
  ServiceUnavailableError: ErrorClass.SERVICE_UNAVAILABLE,
  InternalServerError: ErrorClass.INTERNAL_ERROR,
  NotImplementedError: ErrorClass.INTERNAL_ERROR,
  UnauthorizedError: ErrorClass.INVALID_API_KEY,
  ForbiddenError: ErrorClass.INVALID_API_KEY,
  BadRequestError: ErrorClass.CLIENT_ERROR,
  NotFoundError: ErrorClass.CLIENT_ERROR,
  ClientClosedRequestError: ErrorClass.CLIENT_ERROR,
  ApiError: ErrorClass.UNKNOWN,
  CohereError: ErrorClass.UNKNOWN,
  CohereAPIError: ErrorClass.UNKNOWN,
};

/**
 * Return the canonical ErrorClass for a Cohere SDK exception.
 * Falls back to UNKNOWN for anything not in the map.
 */
export function classifyCohereException(err: unknown): ErrorClassValue {
  if (err instanceof Error && err.constructor.name) {
    return COHERE_EXCEPTION_MAP[err.constructor.name] ?? ErrorClass.UNKNOWN;
  }
  return ErrorClass.UNKNOWN;
}

/** Mapping from Google Gemini / google.api_core.exceptions class
 * names to canonical ErrorClass values. The @google/generative-ai
 * TS package surfaces errors with constructor.name matching the
 * RPC status (matching the Python google.api_core hierarchy). */
const GEMINI_EXCEPTION_MAP: Record<string, ErrorClassValue> = {
  ResourceExhausted: ErrorClass.RATE_LIMITED,
  DeadlineExceeded: ErrorClass.TIMEOUT,
  RetryError: ErrorClass.TIMEOUT,
  ServiceUnavailable: ErrorClass.SERVICE_UNAVAILABLE,
  Cancelled: ErrorClass.SERVICE_UNAVAILABLE,
  Aborted: ErrorClass.SERVICE_UNAVAILABLE,
  InternalServerError: ErrorClass.INTERNAL_ERROR,
  DataLoss: ErrorClass.INTERNAL_ERROR,
  Unknown: ErrorClass.INTERNAL_ERROR,
  NotImplemented: ErrorClass.INTERNAL_ERROR,
  Unauthenticated: ErrorClass.INVALID_API_KEY,
  PermissionDenied: ErrorClass.INVALID_API_KEY,
  InvalidArgument: ErrorClass.CLIENT_ERROR,
  FailedPrecondition: ErrorClass.CLIENT_ERROR,
  OutOfRange: ErrorClass.CLIENT_ERROR,
  NotFound: ErrorClass.CLIENT_ERROR,
  AlreadyExists: ErrorClass.CLIENT_ERROR,
  GoogleAPICallError: ErrorClass.UNKNOWN,
  GoogleAPIError: ErrorClass.UNKNOWN,
};

/**
 * Return the canonical ErrorClass for a Gemini SDK exception.
 *
 * Two-step classification:
 *   1. Look up class name in GEMINI_EXCEPTION_MAP.
 *   2. If result is RATE_LIMITED (ResourceExhausted), probe the
 *      exception message for substring "quota" — Google overloads
 *      ResourceExhausted to cover both true rate-limit and
 *      quota-exhaust.
 *
 * Falls back to UNKNOWN for unmapped classes.
 */
export function classifyGeminiException(err: unknown): ErrorClassValue {
  if (!(err instanceof Error) || !err.constructor.name) {
    return ErrorClass.UNKNOWN;
  }
  const base = GEMINI_EXCEPTION_MAP[err.constructor.name] ?? ErrorClass.UNKNOWN;
  if (base === ErrorClass.RATE_LIMITED) {
    const msg = (err.message ?? "").toLowerCase();
    if (msg.includes("quota")) return ErrorClass.QUOTA_EXHAUSTED;
  }
  return base;
}

/** Mapping from Ollama + httpx exception class names to canonical
 * ErrorClass values. twin of the Python _OLLAMA_EXCEPTION_MAP.
 *
 * Ollama is a local runtime: no API-key auth, no per-minute rate
 * limiting, no billing-cap. So INVALID_API_KEY, RATE_LIMITED, and
 * QUOTA_EXHAUSTED are absent from this map by design.
 *
 * ResponseError (Ollama-native HTTP error) carries .status_code and
 * is bucketed by HTTP-code range inside classifyOllamaException —
 * NOT mapped here because the class name alone doesn't carry the
 * 4xx-vs-5xx distinction the detectors require. */
const OLLAMA_EXCEPTION_MAP: Record<string, ErrorClassValue> = {
  // Ollama-native chat-request layer
  RequestError: ErrorClass.CLIENT_ERROR,
  // httpx transport layer — Ollama runs on top of httpx; the two
  // dominant local-runtime failures are 'server not running'
  // (ConnectError) and 'model load timed out' (TimeoutException).
  ConnectError: ErrorClass.SERVICE_UNAVAILABLE,
  ConnectTimeout: ErrorClass.SERVICE_UNAVAILABLE,
  TimeoutException: ErrorClass.TIMEOUT,
  ReadTimeout: ErrorClass.TIMEOUT,
};

/**
 * Return the canonical ErrorClass for an Ollama or httpx-transport
 * exception raised inside an instrumented chat call.
 *
 * Two-step classification:
 *   1. If the exception is an ollama.ResponseError, bucket by
 *      HTTP-code range from .status_code: 4xx → CLIENT_ERROR,
 *      503 → SERVICE_UNAVAILABLE, other 5xx → INTERNAL_ERROR,
 *      anything else → UNKNOWN.
 *   2. Otherwise, look up the class name in OLLAMA_EXCEPTION_MAP
 *      (covers RequestError + the dominant httpx transport errors).
 *   3. Anything not in the map → UNKNOWN. Falls back rather than
 *      misattributing to a specific bucket.
 */
export function classifyOllamaException(err: unknown): ErrorClassValue {
  if (!(err instanceof Error) || !err.constructor.name) {
    return ErrorClass.UNKNOWN;
  }
  // Step 1: ResponseError / status-bearing exceptions.
  if (err.constructor.name === "ResponseError") {
    const status = (err as Error & { status_code?: unknown }).status_code;
    if (typeof status !== "number" || !Number.isInteger(status)) {
      return ErrorClass.UNKNOWN;
    }
    if (status >= 400 && status < 500) return ErrorClass.CLIENT_ERROR;
    if (status === 503) return ErrorClass.SERVICE_UNAVAILABLE;
    if (status >= 500 && status < 600) return ErrorClass.INTERNAL_ERROR;
    return ErrorClass.UNKNOWN;
  }
  // Step 2: class-name lookup for the simple cases.
  return OLLAMA_EXCEPTION_MAP[err.constructor.name] ?? ErrorClass.UNKNOWN;
}

/**
 * Dispatch classification to the provider-specific error mapper. When
 * the provider string is one of the known lowercase values, calls that
 * provider's classifier. When the provider is unknown, tries each in
 * turn and returns the first non-UNKNOWN bucket; falls through to
 * UNKNOWN when nothing matches.
 *
 * Used by the framework adapters (LangChain, Mastra, Vercel AI SDK)
 * where the exception object is available. Mastra adapter, which only
 * has SpanErrorInfo.name as a string, uses `classifyByProviderName`
 * below instead.
 */
export function classifyByProvider(
  provider: string,
  err: unknown,
): ErrorClassValue {
  switch (provider) {
    case "anthropic":
      return classifyAnthropicException(err);
    case "openai":
      return classifyOpenAIException(err);
    case "cohere":
      return classifyCohereException(err);
    case "gemini":
    case "vertexai":
      return classifyGeminiException(err);
    case "ollama":
      return classifyOllamaException(err);
    default: {
      for (const fn of [
        classifyAnthropicException,
        classifyOpenAIException,
        classifyCohereException,
        classifyGeminiException,
        classifyOllamaException,
      ]) {
        const cls = fn(err);
        if (cls !== ErrorClass.UNKNOWN) return cls;
      }
      return ErrorClass.UNKNOWN;
    }
  }
}

/**
 * Variant of `classifyByProvider` that dispatches on an exception
 * class NAME (string) rather than an Error object. Mastra's
 * SpanErrorInfo carries `.name` as a bare string — the underlying
 * Error object is not preserved through the exporter chain. This
 * shim wraps the name in a synthetic Error so the same class-name
 * lookup path in the per-provider classifiers works unchanged.
 *
 * Returns UNKNOWN when `className` is falsy or unrecognized.
 */
export function classifyByProviderName(
  provider: string,
  className: string | undefined,
): ErrorClassValue {
  if (!className) return ErrorClass.UNKNOWN;
  class ShimError extends Error {}
  Object.defineProperty(ShimError, "name", { value: className });
  const shim = new ShimError();
  Object.defineProperty(shim.constructor, "name", { value: className });
  return classifyByProvider(provider, shim);
}

/** Probe an OpenAI exception's body for the insufficient_quota
 * error code that distinguishes a billing-cap from a true rate
 * limit. Best-effort; any missing attr / unexpected shape returns
 * false rather than throwing. */
function openaiIndicatesInsufficientQuota(err: Error): boolean {
  const obj = err as Error & {
    body?: unknown;
    response?: { json?: () => unknown };
  };
  // Probe 1: direct body attr (most common in current SDK).
  if (obj.body && typeof obj.body === "object") {
    const e = (obj.body as { error?: unknown }).error;
    if (
      e && typeof e === "object" &&
      (e as { code?: unknown }).code === "insufficient_quota"
    ) {
      return true;
    }
  }
  // Probe 2: older SDK exposes response.json() — call only if
  // available, swallow any parse exception.
  if (obj.response && typeof obj.response.json === "function") {
    try {
      const payload = obj.response.json();
      if (payload && typeof payload === "object") {
        const e = (payload as { error?: unknown }).error;
        if (
          e && typeof e === "object" &&
          (e as { code?: unknown }).code === "insufficient_quota"
        ) {
          return true;
        }
      }
    } catch {
      return false;
    }
  }
  return false;
}

/**
 * Return the HTTP status code from an Anthropic SDK exception if
 * one is exposed, otherwise undefined. APIStatusError subclasses
 * expose `status`; base APIError and connection/timeout errors
 * don't.
 */
export function extractHttpStatus(err: unknown): number | undefined {
  if (err && typeof err === "object" && "status" in err) {
    const status = (err as { status?: unknown }).status;
    if (typeof status === "number") return status;
  }
  // Some Anthropic SDK versions expose status_code instead.
  if (err && typeof err === "object" && "status_code" in err) {
    const status = (err as { status_code?: unknown }).status_code;
    if (typeof status === "number") return status;
  }
  return undefined;
}

/**
 * Return the provider-recommended back-off window (seconds) from a
 * provider SDK exception, or undefined when the provider did not
 * supply one.
 *
 * Probes in order, returning the first successful parse:
 *
 *   1. `err.retryAfter` / `err.retry_after` attribute (some SDKs
 *      expose this directly).
 *   2. The `Retry-After` HTTP header on `err.response.headers` /
 *      `err.headers`. Anthropic, OpenAI, Cohere, and Gemini SDKs
 *      all surface response headers on their status-error subclasses.
 *
 * Per RFC 7231, the Retry-After header value may be either:
 *   - A non-negative integer (delay-seconds), the common case.
 *   - An HTTP-date (`Wed, 21 Oct 2026 07:28:00 GMT`), the rare case
 *     used by some CDNs in front of providers.
 *
 * Both shapes are parsed. HTTP-date values that have already passed
 * return 0 (back off but don't wait). Returns undefined on any parse
 * failure so the caller can omit the field cleanly rather than
 * shipping a corrupted value.
 */
export function extractRetryAfter(err: unknown): number | undefined {
  if (!err || typeof err !== "object") return undefined;
  const obj = err as Record<string, unknown>;

  // Probe 1: direct attribute (camelCase OR snake_case).
  const direct = obj.retryAfter ?? obj.retry_after;
  const fromDirect = coerceRetryAfterValue(direct);
  if (fromDirect !== undefined) return fromDirect;

  // Probe 2: Retry-After response header.
  const response = obj.response;
  let headers: unknown = undefined;
  if (response && typeof response === "object") {
    headers = (response as Record<string, unknown>).headers;
  }
  if (headers === undefined) {
    headers = obj.headers;
  }
  if (!headers) return undefined;

  const headerValue = readHeader(headers, "retry-after");
  return coerceRetryAfterValue(headerValue);
}

/** Best-effort read of one header value across the shapes provider
 * SDKs hand us: Fetch-style Headers (has .get()), plain object,
 * Map. All header lookups are case-insensitive per HTTP spec. */
function readHeader(headers: unknown, name: string): unknown {
  const lower = name.toLowerCase();
  // Fetch Headers / undici Headers: .get() is case-insensitive.
  if (
    typeof (headers as { get?: unknown }).get === "function"
  ) {
    try {
      return (headers as { get: (k: string) => string | null }).get(lower);
    } catch {
      // fall through to map/object probes
    }
  }
  // Map.
  if (headers instanceof Map) {
    for (const [k, v] of headers) {
      if (typeof k === "string" && k.toLowerCase() === lower) return v;
    }
    return undefined;
  }
  // Plain object — walk keys for case-insensitive match.
  if (typeof headers === "object" && headers !== null) {
    for (const [k, v] of Object.entries(headers as Record<string, unknown>)) {
      if (k.toLowerCase() === lower) return v;
    }
  }
  return undefined;
}

/** Coerce a Retry-After value (number / numeric string / HTTP-date /
 * Date) to a non-negative integer second count. Returns undefined
 * on any parse failure. */
function coerceRetryAfterValue(raw: unknown): number | undefined {
  if (raw === null || raw === undefined) return undefined;
  if (typeof raw === "boolean") return undefined; // reject bool→number coercion
  if (typeof raw === "number") {
    if (!Number.isFinite(raw)) return undefined;
    return Math.max(0, Math.floor(raw));
  }
  if (raw instanceof Date) {
    const delta = (raw.getTime() - Date.now()) / 1000;
    if (!Number.isFinite(delta)) return undefined;
    return Math.max(0, Math.floor(delta));
  }
  if (typeof raw === "string") {
    const s = raw.trim();
    if (s === "") return undefined;
    // Numeric path first (the common case).
    if (/^-?\d+(\.\d+)?$/.test(s)) {
      const n = Number(s);
      if (Number.isFinite(n)) return Math.max(0, Math.floor(n));
      return undefined;
    }
    // HTTP-date path. Date.parse returns NaN on failure.
    const ts = Date.parse(s);
    if (Number.isNaN(ts)) return undefined;
    return Math.max(0, Math.floor((ts - Date.now()) / 1000));
  }
  return undefined;
}
