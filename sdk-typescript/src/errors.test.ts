/**
 * Unit tests for the canonical provider-error vocabulary.
 *
 * The mapping function is the canonical contract between the SDK
 * and the backend's provider_incident detector. If these tests
 * fail, the SDK + backend pair has drifted and the detector will
 * mis-cluster.
 */

import { describe, expect, test } from "vitest";

import {
  ErrorClass,
  PROVIDER_SIDE_ERROR_CLASSES,
  classifyAnthropicException,
  classifyCohereException,
  classifyGeminiException,
  classifyOpenAIException,
  extractHttpStatus,
  extractRetryAfter,
} from "./errors.js";

/** Build an error instance whose class name matches the given
 * Anthropic exception class. The classifier keys off
 * err.constructor.name so this gives full coverage without an
 * @anthropic-ai/sdk dependency. */
function fakeAnthropicError(name: string): Error {
  // Object.defineProperty on the constructor's `name` is the only
  // way to control what `.constructor.name` returns for a
  // dynamically-created class.
  const cls = class extends Error {};
  Object.defineProperty(cls, "name", { value: name });
  return new cls("test");
}

describe("classifyAnthropicException", () => {
  const cases: Array<[string, string]> = [
    ["RateLimitError", ErrorClass.RATE_LIMITED],
    ["APITimeoutError", ErrorClass.TIMEOUT],
    ["DeadlineExceededError", ErrorClass.TIMEOUT],
    ["APIConnectionError", ErrorClass.SERVICE_UNAVAILABLE],
    ["ServiceUnavailableError", ErrorClass.SERVICE_UNAVAILABLE],
    ["OverloadedError", ErrorClass.SERVICE_UNAVAILABLE],
    ["InternalServerError", ErrorClass.INTERNAL_ERROR],
    ["APIResponseValidationError", ErrorClass.INTERNAL_ERROR],
    ["APIWebhookValidationError", ErrorClass.INTERNAL_ERROR],
    ["AuthenticationError", ErrorClass.INVALID_API_KEY],
    ["PermissionDeniedError", ErrorClass.INVALID_API_KEY],
    ["BadRequestError", ErrorClass.CLIENT_ERROR],
    ["NotFoundError", ErrorClass.CLIENT_ERROR],
    ["ConflictError", ErrorClass.CLIENT_ERROR],
    ["RequestTooLargeError", ErrorClass.CLIENT_ERROR],
    ["UnprocessableEntityError", ErrorClass.CLIENT_ERROR],
    // Base classes — UNKNOWN by design.
    ["APIStatusError", ErrorClass.UNKNOWN],
    ["APIError", ErrorClass.UNKNOWN],
    ["AnthropicError", ErrorClass.UNKNOWN],
  ];
  test.each(cases)("%s -> %s", (name, expected) => {
    expect(classifyAnthropicException(fakeAnthropicError(name))).toBe(expected);
  });

  test("non-Error values fall back to UNKNOWN", () => {
    expect(classifyAnthropicException("a string")).toBe(ErrorClass.UNKNOWN);
    expect(classifyAnthropicException(null)).toBe(ErrorClass.UNKNOWN);
    expect(classifyAnthropicException(undefined)).toBe(ErrorClass.UNKNOWN);
  });

  test("unknown Error class falls back to UNKNOWN", () => {
    // Vanilla Error has constructor.name = "Error" which is not in
    // the map; should return UNKNOWN rather than mislabel.
    expect(classifyAnthropicException(new Error("boom"))).toBe(ErrorClass.UNKNOWN);
  });
});

describe("PROVIDER_SIDE_ERROR_CLASSES", () => {
  test("contains exactly the provider-side bucket", () => {
    // The backend's provider_incident detector uses the SAME
    // filter. Pinning the membership here makes any drift between
    // SDK and backend visible immediately.
    expect(new Set(PROVIDER_SIDE_ERROR_CLASSES)).toEqual(
      new Set([
        ErrorClass.RATE_LIMITED,
        ErrorClass.QUOTA_EXHAUSTED,
        ErrorClass.INTERNAL_ERROR,
        ErrorClass.SERVICE_UNAVAILABLE,
        ErrorClass.TIMEOUT,
      ]),
    );
  });

  test("customer-side classes excluded", () => {
    expect(PROVIDER_SIDE_ERROR_CLASSES.has(ErrorClass.INVALID_API_KEY)).toBe(false);
    expect(PROVIDER_SIDE_ERROR_CLASSES.has(ErrorClass.CLIENT_ERROR)).toBe(false);
    expect(PROVIDER_SIDE_ERROR_CLASSES.has(ErrorClass.UNKNOWN)).toBe(false);
  });
});

describe("extractHttpStatus", () => {
  test("reads `status` property from APIStatusError-shaped error", () => {
    const err = fakeAnthropicError("RateLimitError") as Error & { status?: number };
    err.status = 429;
    expect(extractHttpStatus(err)).toBe(429);
  });

  test("reads `status_code` fallback for older SDK shapes", () => {
    const err = fakeAnthropicError("RateLimitError") as Error & {
      status_code?: number;
    };
    err.status_code = 429;
    expect(extractHttpStatus(err)).toBe(429);
  });

  test("returns undefined when neither attr present", () => {
    expect(extractHttpStatus(fakeAnthropicError("APIConnectionError"))).toBeUndefined();
  });

  test("returns undefined for non-numeric status", () => {
    const err = fakeAnthropicError("RateLimitError") as Error & { status?: unknown };
    err.status = "429";
    expect(extractHttpStatus(err)).toBeUndefined();
  });

  test("returns undefined for non-object input", () => {
    expect(extractHttpStatus("a string")).toBeUndefined();
    expect(extractHttpStatus(null)).toBeUndefined();
    expect(extractHttpStatus(42)).toBeUndefined();
  });
});

describe("extractRetryAfter", () => {
  test("reads direct retryAfter attr (number)", () => {
    const err = fakeAnthropicError("RateLimitError") as Error & {
      retryAfter?: number;
    };
    err.retryAfter = 30;
    expect(extractRetryAfter(err)).toBe(30);
  });

  test("reads direct retry_after attr (snake_case fallback)", () => {
    const err = fakeAnthropicError("RateLimitError") as Error & {
      retry_after?: number;
    };
    err.retry_after = 15;
    expect(extractRetryAfter(err)).toBe(15);
  });

  test("floors fractional seconds", () => {
    const err = fakeAnthropicError("RateLimitError") as Error & {
      retryAfter?: number;
    };
    err.retryAfter = 30.7;
    expect(extractRetryAfter(err)).toBe(30);
  });

  test("computes seconds-until-Date for retryAfter Date attr", () => {
    const err = fakeAnthropicError("RateLimitError") as Error & {
      retryAfter?: Date;
    };
    err.retryAfter = new Date(Date.now() + 45_000);
    const result = extractRetryAfter(err);
    expect(result).toBeGreaterThanOrEqual(43);
    expect(result).toBeLessThanOrEqual(45);
  });

  test("reads Retry-After header from response.headers (plain object)", () => {
    const err = fakeAnthropicError("RateLimitError") as Error & {
      response?: { headers: Record<string, string> };
    };
    err.response = { headers: { "retry-after": "60" } };
    expect(extractRetryAfter(err)).toBe(60);
  });

  test("case-insensitive header lookup ('Retry-After')", () => {
    const err = fakeAnthropicError("RateLimitError") as Error & {
      response?: { headers: Record<string, string> };
    };
    err.response = { headers: { "Retry-After": "12" } };
    expect(extractRetryAfter(err)).toBe(12);
  });

  test("reads Retry-After from Fetch-style Headers (.get())", () => {
    const err = fakeAnthropicError("RateLimitError") as Error & {
      response?: { headers: Headers };
    };
    err.response = { headers: new Headers({ "retry-after": "20" }) };
    expect(extractRetryAfter(err)).toBe(20);
  });

  test("reads Retry-After from Map headers", () => {
    const err = fakeAnthropicError("RateLimitError") as Error & {
      response?: { headers: Map<string, string> };
    };
    err.response = { headers: new Map([["retry-after", "25"]]) };
    expect(extractRetryAfter(err)).toBe(25);
  });

  test("parses HTTP-date (RFC 7231) value to seconds-until", () => {
    const target = new Date(Date.now() + 90_000);
    const err = fakeAnthropicError("RateLimitError") as Error & {
      response?: { headers: Record<string, string> };
    };
    err.response = { headers: { "retry-after": target.toUTCString() } };
    const result = extractRetryAfter(err);
    expect(result).toBeGreaterThanOrEqual(88);
    expect(result).toBeLessThanOrEqual(90);
  });

  test("clamps past HTTP-date to 0 (not negative)", () => {
    const past = new Date(Date.now() - 60_000);
    const err = fakeAnthropicError("RateLimitError") as Error & {
      response?: { headers: Record<string, string> };
    };
    err.response = { headers: { "retry-after": past.toUTCString() } };
    expect(extractRetryAfter(err)).toBe(0);
  });

  test("returns undefined when no signal anywhere", () => {
    expect(extractRetryAfter(fakeAnthropicError("APIConnectionError"))).toBeUndefined();
  });

  test("returns undefined for malformed header value", () => {
    const err = fakeAnthropicError("RateLimitError") as Error & {
      response?: { headers: Record<string, string> };
    };
    err.response = { headers: { "retry-after": "soon-ish" } };
    expect(extractRetryAfter(err)).toBeUndefined();
  });

  test("rejects boolean attr (would otherwise coerce to 1)", () => {
    const err = fakeAnthropicError("RateLimitError") as Error & {
      retryAfter?: boolean;
    };
    err.retryAfter = true;
    expect(extractRetryAfter(err)).toBeUndefined();
  });

  test("clamps negative attr to 0", () => {
    const err = fakeAnthropicError("RateLimitError") as Error & {
      retryAfter?: number;
    };
    err.retryAfter = -5;
    expect(extractRetryAfter(err)).toBe(0);
  });

  test("falls back to top-level headers when no response.headers", () => {
    const err = fakeAnthropicError("RateLimitError") as Error & {
      headers?: Record<string, string>;
    };
    err.headers = { "retry-after": "9" };
    expect(extractRetryAfter(err)).toBe(9);
  });

  test("returns undefined for non-object input", () => {
    expect(extractRetryAfter("a string")).toBeUndefined();
    expect(extractRetryAfter(null)).toBeUndefined();
    expect(extractRetryAfter(42)).toBeUndefined();
  });
});

describe("classifyOpenAIException", () => {
  /** Build an error whose constructor.name matches the given
   * OpenAI exception class. Same trick as fakeAnthropicError. */
  function fakeOpenAIError(name: string): Error {
    const cls = class extends Error {};
    Object.defineProperty(cls, "name", { value: name });
    return new cls("test");
  }

  const cases: Array<[string, string]> = [
    ["RateLimitError", ErrorClass.RATE_LIMITED],
    ["APIConnectionTimeoutError", ErrorClass.TIMEOUT],
    ["APITimeoutError", ErrorClass.TIMEOUT],
    ["APIConnectionError", ErrorClass.SERVICE_UNAVAILABLE],
    ["InternalServerError", ErrorClass.INTERNAL_ERROR],
    ["AuthenticationError", ErrorClass.INVALID_API_KEY],
    ["PermissionDeniedError", ErrorClass.INVALID_API_KEY],
    ["BadRequestError", ErrorClass.CLIENT_ERROR],
    ["NotFoundError", ErrorClass.CLIENT_ERROR],
    ["ConflictError", ErrorClass.CLIENT_ERROR],
    ["UnprocessableEntityError", ErrorClass.CLIENT_ERROR],
    ["APIStatusError", ErrorClass.UNKNOWN],
    ["APIError", ErrorClass.UNKNOWN],
    ["OpenAIError", ErrorClass.UNKNOWN],
  ];
  test.each(cases)("%s -> %s", (name, expected) => {
    expect(classifyOpenAIException(fakeOpenAIError(name))).toBe(expected);
  });

  test("non-Error values fall back to UNKNOWN", () => {
    expect(classifyOpenAIException("a string")).toBe(ErrorClass.UNKNOWN);
    expect(classifyOpenAIException(null)).toBe(ErrorClass.UNKNOWN);
    expect(classifyOpenAIException(undefined)).toBe(ErrorClass.UNKNOWN);
  });

  test("RateLimitError with insufficient_quota body routes to QUOTA_EXHAUSTED", () => {
    const err = fakeOpenAIError("RateLimitError") as Error & { body?: unknown };
    err.body = { error: { code: "insufficient_quota", message: "quota exceeded" } };
    expect(classifyOpenAIException(err)).toBe(ErrorClass.QUOTA_EXHAUSTED);
  });

  test("RateLimitError with other error code stays RATE_LIMITED", () => {
    const err = fakeOpenAIError("RateLimitError") as Error & { body?: unknown };
    err.body = { error: { code: "rate_limit_exceeded" } };
    expect(classifyOpenAIException(err)).toBe(ErrorClass.RATE_LIMITED);
  });

  test("RateLimitError with no body falls through to RATE_LIMITED default", () => {
    expect(classifyOpenAIException(fakeOpenAIError("RateLimitError"))).toBe(
      ErrorClass.RATE_LIMITED,
    );
  });

  test("RateLimitError quota probe via response.json() fallback", () => {
    const err = fakeOpenAIError("RateLimitError") as Error & {
      response?: { json: () => unknown };
    };
    err.response = {
      json: () => ({ error: { code: "insufficient_quota" } }),
    };
    expect(classifyOpenAIException(err)).toBe(ErrorClass.QUOTA_EXHAUSTED);
  });

  test("response.json() throwing does not crash the classifier", () => {
    const err = fakeOpenAIError("RateLimitError") as Error & {
      response?: { json: () => unknown };
    };
    err.response = {
      json: () => {
        throw new Error("not json");
      },
    };
    expect(classifyOpenAIException(err)).toBe(ErrorClass.RATE_LIMITED);
  });
});

describe("classifyCohereException", () => {
  function fakeCohereError(name: string, message = "test"): Error {
    const cls = class extends Error {};
    Object.defineProperty(cls, "name", { value: name });
    return new cls(message);
  }
  const cases: Array<[string, string]> = [
    ["TooManyRequestsError", ErrorClass.RATE_LIMITED],
    ["GatewayTimeoutError", ErrorClass.TIMEOUT],
    ["CohereConnectionError", ErrorClass.SERVICE_UNAVAILABLE],
    ["ServiceUnavailableError", ErrorClass.SERVICE_UNAVAILABLE],
    ["InternalServerError", ErrorClass.INTERNAL_ERROR],
    ["NotImplementedError", ErrorClass.INTERNAL_ERROR],
    ["UnauthorizedError", ErrorClass.INVALID_API_KEY],
    ["ForbiddenError", ErrorClass.INVALID_API_KEY],
    ["BadRequestError", ErrorClass.CLIENT_ERROR],
    ["NotFoundError", ErrorClass.CLIENT_ERROR],
    ["ClientClosedRequestError", ErrorClass.CLIENT_ERROR],
    ["ApiError", ErrorClass.UNKNOWN],
    ["CohereError", ErrorClass.UNKNOWN],
    ["CohereAPIError", ErrorClass.UNKNOWN],
  ];
  test.each(cases)("%s -> %s", (name, expected) => {
    expect(classifyCohereException(fakeCohereError(name))).toBe(expected);
  });

  test("non-Error values fall back to UNKNOWN", () => {
    expect(classifyCohereException("a string")).toBe(ErrorClass.UNKNOWN);
    expect(classifyCohereException(null)).toBe(ErrorClass.UNKNOWN);
  });

  test("unknown Error class falls back to UNKNOWN", () => {
    expect(classifyCohereException(new Error("boom"))).toBe(ErrorClass.UNKNOWN);
  });
});

describe("classifyGeminiException", () => {
  function fakeGeminiError(name: string, message = "test"): Error {
    const cls = class extends Error {};
    Object.defineProperty(cls, "name", { value: name });
    return new cls(message);
  }
  const cases: Array<[string, string]> = [
    ["ResourceExhausted", ErrorClass.RATE_LIMITED],
    ["DeadlineExceeded", ErrorClass.TIMEOUT],
    ["RetryError", ErrorClass.TIMEOUT],
    ["ServiceUnavailable", ErrorClass.SERVICE_UNAVAILABLE],
    ["Cancelled", ErrorClass.SERVICE_UNAVAILABLE],
    ["Aborted", ErrorClass.SERVICE_UNAVAILABLE],
    ["InternalServerError", ErrorClass.INTERNAL_ERROR],
    ["DataLoss", ErrorClass.INTERNAL_ERROR],
    ["Unknown", ErrorClass.INTERNAL_ERROR],
    ["NotImplemented", ErrorClass.INTERNAL_ERROR],
    ["Unauthenticated", ErrorClass.INVALID_API_KEY],
    ["PermissionDenied", ErrorClass.INVALID_API_KEY],
    ["InvalidArgument", ErrorClass.CLIENT_ERROR],
    ["FailedPrecondition", ErrorClass.CLIENT_ERROR],
    ["OutOfRange", ErrorClass.CLIENT_ERROR],
    ["NotFound", ErrorClass.CLIENT_ERROR],
    ["AlreadyExists", ErrorClass.CLIENT_ERROR],
    ["GoogleAPICallError", ErrorClass.UNKNOWN],
    ["GoogleAPIError", ErrorClass.UNKNOWN],
  ];
  test.each(cases)("%s -> %s", (name, expected) => {
    expect(classifyGeminiException(fakeGeminiError(name))).toBe(expected);
  });

  test("ResourceExhausted with 'quota' in message routes to QUOTA_EXHAUSTED", () => {
    expect(
      classifyGeminiException(fakeGeminiError("ResourceExhausted", "429 Quota exceeded")),
    ).toBe(ErrorClass.QUOTA_EXHAUSTED);
  });

  test("ResourceExhausted without 'quota' stays RATE_LIMITED", () => {
    expect(
      classifyGeminiException(fakeGeminiError("ResourceExhausted", "429 too many requests")),
    ).toBe(ErrorClass.RATE_LIMITED);
  });

  test("Quota check is case-insensitive", () => {
    expect(
      classifyGeminiException(fakeGeminiError("ResourceExhausted", "QUOTA EXCEEDED")),
    ).toBe(ErrorClass.QUOTA_EXHAUSTED);
  });

  test("non-Error values fall back to UNKNOWN", () => {
    expect(classifyGeminiException("a string")).toBe(ErrorClass.UNKNOWN);
    expect(classifyGeminiException(null)).toBe(ErrorClass.UNKNOWN);
  });
});
