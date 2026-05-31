# CLI argument parsing + body resolution tests.
#
# Network-side behavior is covered by tests in test_mesedi_client.py;
# this file only exercises argparse + body decoding logic.

import io
import sys

import pytest

from mesedi_curl import cli


def test_version_flag(capsys):
    rc = cli.main(["--version"])
    captured = capsys.readouterr()
    assert "mesedi-curl" in captured.out
    assert rc == 0


def test_help_when_no_url(capsys):
    """Without a URL, we should print help and exit 2 (matches curl's
    behavior of exit 2 on usage error)."""
    rc = cli.main([])
    assert rc == 2


def test_parse_headers_drops_malformed():
    headers = cli._parse_headers(
        ["Authorization: Bearer abc", "no-colon-here", "X-Custom: yes"]
    )
    assert headers["Authorization"] == "Bearer abc"
    assert headers["X-Custom"] == "yes"
    assert "no-colon-here" not in headers


def test_parse_headers_strips_whitespace():
    headers = cli._parse_headers(["  Content-Type :  application/json  "])
    assert headers["Content-Type"] == "application/json"


def test_parse_headers_keeps_last_duplicate():
    """curl semantics: last header wins."""
    headers = cli._parse_headers(["X-Foo: a", "X-Foo: b"])
    assert headers["X-Foo"] == "b"


def test_has_header_ci():
    assert cli._has_header_ci({"Content-Type": "x"}, "content-type") is True
    assert cli._has_header_ci({"Content-Type": "x"}, "X-Other") is False


def test_resolve_body_with_data_raw():
    args = cli._build_parser().parse_args(
        ["--data-raw", "@notafile", "https://example.com"]
    )
    body = cli._resolve_body(args)
    assert body == b"@notafile"


def test_resolve_body_with_inline_data():
    args = cli._build_parser().parse_args(
        ["-d", '{"hello":"world"}', "https://example.com"]
    )
    body = cli._resolve_body(args)
    assert body == b'{"hello":"world"}'


def test_resolve_body_with_file_reference(tmp_path):
    f = tmp_path / "body.json"
    f.write_bytes(b'{"from":"file"}')
    args = cli._build_parser().parse_args(["-d", f"@{f}", "https://example.com"])
    body = cli._resolve_body(args)
    assert body == b'{"from":"file"}'


def test_resolve_body_with_stdin(monkeypatch):
    monkeypatch.setattr(sys, "stdin", _make_stdin(b'{"from":"stdin"}'))
    args = cli._build_parser().parse_args(["-d", "@-", "https://example.com"])
    body = cli._resolve_body(args)
    assert body == b'{"from":"stdin"}'


def test_resolve_body_no_data_returns_none():
    args = cli._build_parser().parse_args(["https://example.com"])
    body = cli._resolve_body(args)
    assert body is None


def _make_stdin(content: bytes):
    """Create a fake sys.stdin whose .buffer.read() returns content."""

    class _Buf:
        def __init__(self, c):
            self._c = c

        def read(self):
            return self._c

    class _Stdin:
        def __init__(self, c):
            self.buffer = _Buf(c)

    return _Stdin(content)
