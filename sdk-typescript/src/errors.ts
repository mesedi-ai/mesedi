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

/** Closed vocabulary of canonical provider-error classes. */
export const ErrorClass = {
  /** Provider says you're going too fast. HTTP 429 in most APIs. */
  RATE_LIMITED: "rate_limited",
  /** Billing/quota cap hit. Distinct from RATE_LIMITED because
   * remediation differs (raise quota vs. back off). */
  QUOTA_EXHAUSTED: "quota_exhausted",
  /** Provider had an internal 5xx failure or returned a malformed
   * response that fails the SDK's own response validation. */
  INTERNAL_ERROR: "internal_error",
  /** Provider unreachable, overloaded, or circuit-open. Connection
   * failures and explicit "service unavailable" responses both
   * map here. */
  SERVICE_UNAVAILABLE: "service_unavailable",
  /** Provider took longer than its configured timeout. Includes
   * deadline-exceeded variants. */
  TIMEOUT: "timeout",
  /** Auth rejection — bad/expired/revoked API key or insufficient
   * permissions. Customer-side, reported consistently so the
   * dashboard can surface it. */
  INVALID_API_KEY: "invalid_api_key",
  /** 4xx request validation failure (bad request, not-found,
   * conflict, payload-too-large, unprocessable entity). Customer
   * bug — the backend's provider_incident detector filters this
   * class out. */
  CLIENT_ERROR: "client_error",
  /** Couldn't classify. Falls through to UNKNOWN rather than
   * mislabeling. The backend treats UNKNOWN as non-clusterable for
   * provider_incident purposes. */
  UNKNOWN: "unknown",
} as const;

export type ErrorClassValue = (typeof ErrorClass)[keyof typeof ErrorClass];

/**
 * Frozen set of values that count as "provider-side" for the
 * provider_incident detector. The backend uses the SAME filter on
 * its side; keeping the canonical list in the SDK lets future
 * integrations stay consistent.
 */
export const PROVIDER_SIDE_ERROR_CLASSES: ReadonlySet<ErrorClassValue> = new Set([
  ErrorClass.RATE_LIMITED,
  ErrorClass.QUOTA_EXHAUSTED,
  ErrorClass.INTERNAL_ERROR,
  ErrorClass.SERVICE_UNAVAILABLE,
  ErrorClass.TIMEOUT,
]);

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
