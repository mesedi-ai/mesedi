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
  extractHttpStatus,
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
