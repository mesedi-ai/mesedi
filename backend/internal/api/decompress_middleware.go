// Request-body decompression middleware.
//
// Transparently decompresses inbound request bodies when the client
// sends a recognized Content-Encoding value. Downstream handlers see
// a normal uncompressed body and never need to know compression was
// used on the wire.
//
// Old SDK versions that do not declare Content-Encoding keep working
// unchanged because the middleware is a no-op when the header is
// absent. New SDK versions can opt in to compression for any payload
// they choose; the SDK default is to compress only above a small
// threshold so tiny calls do not pay the framing overhead.
//
// Why two formats: the Python SDK ships with the `zstandard` package
// and uses zstd (better ratios, ~5-10x on JSON). The TypeScript SDK
// uses Node's built-in `node:zlib` gzip to avoid adding a runtime
// dependency or a WASM binary to the SDK download size. Both reach
// the backend through this same middleware; the Content-Encoding
// header picks the right decoder.
//
// Failure modes:
//
//   - Header absent: pass through unchanged (the common case for
//     legacy SDKs and curl smoke tests).
//   - Header is "zstd" or "gzip": wrap r.Body with the matching
//     decoder so downstream readers see the original uncompressed
//     bytes. On any decoder stream error mid-read, the downstream
//     JSON decoder surfaces a 400 ("invalid JSON") which is the same
//     shape callers already handle for any malformed request body.
//   - Header is any other value: return 415 immediately so the
//     caller knows we do not speak that encoding. Keeps the
//     negotiated surface explicit rather than silently accepting
//     bytes we cannot read.
//
// Placement in the chain: this middleware MUST run after auth (so
// unauthenticated requests skip the decompression cost) and after
// oversized-payload detection (so the abuse signal observes the
// actual wire size rather than the inflated post-decompression
// size). See NewAuthChain in middleware.go for the wired-in order.
package api

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Content-Encoding values this backend accepts on inbound request
// bodies. SDKs and curl callers that wish to compress must declare
// one of these exact values; any other declared encoding returns 415.
const (
	EncodingZstd = "zstd"
	EncodingGzip = "gzip"
)

// SupportedRequestEncoding is retained as an alias for the primary
// (zstd) encoding so older test code that referenced it still
// compiles. New code should use the EncodingZstd / EncodingGzip
// constants directly.
const SupportedRequestEncoding = EncodingZstd

// supportedRequestEncodings is the full set this middleware accepts.
// Used to build the 415 error message so callers see the complete
// list of values they can declare.
var supportedRequestEncodings = []string{EncodingZstd, EncodingGzip}

// decompressMiddleware wraps r.Body with the matching decoder when
// the client declared a recognized Content-Encoding. No-op when the
// header is absent. Returns 415 on any other declared encoding.
func decompressMiddleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			enc := r.Header.Get("Content-Encoding")
			switch enc {
			case "":
				next.ServeHTTP(w, r)
				return
			case EncodingZstd:
				zr, err := zstd.NewReader(r.Body)
				if err != nil {
					writeError(w, http.StatusBadRequest,
						"invalid Content-Encoding: zstd reader rejected the stream: "+err.Error())
					return
				}
				r.Body = &zstdRequestBody{decoder: zr, underlying: r.Body}
				clearEncodingMetadata(r)
				next.ServeHTTP(w, r)
				return
			case EncodingGzip:
				gr, err := gzip.NewReader(r.Body)
				if err != nil {
					writeError(w, http.StatusBadRequest,
						"invalid Content-Encoding: gzip reader rejected the stream: "+err.Error())
					return
				}
				r.Body = &gzipRequestBody{decoder: gr, underlying: r.Body}
				clearEncodingMetadata(r)
				next.ServeHTTP(w, r)
				return
			default:
				writeError(w, http.StatusUnsupportedMediaType,
					"unsupported Content-Encoding: "+enc+" (this backend accepts: "+strings.Join(supportedRequestEncodings, ", ")+")")
				return
			}
		})
	}
}

// clearEncodingMetadata hides the on-wire encoding from downstream
// so handlers and middleware see a "normal" uncompressed request
// shape, and clears the declared length since the decompressed size
// is unknown until the stream drains.
func clearEncodingMetadata(r *http.Request) {
	r.Header.Del("Content-Encoding")
	r.ContentLength = -1
}

// zstdRequestBody adapts a zstd.Decoder into the io.ReadCloser shape
// http.Request.Body expects. The decoder itself is an io.Reader; we
// own the Close so we can release both the decoder's internal buffers
// and the original underlying body in one call (connection pooling
// inside net/http depends on r.Body.Close() returning when the handler
// is done).
type zstdRequestBody struct {
	decoder    *zstd.Decoder
	underlying io.ReadCloser
}

func (z *zstdRequestBody) Read(p []byte) (int, error) {
	return z.decoder.Read(p)
}

func (z *zstdRequestBody) Close() error {
	z.decoder.Close()
	return z.underlying.Close()
}

// gzipRequestBody mirrors zstdRequestBody for the gzip case. The
// stdlib gzip.Reader is itself an io.ReadCloser, so we could
// technically embed it; we keep the explicit shape to match
// zstdRequestBody for symmetry and to be explicit about closing the
// underlying body too.
type gzipRequestBody struct {
	decoder    *gzip.Reader
	underlying io.ReadCloser
}

func (g *gzipRequestBody) Read(p []byte) (int, error) {
	return g.decoder.Read(p)
}

func (g *gzipRequestBody) Close() error {
	_ = g.decoder.Close()
	return g.underlying.Close()
}
