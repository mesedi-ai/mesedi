// Request-body decompression middleware.
//
// Transparently decompresses inbound request bodies when the client
// sends Content-Encoding: zstd. Downstream handlers see a normal
// uncompressed body and never need to know compression was used on
// the wire.
//
// Old SDK versions that do not declare Content-Encoding keep working
// unchanged because the middleware is a no-op when the header is
// absent. New SDK versions can opt in to compression for any payload
// they choose; the SDK default is to compress only above a small
// threshold so tiny calls do not pay the framing overhead.
//
// Failure modes:
//
//   - Header absent: pass through unchanged (the common case for
//     legacy SDKs and curl smoke tests).
//   - Header is "zstd": wrap r.Body with a zstd decoder so downstream
//     readers see the original uncompressed bytes. On any decoder
//     stream error mid-read, the downstream JSON decoder surfaces a
//     400 ("invalid JSON") which is the same shape callers already
//     handle for any malformed request body.
//   - Header is any other value: return 415 immediately so the
//     caller knows we do not speak that encoding. Keeps the negotiated
//     surface explicit rather than silently accepting bytes we cannot
//     read.
//
// Placement in the chain: this middleware MUST run after auth (so
// unauthenticated requests skip the decompression cost) and after
// oversized-payload detection (so the abuse signal observes the actual
// wire size rather than the inflated post-decompression size). See
// NewAuthChain in middleware.go for the wired-in order.
package api

import (
	"io"
	"net/http"

	"github.com/klauspost/compress/zstd"
)

// SupportedRequestEncoding is the Content-Encoding value this backend
// accepts on inbound request bodies. SDKs and curl callers that wish
// to compress must declare this exact value; any other declared
// encoding returns 415.
const SupportedRequestEncoding = "zstd"

// decompressMiddleware wraps r.Body with a zstd reader when the
// client declared Content-Encoding: zstd. No-op when the header is
// absent. Returns 415 on any other declared encoding.
func decompressMiddleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			enc := r.Header.Get("Content-Encoding")
			switch enc {
			case "":
				next.ServeHTTP(w, r)
				return
			case SupportedRequestEncoding:
				zr, err := zstd.NewReader(r.Body)
				if err != nil {
					writeError(w, http.StatusBadRequest,
						"invalid Content-Encoding: zstd reader rejected the stream: "+err.Error())
					return
				}
				r.Body = &zstdRequestBody{decoder: zr, underlying: r.Body}
				// Hide the encoding from downstream so handlers + middleware
				// see a "normal" uncompressed request shape, and clear the
				// declared length since the decompressed size is unknown
				// until the stream drains.
				r.Header.Del("Content-Encoding")
				r.ContentLength = -1
				next.ServeHTTP(w, r)
				return
			default:
				writeError(w, http.StatusUnsupportedMediaType,
					"unsupported Content-Encoding: "+enc+" (this backend accepts: "+SupportedRequestEncoding+")")
				return
			}
		})
	}
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
