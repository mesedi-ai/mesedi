# mesedi-curl: a transparent curl wrapper that captures direct provider
# API calls and forwards them to Mesedi's ingest endpoints.
#
# Designed for the engineer who runs curl from a notebook, a bash script,
# or a CI job and bypasses the Mesedi SDK entirely. Wrap the call once
# and Mesedi sees every request, response, token count, and latency.
#
# The wrapper NEVER blocks the underlying curl call: if Mesedi's backend
# is unreachable, the user-visible request and response are unaffected
# and a warning is printed to stderr.
from mesedi_curl.version import __version__

__all__ = ["__version__"]
