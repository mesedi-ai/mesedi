"""
Unit tests for the canonical provider-error vocabulary.

The mapping function is the canonical contract between the SDK and
the backend's provider_incident detector. If these tests fail, the
SDK + backend pair has drifted and the detector will mis-cluster.
"""

from __future__ import annotations

import pytest

from mesedi.errors import (
    ErrorClass,
    PROVIDER_SIDE_ERROR_CLASSES,
    classify_anthropic_exception,
    classify_cohere_exception,
    classify_gemini_exception,
    classify_ollama_exception,
    classify_openai_exception,
    extract_http_status,
    extract_retry_after,
)


# ──────────────────────────────────────────────────────────────────────
# classify_anthropic_exception
# ──────────────────────────────────────────────────────────────────────


class _FakeExc(Exception):
    """Subclassable stand-in so tests can pretend to be any Anthropic
    exception class without needing the anthropic package installed."""


def _fake(name: str) -> Exception:
    """Build an exception instance whose class name matches the
    given Anthropic exception class. The classifier keys off
    type(exc).__name__ so this gives full coverage without an
    anthropic dependency."""
    cls = type(name, (_FakeExc,), {})
    return cls("test")


@pytest.mark.parametrize(
    "exception_name,expected",
    [
        # rate / quota
        ("RateLimitError", ErrorClass.RATE_LIMITED),
        # timeouts
        ("APITimeoutError", ErrorClass.TIMEOUT),
        ("DeadlineExceededError", ErrorClass.TIMEOUT),
        # service unavailable / connection
        ("APIConnectionError", ErrorClass.SERVICE_UNAVAILABLE),
        ("ServiceUnavailableError", ErrorClass.SERVICE_UNAVAILABLE),
        ("OverloadedError", ErrorClass.SERVICE_UNAVAILABLE),
        # internal
        ("InternalServerError", ErrorClass.INTERNAL_ERROR),
        ("APIResponseValidationError", ErrorClass.INTERNAL_ERROR),
        ("APIWebhookValidationError", ErrorClass.INTERNAL_ERROR),
        # auth
        ("AuthenticationError", ErrorClass.INVALID_API_KEY),
        ("PermissionDeniedError", ErrorClass.INVALID_API_KEY),
        # client errors
        ("BadRequestError", ErrorClass.CLIENT_ERROR),
        ("NotFoundError", ErrorClass.CLIENT_ERROR),
        ("ConflictError", ErrorClass.CLIENT_ERROR),
        ("RequestTooLargeError", ErrorClass.CLIENT_ERROR),
        ("UnprocessableEntityError", ErrorClass.CLIENT_ERROR),
        # base classes — UNKNOWN by design
        ("APIStatusError", ErrorClass.UNKNOWN),
        ("APIError", ErrorClass.UNKNOWN),
        ("AnthropicError", ErrorClass.UNKNOWN),
    ],
)
def test_classify_anthropic_exception_mapping(
    exception_name: str, expected: str
) -> None:
    assert classify_anthropic_exception(_fake(exception_name)) == expected


def test_classify_unknown_exception_falls_back_to_unknown() -> None:
    # Non-Anthropic exception that somehow reached the classifier.
    assert classify_anthropic_exception(ValueError("boom")) == ErrorClass.UNKNOWN


# ──────────────────────────────────────────────────────────────────────
# PROVIDER_SIDE_ERROR_CLASSES filter
# ──────────────────────────────────────────────────────────────────────


def test_provider_side_filter_contents() -> None:
    """The backend's provider_incident detector uses the SAME filter.
    Pinning the membership here makes any drift between SDK and
    backend visible immediately."""
    assert PROVIDER_SIDE_ERROR_CLASSES == frozenset(
        {
            ErrorClass.RATE_LIMITED,
            ErrorClass.QUOTA_EXHAUSTED,
            ErrorClass.INTERNAL_ERROR,
            ErrorClass.SERVICE_UNAVAILABLE,
            ErrorClass.TIMEOUT,
        }
    )


@pytest.mark.parametrize(
    "non_provider_class",
    [ErrorClass.INVALID_API_KEY, ErrorClass.CLIENT_ERROR, ErrorClass.UNKNOWN],
)
def test_customer_side_classes_not_in_provider_filter(non_provider_class: str) -> None:
    assert non_provider_class not in PROVIDER_SIDE_ERROR_CLASSES


# ──────────────────────────────────────────────────────────────────────
# extract_http_status
# ──────────────────────────────────────────────────────────────────────


def test_extract_http_status_from_status_code_attr() -> None:
    exc = _fake("RateLimitError")
    exc.status_code = 429  # Anthropic APIStatusError shape
    assert extract_http_status(exc) == 429


def test_extract_http_status_returns_none_when_attr_absent() -> None:
    # APIConnectionError, APITimeoutError — no status_code attr.
    assert extract_http_status(_fake("APIConnectionError")) is None


def test_extract_http_status_returns_none_for_non_int() -> None:
    exc = _fake("RateLimitError")
    exc.status_code = "429"  # malformed (string instead of int)
    assert extract_http_status(exc) is None


# ──────────────────────────────────────────────────────────────────────
# extract_retry_after
# ──────────────────────────────────────────────────────────────────────


def test_retry_after_from_direct_int_attr() -> None:
    """Some custom subclasses expose retry_after as int seconds."""
    exc = _fake("RateLimitError")
    exc.retry_after = 30
    assert extract_retry_after(exc) == 30


def test_retry_after_from_direct_float_attr() -> None:
    exc = _fake("RateLimitError")
    exc.retry_after = 30.7
    assert extract_retry_after(exc) == 30  # floor


def test_retry_after_from_direct_datetime_attr() -> None:
    """Cohere v5+ exposes retry_after as a datetime in the future."""
    import datetime as dt
    exc = _fake("RateLimitError")
    target = dt.datetime.now(dt.timezone.utc) + dt.timedelta(seconds=45)
    exc.retry_after = target
    result = extract_retry_after(exc)
    assert result is not None
    # Allow 2s of clock drift for test runtime.
    assert 43 <= result <= 45


def test_retry_after_from_response_header_int_string() -> None:
    """Most common path: Retry-After header as integer-seconds string."""
    exc = _fake("RateLimitError")
    # Dict mimics httpx.Headers — we probe with case-insensitive lookup.
    exc.response = type("R", (), {"headers": {"retry-after": "60"}})()
    assert extract_retry_after(exc) == 60


def test_retry_after_from_response_header_capitalized() -> None:
    """Case-insensitive: 'Retry-After' (RFC casing) also resolves."""
    exc = _fake("RateLimitError")
    exc.response = type("R", (), {"headers": {"Retry-After": "12"}})()
    assert extract_retry_after(exc) == 12


def test_retry_after_from_http_date_header() -> None:
    """RFC 7231: Retry-After may be an HTTP-date instead of seconds."""
    import datetime as dt
    from email.utils import format_datetime
    target = dt.datetime.now(dt.timezone.utc) + dt.timedelta(seconds=90)
    exc = _fake("RateLimitError")
    exc.response = type(
        "R", (), {"headers": {"retry-after": format_datetime(target, usegmt=True)}}
    )()
    result = extract_retry_after(exc)
    assert result is not None
    # Allow 2s of clock drift.
    assert 88 <= result <= 90


def test_retry_after_returns_zero_for_past_date() -> None:
    """An HTTP-date in the past clamps to 0, not negative."""
    import datetime as dt
    from email.utils import format_datetime
    past = dt.datetime.now(dt.timezone.utc) - dt.timedelta(minutes=5)
    exc = _fake("RateLimitError")
    exc.response = type(
        "R", (), {"headers": {"retry-after": format_datetime(past, usegmt=True)}}
    )()
    assert extract_retry_after(exc) == 0


def test_retry_after_returns_none_when_no_signal() -> None:
    """Connection / timeout errors typically have no Retry-After at all."""
    exc = _fake("APIConnectionError")
    assert extract_retry_after(exc) is None


def test_retry_after_returns_none_for_malformed_string() -> None:
    exc = _fake("RateLimitError")
    exc.response = type("R", (), {"headers": {"retry-after": "soon-ish"}})()
    assert extract_retry_after(exc) is None


def test_retry_after_rejects_bool_attr() -> None:
    """bool is a subclass of int in Python; explicit guard required."""
    exc = _fake("RateLimitError")
    exc.retry_after = True
    assert extract_retry_after(exc) is None


def test_retry_after_clamps_negative_to_zero() -> None:
    exc = _fake("RateLimitError")
    exc.retry_after = -5
    assert extract_retry_after(exc) == 0


def test_retry_after_handles_missing_response_attr() -> None:
    """Common case for non-status errors."""
    exc = _fake("APITimeoutError")
    # No response attr at all.
    assert extract_retry_after(exc) is None


def test_retry_after_handles_response_without_headers() -> None:
    exc = _fake("RateLimitError")
    exc.response = type("R", (), {})()  # no headers attr
    assert extract_retry_after(exc) is None


# ──────────────────────────────────────────────────────────────────────
# classify_openai_exception
# ──────────────────────────────────────────────────────────────────────


@pytest.mark.parametrize(
    "class_name,expected",
    [
        ("RateLimitError", ErrorClass.RATE_LIMITED),
        ("APITimeoutError", ErrorClass.TIMEOUT),
        ("APIConnectionError", ErrorClass.SERVICE_UNAVAILABLE),
        ("InternalServerError", ErrorClass.INTERNAL_ERROR),
        ("AuthenticationError", ErrorClass.INVALID_API_KEY),
        ("PermissionDeniedError", ErrorClass.INVALID_API_KEY),
        ("BadRequestError", ErrorClass.CLIENT_ERROR),
        ("NotFoundError", ErrorClass.CLIENT_ERROR),
        ("ConflictError", ErrorClass.CLIENT_ERROR),
        ("UnprocessableEntityError", ErrorClass.CLIENT_ERROR),
        ("APIStatusError", ErrorClass.UNKNOWN),
        ("APIError", ErrorClass.UNKNOWN),
        ("OpenAIError", ErrorClass.UNKNOWN),
    ],
)
def test_classify_openai_exception_mapping(class_name: str, expected: str) -> None:
    """Every OpenAI exception class maps to its canonical bucket."""
    assert classify_openai_exception(_fake(class_name)) == expected


def test_classify_openai_unknown_exception_falls_back_to_unknown() -> None:
    """Non-OpenAI exception classes reach UNKNOWN."""
    assert classify_openai_exception(ValueError("not openai")) == ErrorClass.UNKNOWN


def test_openai_rate_limit_with_insufficient_quota_body_routes_to_quota_exhausted() -> None:
    """OpenAI overloads RateLimitError to also cover billing-cap
    exhaustion. The classifier must distinguish the two by probing
    the body for error.code == 'insufficient_quota'."""
    exc = _fake("RateLimitError")
    exc.body = {"error": {"code": "insufficient_quota", "message": "You exceeded your current quota"}}
    assert classify_openai_exception(exc) == ErrorClass.QUOTA_EXHAUSTED


def test_openai_rate_limit_with_other_error_code_stays_rate_limited() -> None:
    """A non-quota error code (e.g. true rate limit) keeps the
    default classification."""
    exc = _fake("RateLimitError")
    exc.body = {"error": {"code": "rate_limit_exceeded", "message": "Too many requests"}}
    assert classify_openai_exception(exc) == ErrorClass.RATE_LIMITED


def test_openai_rate_limit_with_no_body_stays_rate_limited() -> None:
    """Missing body falls through to default RATE_LIMITED."""
    exc = _fake("RateLimitError")
    # No body attr at all.
    assert classify_openai_exception(exc) == ErrorClass.RATE_LIMITED


def test_openai_quota_probe_via_response_json_fallback() -> None:
    """Older OpenAI SDK versions don't expose .body but do expose
    .response.json(). The classifier must probe both paths."""
    exc = _fake("RateLimitError")
    class _Resp:
        def json(self):
            return {"error": {"code": "insufficient_quota"}}
    exc.response = _Resp()
    assert classify_openai_exception(exc) == ErrorClass.QUOTA_EXHAUSTED


def test_openai_quota_probe_survives_response_json_exception() -> None:
    """If response.json() raises, the classifier must not crash —
    just fall through to default classification."""
    exc = _fake("RateLimitError")
    class _Resp:
        def json(self):
            raise ValueError("not json")
    exc.response = _Resp()
    assert classify_openai_exception(exc) == ErrorClass.RATE_LIMITED


def test_openai_extract_http_status_matches_anthropic_path() -> None:
    """OpenAI APIStatusError exposes status_code the same way
    Anthropic does, so the shared extract_http_status works on both."""
    exc = _fake("RateLimitError")
    exc.status_code = 429
    assert extract_http_status(exc) == 429


def test_openai_extract_retry_after_matches_anthropic_path() -> None:
    """extract_retry_after is provider-agnostic — same code path for
    OpenAI as for Anthropic. Smoke-test the integration."""
    exc = _fake("RateLimitError")
    exc.response = type("R", (), {"headers": {"retry-after": "42"}})()
    assert extract_retry_after(exc) == 42


# ──────────────────────────────────────────────────────────────────────
# classify_cohere_exception
# ──────────────────────────────────────────────────────────────────────


@pytest.mark.parametrize(
    "class_name,expected",
    [
        ("TooManyRequestsError", ErrorClass.RATE_LIMITED),
        ("GatewayTimeoutError", ErrorClass.TIMEOUT),
        ("CohereConnectionError", ErrorClass.SERVICE_UNAVAILABLE),
        ("ServiceUnavailableError", ErrorClass.SERVICE_UNAVAILABLE),
        ("InternalServerError", ErrorClass.INTERNAL_ERROR),
        ("NotImplementedError", ErrorClass.INTERNAL_ERROR),
        ("UnauthorizedError", ErrorClass.INVALID_API_KEY),
        ("ForbiddenError", ErrorClass.INVALID_API_KEY),
        ("BadRequestError", ErrorClass.CLIENT_ERROR),
        ("NotFoundError", ErrorClass.CLIENT_ERROR),
        ("ClientClosedRequestError", ErrorClass.CLIENT_ERROR),
        ("ApiError", ErrorClass.UNKNOWN),
        ("CohereError", ErrorClass.UNKNOWN),
        ("CohereAPIError", ErrorClass.UNKNOWN),
    ],
)
def test_classify_cohere_exception_mapping(class_name: str, expected: str) -> None:
    assert classify_cohere_exception(_fake(class_name)) == expected


def test_classify_cohere_unknown_exception_falls_back_to_unknown() -> None:
    assert classify_cohere_exception(ValueError("not cohere")) == ErrorClass.UNKNOWN


# ──────────────────────────────────────────────────────────────────────
# classify_gemini_exception
# ──────────────────────────────────────────────────────────────────────


@pytest.mark.parametrize(
    "class_name,expected",
    [
        ("ResourceExhausted", ErrorClass.RATE_LIMITED),
        ("DeadlineExceeded", ErrorClass.TIMEOUT),
        ("RetryError", ErrorClass.TIMEOUT),
        ("ServiceUnavailable", ErrorClass.SERVICE_UNAVAILABLE),
        ("Cancelled", ErrorClass.SERVICE_UNAVAILABLE),
        ("Aborted", ErrorClass.SERVICE_UNAVAILABLE),
        ("InternalServerError", ErrorClass.INTERNAL_ERROR),
        ("DataLoss", ErrorClass.INTERNAL_ERROR),
        ("Unknown", ErrorClass.INTERNAL_ERROR),
        ("NotImplemented", ErrorClass.INTERNAL_ERROR),
        ("Unauthenticated", ErrorClass.INVALID_API_KEY),
        ("PermissionDenied", ErrorClass.INVALID_API_KEY),
        ("InvalidArgument", ErrorClass.CLIENT_ERROR),
        ("FailedPrecondition", ErrorClass.CLIENT_ERROR),
        ("OutOfRange", ErrorClass.CLIENT_ERROR),
        ("NotFound", ErrorClass.CLIENT_ERROR),
        ("AlreadyExists", ErrorClass.CLIENT_ERROR),
        ("GoogleAPICallError", ErrorClass.UNKNOWN),
        ("GoogleAPIError", ErrorClass.UNKNOWN),
    ],
)
def test_classify_gemini_exception_mapping(class_name: str, expected: str) -> None:
    assert classify_gemini_exception(_fake(class_name)) == expected


def test_gemini_resource_exhausted_with_quota_message_routes_to_quota_exhausted() -> None:
    """Google overloads ResourceExhausted to cover quota exhaustion.
    The classifier probes the message for 'quota' substring."""
    cls = type("ResourceExhausted", (Exception,), {})
    exc = cls("429 Quota exceeded for project foo")
    assert classify_gemini_exception(exc) == ErrorClass.QUOTA_EXHAUSTED


def test_gemini_resource_exhausted_without_quota_message_stays_rate_limited() -> None:
    cls = type("ResourceExhausted", (Exception,), {})
    exc = cls("429 too many requests per minute")
    assert classify_gemini_exception(exc) == ErrorClass.RATE_LIMITED


def test_gemini_quota_case_insensitive() -> None:
    """Probe is case-insensitive so 'QUOTA' / 'Quota' / 'quota' all match."""
    cls = type("ResourceExhausted", (Exception,), {})
    exc = cls("QUOTA EXCEEDED")
    assert classify_gemini_exception(exc) == ErrorClass.QUOTA_EXHAUSTED


def test_classify_gemini_unknown_exception_falls_back_to_unknown() -> None:
    assert classify_gemini_exception(ValueError("not gemini")) == ErrorClass.UNKNOWN


# ──────────────────────────────────────────────────────────────────────
# classify_ollama_exception (Wave 2.5.2)
# ──────────────────────────────────────────────────────────────────────


def _fake_response_error(status_code) -> Exception:
    """Stand-in for ollama.ResponseError. The real class exposes
    .status_code; we mimic the shape without depending on the
    ollama package."""
    cls = type("ResponseError", (Exception,), {})
    exc = cls("HTTP error")
    exc.status_code = status_code
    return exc


@pytest.mark.parametrize(
    "status,expected",
    [
        # 4xx → CLIENT_ERROR (customer's fault: bad model name,
        # malformed messages, missing required field)
        (400, ErrorClass.CLIENT_ERROR),
        (404, ErrorClass.CLIENT_ERROR),  # model not pulled
        (422, ErrorClass.CLIENT_ERROR),
        (499, ErrorClass.CLIENT_ERROR),
        # 503 → SERVICE_UNAVAILABLE (Ollama server overloaded)
        (503, ErrorClass.SERVICE_UNAVAILABLE),
        # Other 5xx → INTERNAL_ERROR (CUDA OOM, model crash, etc.)
        (500, ErrorClass.INTERNAL_ERROR),
        (502, ErrorClass.INTERNAL_ERROR),
        (504, ErrorClass.INTERNAL_ERROR),
        (599, ErrorClass.INTERNAL_ERROR),
    ],
)
def test_classify_ollama_response_error_bucketed_by_status_code(
    status: int, expected: str,
) -> None:
    """ResponseError carries a status_code; the classifier buckets it
    by HTTP-code range so detectors can distinguish customer-fault
    from server-fault from overload."""
    assert classify_ollama_exception(_fake_response_error(status)) == expected


def test_classify_ollama_response_error_with_no_status_code_falls_back_to_unknown() -> None:
    """Defensive: a malformed ResponseError without a status_code
    attribute returns UNKNOWN rather than crashing the classifier."""
    cls = type("ResponseError", (Exception,), {})
    exc = cls("missing status")
    assert classify_ollama_exception(exc) == ErrorClass.UNKNOWN


def test_classify_ollama_response_error_with_non_int_status_falls_back_to_unknown() -> None:
    """Defensive: a string status_code (from a misbehaving proxy)
    returns UNKNOWN rather than misclassifying."""
    assert classify_ollama_exception(_fake_response_error("503")) == ErrorClass.UNKNOWN


def test_classify_ollama_response_error_with_status_out_of_http_range() -> None:
    """Status codes outside the standard HTTP range (< 400 or >= 600)
    fall through to UNKNOWN. The classifier is conservative."""
    assert classify_ollama_exception(_fake_response_error(200)) == ErrorClass.UNKNOWN
    assert classify_ollama_exception(_fake_response_error(600)) == ErrorClass.UNKNOWN


@pytest.mark.parametrize(
    "class_name,expected",
    [
        # Ollama-native request layer
        ("RequestError", ErrorClass.CLIENT_ERROR),
        # httpx transport layer — the two dominant failures for
        # local-runtime customers
        ("ConnectError", ErrorClass.SERVICE_UNAVAILABLE),
        ("ConnectTimeout", ErrorClass.SERVICE_UNAVAILABLE),
        ("TimeoutException", ErrorClass.TIMEOUT),
        ("ReadTimeout", ErrorClass.TIMEOUT),
    ],
)
def test_classify_ollama_transport_layer_mapping(
    class_name: str, expected: str,
) -> None:
    """Wave 2.5.2 1B: the dominant local-runtime failure for Ollama
    customers is 'I forgot to start ollama serve' — an httpx
    ConnectError. Mapping it means infrastructure_throttled and
    provider_incident get useful signal exactly when customers most
    need diagnostic help."""
    assert classify_ollama_exception(_fake(class_name)) == expected


def test_classify_ollama_unknown_exception_falls_back_to_unknown() -> None:
    """Non-Ollama / non-httpx exceptions (a customer's own ValueError
    bubbling up through the integration) reach UNKNOWN rather than
    being misattributed."""
    assert classify_ollama_exception(ValueError("not ollama")) == ErrorClass.UNKNOWN


def test_ollama_classification_does_NOT_map_to_rate_limited() -> None:
    """Ollama is a local runtime: no per-minute rate limiting, no
    quota. The classifier must NEVER return RATE_LIMITED or
    QUOTA_EXHAUSTED for any Ollama exception — those buckets stay
    quiet for Ollama-only projects, which is the correct empty-state
    for local inference. This regression guard fails loudly if a
    future refactor accidentally adds those mappings."""
    # Sweep across every possible mapping and assert none returns
    # RATE_LIMITED / QUOTA_EXHAUSTED / INVALID_API_KEY (also absent
    # for the same local-runtime reason).
    forbidden = {
        ErrorClass.RATE_LIMITED,
        ErrorClass.QUOTA_EXHAUSTED,
        ErrorClass.INVALID_API_KEY,
    }
    for status in [400, 401, 403, 404, 429, 500, 503, 504]:
        result = classify_ollama_exception(_fake_response_error(status))
        assert result not in forbidden, (
            f"status_code {status} mapped to {result!r}; Ollama is local-"
            f"runtime and must not classify into {forbidden!r}"
        )
    for class_name in [
        "RequestError", "ConnectError", "ConnectTimeout",
        "TimeoutException", "ReadTimeout",
    ]:
        result = classify_ollama_exception(_fake(class_name))
        assert result not in forbidden, (
            f"{class_name} mapped to {result!r}; Ollama is local-"
            f"runtime and must not classify into {forbidden!r}"
        )
