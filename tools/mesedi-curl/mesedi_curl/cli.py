# Command-line entry point.
#
# Parses a curl-compatible subset of arguments, performs the HTTP
# request via `requests` (with streaming pass-through), captures the
# call, and forwards a summary to the Mesedi backend.
#
# The argument parser is deliberately a curl-compatible SUBSET, not a
# clone. We cover the flags that show up in 99% of LLM-API calls in
# the wild. Unsupported flags raise a usage error rather than silently
# being dropped, which would lead to confusing differences between
# `curl` and `mesedi-curl` behavior.
#
# Supported flags (matches curl semantics):
#   -X / --request METHOD              HTTP method
#   -H / --header "Name: value"        Request header (repeatable)
#   -d / --data DATA                   Request body (JSON by default if @file)
#   --data-raw DATA                    Same as -d but no @file interpretation
#   --data-binary DATA                 Same as -d, no encoding mangling
#   -o / --output FILE                 Write body to FILE instead of stdout
#   -s / --silent                      Don't print progress meter
#   -i / --include                     Include response headers in output
#   -L / --location                    Follow redirects
#   -k / --insecure                    Skip TLS verification
#   --connect-timeout SECONDS          Connect timeout
#   --max-time SECONDS                 Overall timeout
#   --version                          Print mesedi-curl version
#   -h / --help                        Show help
#
# Plus the positional URL argument.
from __future__ import annotations

import argparse
import os
import sys
import time
from typing import Optional

import requests

from mesedi_curl import mesedi_client, providers, streaming
from mesedi_curl.version import __version__


def main_console() -> int:
    """Entry point for the `mesedi-curl` console script.

    Wraps main() so setuptools' generated stub can call a zero-arg
    function. Returns the exit code; setuptools will sys.exit() it.
    """
    return main(sys.argv[1:])


def main(argv: list[str]) -> int:
    """Entry point. Returns the equivalent of curl's exit code.

    The Mesedi-recording side is fail-soft, so this function's only
    job is to make a successful HTTP request happen the same way curl
    would have. The recording is best-effort and never alters the
    return code.
    """
    parser = _build_parser()
    args = parser.parse_args(argv)

    if args.version:
        print(f"mesedi-curl {__version__}")
        return 0

    if not args.url:
        parser.print_help(sys.stderr)
        return 2

    config = mesedi_client.load_from_env()
    call = mesedi_client.CapturedCall()
    call.method = args.request or ("POST" if args.data is not None else "GET")
    call.url = args.url
    call.provider = providers.detect_provider(args.url)

    # Decode the request body if any. Curl's -d treats @file specially
    # (load from file), -d @- reads stdin, --data-raw skips that
    # interpretation.
    body_bytes = _resolve_body(args)
    request_info = providers.parse_request_body(call.provider, body_bytes)
    call.model = request_info["model"]
    call.user_prompt_preview = request_info["user_prompt_preview"]
    call.is_streaming = request_info["stream"]

    # Build the headers dict for the upstream call.
    headers = _parse_headers(args.header or [])
    # If the user gave -d but didn't set a Content-Type, default to
    # application/json to match every LLM API's expectation. This
    # matches what curl users almost always intend.
    if body_bytes and not _has_header_ci(headers, "Content-Type"):
        # Only default to JSON if the body looks like JSON.
        stripped = body_bytes.lstrip()
        if stripped.startswith(b"{") or stripped.startswith(b"["):
            headers["Content-Type"] = "application/json"

    # Open the output sink. Default is stdout.buffer; -o redirects to
    # a file. We use binary mode throughout so we never mangle UTF-8.
    sink, close_sink = _open_sink(args.output)
    exit_code = 0
    response: Optional[requests.Response] = None
    response_bytes = b""

    try:
        call.started_at = time.time()
        response = requests.request(
            method=call.method,
            url=call.url,
            headers=headers,
            data=body_bytes,
            stream=True,  # always stream so we can tee
            allow_redirects=args.location,
            verify=not args.insecure,
            timeout=(args.connect_timeout or 30.0, args.max_time or 600.0),
        )
        call.status_code = response.status_code

        # If `include` is set, write the status line + headers first
        # to mimic curl -i.
        if args.include:
            _write_status_and_headers(response, sink)

        is_streaming = call.is_streaming or streaming.detect_streaming_response(
            response
        )
        call.is_streaming = is_streaming
        response_bytes = streaming.stream_response_to_sink(response, sink)
        call.ended_at = time.time()

        # curl exits non-zero only on transport errors; HTTP 4xx/5xx
        # by default leaves curl's exit code at 0. We match.
    except requests.exceptions.SSLError as exc:
        call.ended_at = time.time()
        call.error = f"ssl_error: {exc}"
        sys.stderr.write(f"curl: ({_curl_code_for(exc)}) {exc}\n")
        exit_code = _curl_code_for(exc)
    except requests.exceptions.ConnectionError as exc:
        call.ended_at = time.time()
        call.error = f"connection_error: {exc}"
        sys.stderr.write(f"curl: (7) Failed to connect: {exc}\n")
        exit_code = 7
    except requests.exceptions.Timeout as exc:
        call.ended_at = time.time()
        call.error = f"timeout: {exc}"
        sys.stderr.write(f"curl: (28) Operation timed out: {exc}\n")
        exit_code = 28
    except requests.RequestException as exc:
        call.ended_at = time.time()
        call.error = f"request_failed: {exc}"
        sys.stderr.write(f"curl: (6) {exc}\n")
        exit_code = 6
    finally:
        if response is not None:
            response.close()
        if close_sink:
            sink.close()

    # Extract token math from the captured response. Always best-effort.
    if response_bytes and not call.error:
        parsed = providers.parse_response_body(
            call.provider, response_bytes, call.is_streaming
        )
        call.input_tokens = parsed["input_tokens"]
        call.output_tokens = parsed["output_tokens"]
        call.finish_reason = parsed["finish_reason"]
        call.response_preview = parsed["response_preview"]

    # Fire the recording. Never raises, never affects exit code.
    try:
        mesedi_client.publish(config, call)
    except Exception as exc:  # pragma: no cover, last-line defense
        if not config.silent:
            sys.stderr.write(
                f"mesedi-curl: internal error during publish: {exc}\n"
            )

    return exit_code


def _build_parser() -> argparse.ArgumentParser:
    """Construct the curl-compatible argument parser."""
    p = argparse.ArgumentParser(
        prog="mesedi-curl",
        description=(
            "Transparent curl wrapper that records LLM API calls to Mesedi. "
            "Drop-in for the curl-flag subset commonly used against provider "
            "APIs. See https://docs.mesedi.ai/curl-shim for the full flag list."
        ),
    )
    p.add_argument("url", nargs="?", help="Target URL")
    p.add_argument("-X", "--request", metavar="METHOD", help="HTTP method")
    p.add_argument(
        "-H",
        "--header",
        action="append",
        metavar='"Name: value"',
        help="Custom header (repeatable)",
    )
    p.add_argument(
        "-d",
        "--data",
        metavar="DATA",
        help="Request body. Use @file to load from file, @- for stdin.",
    )
    p.add_argument(
        "--data-raw", dest="data_raw", metavar="DATA", help="Request body, no @ expansion."
    )
    p.add_argument(
        "--data-binary",
        dest="data_binary",
        metavar="DATA",
        help="Request body, no encoding.",
    )
    p.add_argument("-o", "--output", metavar="FILE", help="Write body to FILE")
    p.add_argument("-s", "--silent", action="store_true", help="Suppress progress")
    p.add_argument(
        "-i", "--include", action="store_true", help="Include response headers"
    )
    p.add_argument(
        "-L", "--location", action="store_true", help="Follow redirects"
    )
    p.add_argument(
        "-k", "--insecure", action="store_true", help="Skip TLS verification"
    )
    p.add_argument(
        "--connect-timeout",
        dest="connect_timeout",
        type=float,
        metavar="SECONDS",
        help="Connect timeout",
    )
    p.add_argument(
        "--max-time",
        dest="max_time",
        type=float,
        metavar="SECONDS",
        help="Overall timeout",
    )
    p.add_argument(
        "--version", action="store_true", help="Print mesedi-curl version"
    )
    return p


def _resolve_body(args: argparse.Namespace) -> Optional[bytes]:
    """Convert -d / --data-raw / --data-binary into bytes.

    curl semantics:
      -d / --data: if value starts with '@', load from file. '@-' is stdin.
      --data-raw:  literal value, no @ expansion.
      --data-binary: literal value, no newline stripping (curl's -d strips
                     newlines from @file input; we don't bother, modern
                     LLM JSON is sent intact).
    """
    raw = args.data_binary or args.data_raw or args.data
    if raw is None:
        return None
    if args.data is raw and raw.startswith("@"):
        path = raw[1:]
        if path == "-":
            return sys.stdin.buffer.read()
        with open(path, "rb") as f:
            return f.read()
    return raw.encode("utf-8")


def _parse_headers(header_args: list[str]) -> dict[str, str]:
    """Convert ["Name: value", ...] into a dict.

    Headers given multiple times retain only the LAST value, matching
    curl's behavior. Headers without a colon are dropped with a warning
    on stderr.
    """
    out: dict[str, str] = {}
    for raw in header_args:
        if ":" not in raw:
            sys.stderr.write(f"mesedi-curl: ignoring malformed header: {raw}\n")
            continue
        name, _, value = raw.partition(":")
        out[name.strip()] = value.strip()
    return out


def _has_header_ci(headers: dict[str, str], name: str) -> bool:
    """Case-insensitive header membership check."""
    lo = name.lower()
    return any(k.lower() == lo for k in headers.keys())


def _open_sink(output: Optional[str]):
    """Resolve the binary write sink. Returns (sink, should_close)."""
    if output:
        f = open(output, "wb")
        return f, True
    return sys.stdout.buffer, False


def _write_status_and_headers(response: requests.Response, sink) -> None:
    """Emit the status line + headers (-i mode)."""
    line = f"HTTP/{_http_version(response)} {response.status_code} {response.reason}\r\n"
    sink.write(line.encode("utf-8", errors="replace"))
    for name, value in response.headers.items():
        sink.write(f"{name}: {value}\r\n".encode("utf-8", errors="replace"))
    sink.write(b"\r\n")
    sink.flush()


def _http_version(response: requests.Response) -> str:
    raw = getattr(response.raw, "version", None)
    if raw == 11:
        return "1.1"
    if raw == 10:
        return "1.0"
    if raw == 20:
        return "2"
    return "1.1"


def _curl_code_for(exc: Exception) -> int:
    """Map a requests exception class to curl's documented exit code.

    See https://everything.curl.dev/usingcurl/returns. We cover the
    handful that show up against LLM APIs:
      6 = could not resolve host
      7 = could not connect
      28 = timeout
      60 = TLS / cert verification failed
    """
    name = type(exc).__name__
    if name == "SSLError":
        return 60
    return 7
