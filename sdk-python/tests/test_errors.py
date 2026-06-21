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
    extract_http_status,
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
