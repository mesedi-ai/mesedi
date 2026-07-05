# mesedi-curl

Transparent `curl` wrapper that records direct LLM API calls to Mesedi.

For the engineer who runs `curl` from a notebook, a bash script, or a CI
job and bypasses the Mesedi SDK entirely. Wrap the call once and Mesedi
sees every request, response, token count, latency, and finish reason.

The wrapper never blocks the underlying request. If Mesedi's backend is
unreachable, the user-visible request and response are unaffected; a
warning is printed to stderr and the call proceeds as if `mesedi-curl`
were plain `curl`.

## Install

```
pip install mesedi-curl
```

Or one-line install via the install script published with each GitHub
release:

```
curl -fsSL https://get.mesedi.ai/curl-shim | bash
```

## Configure

Export your Mesedi project API key:

```
export MESEDI_API_KEY=mesedi_sk_...
```

That's the only required variable. Optional:

```
MESEDI_BACKEND_URL=https://api.mesedi.ai     # default; override for self-host
MESEDI_PROJECT_ID=proj_xxxxxxxx              # usually inferred from key
MESEDI_SILENT=1                              # suppress stderr warnings
```

If `MESEDI_API_KEY` is unset the wrapper acts as a pure pass-through,
which makes it safe to drop into a script without breaking anyone who
has not configured Mesedi yet.

## Use

Anywhere you'd run `curl`, swap in `mesedi-curl`:

```
mesedi-curl https://api.anthropic.com/v1/messages \
  -H "x-api-key: $ANTHROPIC_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "content-type: application/json" \
  -d '{
    "model": "claude-opus-4-6",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Hello, Claude"}]
  }'
```

Streaming, OpenAI, Gemini, all work the same way. The wrapper detects
the provider from the URL and parses tokens accordingly.

To alias `curl` itself in a CI image:

```
alias curl=mesedi-curl
```

…or symlink:

```
ln -sf "$(which mesedi-curl)" /usr/local/bin/curl
```

## Provider support

| Provider  | Host                                           | Streaming | Token math |
| --------- | ---------------------------------------------- | --------- | ---------- |
| Anthropic | `api.anthropic.com`                            | Yes       | Yes        |
| OpenAI    | `api.openai.com`, `*.openai.azure.com`         | Yes       | Yes (\*)   |
| Gemini    | `generativelanguage.googleapis.com`, Vertex AI | Yes       | Yes        |
| Other     | Any URL                                        | Yes       | Pass-through (no token math) |

(\*) OpenAI streaming emits usage stats only when the request body
includes `"stream_options": {"include_usage": true}`. Without it, token
counts in Mesedi will be zero for streaming OpenAI calls. This is an
upstream API constraint, not a wrapper limitation.

## What ends up in your Mesedi dashboard

Every `mesedi-curl` invocation creates one execution and one `llm_call`
event under it. The execution carries:

- request URL, method, HTTP status
- provider + model name
- input + output token counts
- request latency
- crash signature (transport-level error) or finish reason

The event carries the full request body, response (or streamed text
concatenation), and per-call payload fields. Mesedi's seven failure-class
detectors run against the event stream the same way they would for a
call made through the Python or TypeScript SDKs.

## Failure modes

The wrapper is fail-soft on the recording side:

| Condition                          | Behavior                                                |
| ---------------------------------- | ------------------------------------------------------- |
| `MESEDI_API_KEY` unset             | Silent pass-through. Acts as plain curl.                |
| Mesedi backend unreachable         | Warn once on stderr, proceed with the request normally. |
| Backend returns 4xx (auth, quota)  | Warn once on stderr with the response body preview.     |
| Backend returns 5xx                | Warn once on stderr, proceed.                           |
| Provider returns non-JSON or HTML  | Record the call with a preview of the raw body.         |

The underlying curl-equivalent request's exit code is never affected by
Mesedi-side issues.

## Supported flags

A curl-compatible subset:

```
-X / --request METHOD              HTTP method
-H / --header "Name: value"        Request header (repeatable)
-d / --data DATA                   Request body. @file loads from file, @- reads stdin.
--data-raw DATA                    Request body, no @-expansion.
--data-binary DATA                 Request body, no encoding mangling.
-o / --output FILE                 Write body to FILE instead of stdout.
-s / --silent                      Suppress progress meter.
-i / --include                     Include response headers in output.
-L / --location                    Follow redirects.
-k / --insecure                    Skip TLS verification.
--connect-timeout SECONDS          Connect timeout.
--max-time SECONDS                 Overall timeout.
--version                          Print mesedi-curl version.
```

Unsupported flags raise a usage error rather than being silently
dropped.

## License

MIT. See LICENSE in the repo root.
