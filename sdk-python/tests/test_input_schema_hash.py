"""input_schema_hash: the declared-definition capture for the
definition-drift detector (the mcp-pin class).

Follows test_tool_description_decorated.py's discipline: the
decorator tests go through the public @tool and read what was
actually submitted, and the swap test changes the schema AFTER
decoration, because a definition swapped between calls is the entire
scenario this field exists to catch.
"""

from __future__ import annotations

import sys

import pytest

import mesedi  # noqa: F401
from mesedi._context import ExecutionContext
from mesedi._jcs import input_schema_hash

tool_mod = sys.modules["mesedi.tool"]
observe_mod = sys.modules["mesedi.observe"]
mcp_mod = sys.modules["mesedi.mcp"]


class _CapturedClient:
    def __init__(self) -> None:
        self.events: list = []

    def submit_event(self, event) -> None:
        self.events.append(event)


@pytest.fixture()
def captured(monkeypatch: pytest.MonkeyPatch) -> _CapturedClient:
    cap = _CapturedClient()
    for mod in (tool_mod, observe_mod, mcp_mod):
        monkeypatch.setattr(mod, "get_client", lambda: cap)
        monkeypatch.setattr(
            mod,
            "current_execution_context",
            lambda: ExecutionContext(execution_id="exec-test"),
        )
    return cap


def _last_payload(cap: _CapturedClient) -> dict:
    assert cap.events, "no event was submitted"
    return cap.events[-1].payload


# ── the canonical hash itself ─────────────────────────────────────

# The same constant is pinned in the TypeScript SDK's
# input_schema_hash.test.ts. If either SDK's canonicalization drifts,
# one of the two suites goes red; a project mixing SDKs must never
# see phantom definition drift from SDK disagreement.
CROSS_SDK_SCHEMA = {
    "type": "object",
    "properties": {"city": {"type": "string"}},
    "required": ["city"],
}
CROSS_SDK_EXPECTED_HASH = (
    "be45ab5b6d9f7d0b36dea2e7e355170888fb49d00124420f9e6f76c5d5620fc4"
)


def test_cross_sdk_fixture_hash_is_pinned():
    assert input_schema_hash(CROSS_SDK_SCHEMA) == CROSS_SDK_EXPECTED_HASH


def test_hash_is_deterministic_and_key_order_invariant():
    a = {"type": "object", "properties": {"city": {"type": "string"}}}
    b = {"properties": {"city": {"type": "string"}}, "type": "object"}
    assert input_schema_hash(a) == input_schema_hash(b)
    assert len(input_schema_hash(a)) == 64


def test_different_schemas_hash_differently():
    a = {"type": "object", "properties": {"city": {"type": "string"}}}
    b = {"type": "object", "properties": {"city": {"type": "number"}}}
    assert input_schema_hash(a) != input_schema_hash(b)


def test_none_and_unserializable_yield_empty():
    assert input_schema_hash(None) == ""
    assert input_schema_hash({"bad": object()}) == ""


def test_unicode_keys_do_not_depend_on_ascii_escaping():
    # ensure_ascii=False is load-bearing: the TypeScript SDK never
    # escapes non-ASCII, so Python must not either or the two SDKs
    # would hash the same schema differently.
    schema = {"описание": "ciudad"}
    assert input_schema_hash(schema) == input_schema_hash(dict(schema))


# ── the attribute reader, directly ────────────────────────────────

def test_input_schema_hash_reader_precedence_and_absence():
    from mesedi.tool import _input_schema_hash

    def bare():
        return None

    assert _input_schema_hash(bare) == ""

    bare.input_schema = {"type": "object"}
    assert _input_schema_hash(bare) == input_schema_hash({"type": "object"})

    class Model:
        @staticmethod
        def model_json_schema():
            return {"type": "object", "properties": {}}

    def with_model():
        return None

    with_model.args_schema = Model
    assert _input_schema_hash(with_model) == input_schema_hash(
        Model.model_json_schema()
    )


# ── through the decorator ─────────────────────────────────────────

def test_plain_function_sends_no_schema_hash(captured):
    @tool_mod.tool
    def lookup(city: str) -> dict:
        """A tool with no declared schema."""
        return {"city": city}

    lookup("melbourne")
    assert "input_schema_hash" not in _last_payload(captured)


def test_explicit_input_schema_attribute_is_hashed(captured):
    @tool_mod.tool
    def lookup(city: str) -> dict:
        return {"city": city}

    schema = {"type": "object", "properties": {"city": {"type": "string"}}}
    lookup.input_schema = schema
    lookup("melbourne")
    assert _last_payload(captured)["input_schema_hash"] == input_schema_hash(schema)


def test_schema_swapped_after_decoration_is_visible(captured):
    """THE REGRESSION TEST, definition edition: the hash must track
    the object the caller holds, at call time, so a swap between
    calls changes what goes over the wire."""

    @tool_mod.tool
    def lookup(city: str) -> dict:
        return {"city": city}

    v1 = {"type": "object", "properties": {"city": {"type": "string"}}}
    v2 = {"type": "object", "properties": {"city": {"type": "string"},
                                           "ssn": {"type": "string"}}}
    lookup.input_schema = v1
    lookup("melbourne")
    first = _last_payload(captured)["input_schema_hash"]
    lookup.input_schema = v2
    lookup("melbourne")
    second = _last_payload(captured)["input_schema_hash"]
    assert first == input_schema_hash(v1)
    assert second == input_schema_hash(v2)
    assert first != second


def test_pydantic_style_args_schema_is_hashed(captured):
    class FakeArgsModel:
        @staticmethod
        def model_json_schema() -> dict:  # pydantic v2 surface
            return {"type": "object", "properties": {"q": {"type": "string"}}}

    @tool_mod.tool
    def search(q: str) -> dict:
        return {"q": q}

    search.args_schema = FakeArgsModel
    search("observability")
    assert _last_payload(captured)["input_schema_hash"] == input_schema_hash(
        FakeArgsModel.model_json_schema()
    )


# ── through emit_mcp_call ─────────────────────────────────────────

def test_emit_mcp_call_hashes_declared_schema(captured):
    schema = {"type": "object", "properties": {"path": {"type": "string"}}}
    observe_mod.emit_mcp_call(
        server_name="filesystem", method="read_file",
        arguments={"path": "/etc/motd"}, input_schema=schema,
    )
    assert _last_payload(captured)["input_schema_hash"] == input_schema_hash(schema)


def test_emit_mcp_call_without_schema_omits_field(captured):
    observe_mod.emit_mcp_call(server_name="filesystem", method="read_file")
    assert "input_schema_hash" not in _last_payload(captured)
