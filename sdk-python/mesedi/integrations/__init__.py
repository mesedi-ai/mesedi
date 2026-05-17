"""Framework adapter shims for Mesedi.

Each submodule here translates a third-party agent framework's callback
or hook surface into Mesedi telemetry. They are intentionally NOT
imported by ``mesedi/__init__.py`` so importing ``mesedi`` does not
require any of the frameworks to be installed.

Usage:

    from mesedi.integrations.langchain import MesediCallbackHandler

Each adapter is a thin shim: it relies on the existing ``@mesedi.wrap``
decorator to establish the execution boundary, and emits per-event
telemetry inside that execution using the same helpers a hand-written
``@mesedi.tool`` / ``emit_llm_call`` pair would use.
"""
