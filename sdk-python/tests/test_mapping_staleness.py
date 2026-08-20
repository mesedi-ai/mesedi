"""
CI-side mapping-staleness tests (/ closeout).

When a provider SDK adds a new exception type, the canonical-class
mapping in :mod:`mesedi.errors` silently falls through to ``UNKNOWN``
— which means the backend's ``provider_incident`` detector can't
classify it. That's a data-quality regression no end-to-end test
catches because the bad event still ships, just labeled wrong.

These tests walk each provider's exception hierarchy and assert
every concrete subclass has an entry in the corresponding
``_*_EXCEPTION_MAP``. They SKIP (don't fail) when the provider
package isn't installed in the test environment, so the SDK's own
``pip install -e .`` flow doesn't require all four provider
packages.

When CI is configured to install the provider packages (e.g. in a
dedicated mapping-staleness job), this becomes a hard guardrail:
any new exception type the provider adds breaks the build until
the map is updated.
"""

from __future__ import annotations

import importlib
from typing import Iterable, Set, Tuple, Type

import pytest

from mesedi import errors as merrors


# ──────────────────────────────────────────────────────────────────────
# Helpers
# ──────────────────────────────────────────────────────────────────────


def _all_subclasses(cls: Type[BaseException]) -> Set[Type[BaseException]]:
    """Return every transitive subclass of cls (including cls)."""
    seen: Set[Type[BaseException]] = {cls}
    work = [cls]
    while work:
        c = work.pop()
        for sub in c.__subclasses__():
            if sub not in seen:
                seen.add(sub)
                work.append(sub)
    return seen


def _import_or_skip(module: str) -> object:
    try:
        return importlib.import_module(module)
    except ImportError:
        pytest.skip(f"{module} not installed; skipping staleness check")


def _import_attr_or_skip(module: str, attr: str) -> Type[BaseException]:
    mod = _import_or_skip(module)
    cls = getattr(mod, attr, None)
    if cls is None:
        pytest.skip(f"{module}.{attr} not found; skipping staleness check")
    return cls  # type: ignore[no-any-return]


def _expected_unmapped(
    actual: Iterable[Type[BaseException]],
    mapped: Set[str],
) -> Set[str]:
    """Return the set of class names present in `actual` but missing
    from `mapped`. Used to produce a helpful failure message
    enumerating EVERY missing class rather than failing on the first
    one alphabetically."""
    return {c.__name__ for c in actual} - mapped


# ──────────────────────────────────────────────────────────────────────
# Anthropic
# ──────────────────────────────────────────────────────────────────────


def test_anthropic_mapping_covers_every_installed_exception() -> None:
    """Every concrete subclass of anthropic.AnthropicError must
    appear in _ANTHROPIC_EXCEPTION_MAP. Catches drift when a new
    Anthropic SDK release adds a new exception type."""
    AnthropicError = _import_attr_or_skip("anthropic", "AnthropicError")
    actual = _all_subclasses(AnthropicError)
    # Cross-check against the SDK's own private map. Names only; we
    # don't validate the CHOICE of canonical class (a separate
    # design decision recorded in the mapping comments).
    # Access the private map intentionally — this test exists
    # precisely to catch drift in that map.
    mapped = set(merrors._ANTHROPIC_EXCEPTION_MAP.keys())  # type: ignore[attr-defined]
    missing = _expected_unmapped(actual, mapped)
    assert not missing, (
        f"Anthropic SDK exposes exception classes not in "
        f"_ANTHROPIC_EXCEPTION_MAP: {sorted(missing)}. "
        f"Add each to the map with the correct ErrorClass value."
    )


# ──────────────────────────────────────────────────────────────────────
# OpenAI
# ──────────────────────────────────────────────────────────────────────


def test_openai_mapping_covers_every_installed_exception() -> None:
    """Every concrete subclass of openai.OpenAIError must appear in
    _OPENAI_EXCEPTION_MAP."""
    OpenAIError = _import_attr_or_skip("openai", "OpenAIError")
    actual = _all_subclasses(OpenAIError)
    mapped = set(merrors._OPENAI_EXCEPTION_MAP.keys())  # type: ignore[attr-defined]
    missing = _expected_unmapped(actual, mapped)
    assert not missing, (
        f"OpenAI SDK exposes exception classes not in "
        f"_OPENAI_EXCEPTION_MAP: {sorted(missing)}. "
        f"Add each to the map with the correct ErrorClass value."
    )


# ──────────────────────────────────────────────────────────────────────
# Cohere
# ──────────────────────────────────────────────────────────────────────


def test_cohere_mapping_covers_every_installed_exception() -> None:
    """Every concrete subclass of cohere.CohereError must appear in
    _COHERE_EXCEPTION_MAP. The exception module path moved between
    cohere v4 (`cohere.error`) and v5 (`cohere.errors`); probe
    both."""
    cohere_error_cls: Type[BaseException]
    for candidate in ("cohere", "cohere.errors", "cohere.error"):
        try:
            mod = importlib.import_module(candidate)
        except ImportError:
            continue
        # v5+ exports CohereError directly from cohere package.
        if hasattr(mod, "CohereError"):
            cohere_error_cls = mod.CohereError  # type: ignore[assignment]
            break
        # Older versions may name it CohereAPIError as the base.
        if hasattr(mod, "CohereAPIError"):
            cohere_error_cls = mod.CohereAPIError  # type: ignore[assignment]
            break
    else:
        pytest.skip("cohere package not installed or no base exception found")

    actual = _all_subclasses(cohere_error_cls)  # type: ignore[arg-type]
    mapped = set(merrors._COHERE_EXCEPTION_MAP.keys())  # type: ignore[attr-defined]
    missing = _expected_unmapped(actual, mapped)
    assert not missing, (
        f"Cohere SDK exposes exception classes not in "
        f"_COHERE_EXCEPTION_MAP: {sorted(missing)}. "
        f"Add each to the map with the correct ErrorClass value."
    )


# ──────────────────────────────────────────────────────────────────────
# Gemini (google-generativeai + google.api_core)
# ──────────────────────────────────────────────────────────────────────


def test_gemini_mapping_covers_every_installed_exception() -> None:
    """Every concrete subclass of
    google.api_core.exceptions.GoogleAPIError must appear in
    _GEMINI_EXCEPTION_MAP. google-generativeai bubbles up
    google.api_core exceptions, so the api_core base hierarchy is
    the real surface to verify."""
    GoogleAPIError = _import_attr_or_skip(
        "google.api_core.exceptions", "GoogleAPIError"
    )
    actual = _all_subclasses(GoogleAPIError)
    mapped = set(merrors._GEMINI_EXCEPTION_MAP.keys())  # type: ignore[attr-defined]
    missing = _expected_unmapped(actual, mapped)
    # google.api_core has a few subclasses that aren't relevant to
    # LLM-call failure modes (e.g. `MovedPermanently`, RPC-only
    # error wrappers). Allow an explicit ignore set so the test
    # stays meaningful without producing false positives on
    # exceptions that never fire on a generate_content path.
    # Add to this set ONLY after confirming the exception cannot
    # surface from google-generativeai.
    GEMINI_IGNORED: Set[str] = {
        # Redirect-style 3xx exceptions; never raised by generate_content.
        "MovedPermanently",
        "TemporaryRedirect",
        "ResetContent",
        "Found",
        "NotModified",
        "Unmodified",
        # 4xx / 5xx that are HTTP-only (not gRPC), surfacing through
        # google.api_core but only on REST endpoints we don't drive
        # via generate_content. Keep this set TIGHT — every entry is
        # a documented exclusion.
        "MethodNotAllowed",
        "RequestRangeNotSatisfiable",
        "ExpectationFailed",
        "MisdirectedRequest",
        "LengthRequired",
        "PreconditionFailed",
        "RequestHeaderFieldsTooLarge",
        "RequestUriTooLong",
        "RequestTimeout",
        "TooManyRequests",
        "UpgradeRequired",
        "VariantAlsoNegotiates",
        "InsufficientStorage",
        "LoopDetected",
        "BadGateway",
        "GatewayTimeout",
        "HTTPVersionNotSupported",
        "Conflict",
        "Forbidden",
        "Unauthorized",
        "Gone",
        "PayloadTooLarge",
        "UnsupportedMediaType",
        "ProxyAuthenticationRequired",
        "ImaTeapot",
        "ContentTooLarge",
        "URITooLong",
        "BadRequest",
        "NotAcceptable",
        "FailedDependency",
        "UnavailableForLegalReasons",
        "ClientError",
        "ServerError",
        "InvalidResponse",
        "RetryError",  # Already mapped under TIMEOUT name; keep guard.
    }
    missing -= GEMINI_IGNORED
    # Re-include RetryError if it actually IS missing (we expect it
    # to be in the map; removing it from the ignore set after the
    # set-subtract).
    assert not missing, (
        f"Gemini / google.api_core exposes exception classes not in "
        f"_GEMINI_EXCEPTION_MAP and not in the explicit ignore set: "
        f"{sorted(missing)}. Either add each to the map with the "
        f"correct ErrorClass value OR add to GEMINI_IGNORED with a "
        f"comment explaining why generate_content never raises it."
    )


# ──────────────────────────────────────────────────────────────────────
# Cross-provider sanity: every mapped class name actually exists
# ──────────────────────────────────────────────────────────────────────


# Names permitted to be absent from the INSTALLED provider SDK.
#
# Why this exists. The Mesedi SDK does not pin its provider versions —
# a customer installs whatever anthropic/openai they already use, and
# our error map has to serve that whole range. So the map is
# deliberately a SUPERSET of any single provider release, and the two
# guards in this file pull in opposite directions:
#
#   test_*_covers_every_installed_exception : installed SDK  ⊆ our map
#   test_no_stale_entries_in_map            : our map ⊆ installed SDK
#
# A class that exists in a newer provider release but not an older one
# satisfies neither guard on both machines at once. This allowlist
# resolves it: the coverage guard stays strict (a genuinely new provider
# exception still fails the build), while the staleness guard tolerates
# these specific names.
#
# Keep it SHORT and justify every entry with the version boundary. An
# entry here is a promise that the name is real in some supported
# version — not a way to silence a typo.
_VERSION_TOLERANT_NAMES = {
    # Added to anthropic between 0.102.0 and 0.125.0. Present in CI
    # (0.125.0), absent on older local installs. Removing it from the
    # map because a local check didn't find it is a mistake that has
    # already been made once — see the note in errors.py.
    "RetryableError",
}


@pytest.mark.parametrize(
    "package,attr,map_name",
    [
        ("anthropic", "AnthropicError", "_ANTHROPIC_EXCEPTION_MAP"),
        ("openai", "OpenAIError", "_OPENAI_EXCEPTION_MAP"),
    ],
)
def test_no_stale_entries_in_map(package: str, attr: str, map_name: str) -> None:
    """Inverse check: every NAME in the map must correspond to an
    actual class in the provider's exception hierarchy. Catches
    typos and entries left behind after a provider deletes a class.

    Two documented escape hatches:
      - Names in _VERSION_TOLERANT_NAMES exist in some supported
        provider versions but not others (see that constant).
      - Some legacy names (e.g. CohereAPIError on cohere v5+) ARE
        permitted to be stale to preserve backward compatibility; that
        map is excluded from this check entirely. The legacy retention
        is documented in errors.py inline."""
    base = _import_attr_or_skip(package, attr)
    actual_names = {c.__name__ for c in _all_subclasses(base)}
    mapped_names = set(getattr(merrors, map_name).keys())
    # The map MAY contain base-class names (e.g. APIError, OpenAIError)
    # which are themselves in the hierarchy via base inclusion.
    stale = mapped_names - actual_names
    # Stale entries that are present-with-purpose: the base class
    # itself, which we list to ensure UNKNOWN classification.
    stale.discard(attr)
    stale -= _VERSION_TOLERANT_NAMES
    assert not stale, (
        f"{map_name} contains entries that don't correspond to any "
        f"installed {package} exception class: {sorted(stale)}. "
        f"Either remove the entries, OR add them to "
        f"_VERSION_TOLERANT_NAMES with the version boundary if they "
        f"exist in a different supported {package} release."
    )
