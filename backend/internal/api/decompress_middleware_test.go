package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// TestDecompressMiddleware_PassthroughWhenNoEncoding covers the
// common case for legacy SDK versions and curl smoke tests: no
// Content-Encoding header at all. The middleware must be a complete
// no-op, forwarding r.Body to the handler unchanged.
func TestDecompressMiddleware_PassthroughWhenNoEncoding(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	var received []byte

	handler := decompressMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		received = b
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(received, body) {
		t.Fatalf("body mismatch:\n  got:  %q\n  want: %q", received, body)
	}
}

// TestDecompressMiddleware_ZstdEncodingDecompressed covers the
// happy compression path. The client sends a zstd-encoded body with
// Content-Encoding: zstd; the middleware must wrap r.Body with the
// decoder, strip the encoding header so downstream sees a normal
// shape, and clear ContentLength since the decompressed size is
// unknown until the stream drains.
func TestDecompressMiddleware_ZstdEncodingDecompressed(t *testing.T) {
	original := []byte(`{"hello":"world","events":[{"id":1},{"id":2}]}`)

	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	compressed := encoder.EncodeAll(original, nil)
	encoder.Close()

	var receivedBody []byte
	var receivedEncoding string
	var receivedLength int64

	handler := decompressMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		receivedBody = b
		receivedEncoding = r.Header.Get("Content-Encoding")
		receivedLength = r.ContentLength
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/events", bytes.NewReader(compressed))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", SupportedRequestEncoding)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(receivedBody, original) {
		t.Fatalf("body mismatch:\n  got:  %q\n  want: %q", receivedBody, original)
	}
	if receivedEncoding != "" {
		t.Fatalf("expected Content-Encoding to be stripped after decompression, got %q", receivedEncoding)
	}
	if receivedLength != -1 {
		t.Fatalf("expected ContentLength=-1 after decompression, got %d", receivedLength)
	}

	var parsed map[string]any
	if err := json.Unmarshal(receivedBody, &parsed); err != nil {
		t.Fatalf("decompressed body not valid JSON: %v", err)
	}
	if parsed["hello"] != "world" {
		t.Fatalf("unexpected JSON content: %v", parsed)
	}
}

// TestDecompressMiddleware_GzipEncodingDecompressed covers the gzip
// happy path. The TypeScript SDK ships gzip (Node's built-in zlib)
// rather than zstd to avoid adding a WASM runtime dependency to the
// SDK download. Backend recognizes both Content-Encoding values
// through the same middleware path.
func TestDecompressMiddleware_GzipEncodingDecompressed(t *testing.T) {
	original := []byte(`{"hello":"world","events":[{"id":1},{"id":2}]}`)

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(original); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	compressed := buf.Bytes()

	var receivedBody []byte
	var receivedEncoding string
	var receivedLength int64

	handler := decompressMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		receivedBody = b
		receivedEncoding = r.Header.Get("Content-Encoding")
		receivedLength = r.ContentLength
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/events", bytes.NewReader(compressed))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", EncodingGzip)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(receivedBody, original) {
		t.Fatalf("body mismatch:\n  got:  %q\n  want: %q", receivedBody, original)
	}
	if receivedEncoding != "" {
		t.Fatalf("expected Content-Encoding to be stripped after decompression, got %q", receivedEncoding)
	}
	if receivedLength != -1 {
		t.Fatalf("expected ContentLength=-1 after decompression, got %d", receivedLength)
	}
}

// TestDecompressMiddleware_UnsupportedEncoding covers the case where
// a client declares an encoding this backend does not implement. The
// middleware must reject up front with 415 and name both supported
// encodings (zstd, gzip) so the caller learns the full negotiated
// surface instead of silently mis-reading bytes.
func TestDecompressMiddleware_UnsupportedEncoding(t *testing.T) {
	handler := decompressMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be reached when encoding is unsupported")
	}))

	req := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader("ignored"))
	req.Header.Set("Content-Encoding", "br") // Brotli, not implemented here
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), EncodingZstd) {
		t.Fatalf("expected error to name %q, got: %s", EncodingZstd, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), EncodingGzip) {
		t.Fatalf("expected error to name %q, got: %s", EncodingGzip, rec.Body.String())
	}
}

// TestDecompressMiddleware_LargePayloadRoundTrip is a sanity check
// that the wrapping survives a realistic batched-events shape with
// repeating structure. zstd typically compresses such payloads 5-10x;
// the test does not enforce a ratio, only that the round trip is
// faithful end to end.
func TestDecompressMiddleware_LargePayloadRoundTrip(t *testing.T) {
	// Build a batch of 100 events with repeating fields, the shape the
	// Python SDK sends in production after the default 100-item batch.
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < 100; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"event_id":"evt_`)
		b.WriteString(strings.Repeat("x", 24))
		b.WriteString(`","execution_id":"exec_abc123","event_type":"llm_call","payload":{"prompt":"summarize","response":"ok"}}`)
	}
	b.WriteString("]")
	original := []byte(b.String())

	if len(original) < 1024 {
		t.Fatalf("expected synthetic batch above 1 KB to exercise real compression, got %d bytes", len(original))
	}

	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	compressed := encoder.EncodeAll(original, nil)
	encoder.Close()

	if len(compressed) >= len(original) {
		t.Fatalf("expected compression to reduce payload size, got compressed=%d original=%d", len(compressed), len(original))
	}

	var received []byte
	handler := decompressMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		received = got
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/events", bytes.NewReader(compressed))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", SupportedRequestEncoding)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(received, original) {
		t.Fatalf("round-trip body mismatch: got %d bytes, want %d bytes", len(received), len(original))
	}
}
