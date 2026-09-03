package api

// Tests for VerdifaxAnchorer.
//
// The two that carry the weight:
//
//   TestAnchorer_RefusesAReceiptForADifferentDigest
//     — nothing downstream would catch this. Every hash in the chain
//       would still verify against itself while the chain named a log
//       entry attesting to something else entirely.
//
//   TestAnchorer_RefusesAReceiptClaimingAThirdParty
//     — Mesedi and Verdifax are one Delaware LLC. A receipt saying a
//       third party submitted this is false, and it would be false in
//       every receipt afterwards, quietly, until someone read the JSON.
//
// httptest.Server rather than a mocked RoundTripper: it exercises the
// real request path including headers and status handling, which is
// where the auth-header bug in this file actually was.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mesedi/backend/internal/attest"
)

func anchorTestCheckpoint(t *testing.T) attest.Checkpoint {
	t.Helper()
	cp, err := attest.BuildCheckpoint(attest.CheckpointParams{
		IntervalStart: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC),
		IntervalEnd:   time.Date(2026, 9, 3, 13, 0, 0, 0, time.UTC),
		Interval:      time.Hour,
		Leaves: []attest.TenantLeaf{{
			ProjectID:       "proj-a",
			IntervalRoot:    strings.Repeat("ab", 32),
			ExecutionCount:  2,
			CumulativeCount: 2,
			PrevLeafHash:    attest.ZeroHash,
		}},
		Now: time.Date(2026, 9, 3, 13, 0, 30, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("BuildCheckpoint: %v", err)
	}
	return cp
}

// anchorerAgainst spins a fake Verdifax that runs handler, and returns
// an anchorer pointed at it.
func anchorerAgainst(t *testing.T, handler http.HandlerFunc) (*VerdifaxAnchorer, func()) {
	t.Helper()
	srv := httptest.NewServer(handler)
	a := NewVerdifaxAnchorer(srv.URL, "test-key", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if a == nil {
		t.Fatal("NewVerdifaxAnchorer returned nil for a configured base URL and key")
	}
	return a, srv.Close
}

// okReceipt writes a well-formed receipt for the given digest.
func okReceipt(w http.ResponseWriter, digest, provenance, entryID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":             true,
		"attestation_id": 42,
		"digest":         digest,
		"ledger_backend": "mock",
		"log_entry_id":   entryID,
		"provenance":     provenance,
	})
}

// ── the happy path, and what it proves about the wire format ─────────

func TestAnchorer_SendsTheCheckpointHashWithTheRightHeaderAndFields(t *testing.T) {
	t.Parallel()
	cp := anchorTestCheckpoint(t)

	var (
		gotKeyHeader string
		gotAuthTried string
		gotUA        string
		gotPath      string
		gotBody      map[string]any
	)
	a, done := anchorerAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		gotKeyHeader = r.Header.Get("X-Verdifax-Key")
		gotAuthTried = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		okReceipt(w, cp.Hash, expectedProvenance, "rekor-9001")
	})
	defer done()

	entryID, backend, err := a.AnchorCheckpoint(context.Background(), cp)
	if err != nil {
		t.Fatalf("AnchorCheckpoint: %v", err)
	}
	if entryID != "rekor-9001" || backend != "mock" {
		t.Errorf("got (%q, %q), want (rekor-9001, mock)", entryID, backend)
	}

	// The auth header this file originally got wrong. Verdifax reads
	// X-Verdifax-Key; a Bearer token would 401 on every anchor.
	if gotKeyHeader != "test-key" {
		t.Errorf("X-Verdifax-Key = %q, want the api key", gotKeyHeader)
	}
	if gotAuthTried != "" {
		t.Errorf("an Authorization header was sent (%q); Verdifax does not read it "+
			"and sending the key twice widens where it can leak", gotAuthTried)
	}
	if gotUA != VerdifaxUserAgent {
		t.Errorf("User-Agent = %q, want %q", gotUA, VerdifaxUserAgent)
	}
	if gotPath != "/attest" {
		t.Errorf("path = %q, want /attest", gotPath)
	}

	// The digest submitted must be the CHECKPOINT hash, not a leaf, not
	// the merkle root. This is the value the whole chain is built on.
	if gotBody["digest"] != cp.Hash {
		t.Errorf("submitted digest %v, want the checkpoint hash %s",
			gotBody["digest"], cp.Hash)
	}
	if gotBody["algorithm"] != cp.Format {
		t.Errorf("algorithm = %v, want %s; Verdifax stores this verbatim so a "+
			"future construction change stays distinguishable",
			gotBody["algorithm"], cp.Format)
	}

	// Nothing identifying may reach Verdifax. It anchors a hash it
	// cannot interpret, and that is the design.
	body, _ := json.Marshal(gotBody)
	for _, leaked := range []string{"proj-a", "execution", "payload"} {
		if strings.Contains(string(body), leaked) {
			t.Errorf("request body leaks %q to Verdifax: %s", leaked, body)
		}
	}
}

// ── the two checks this file exists for ──────────────────────────────

func TestAnchorer_RefusesAReceiptForADifferentDigest(t *testing.T) {
	t.Parallel()
	cp := anchorTestCheckpoint(t)
	other := strings.Repeat("cd", 32)

	a, done := anchorerAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		okReceipt(w, other, expectedProvenance, "rekor-1")
	})
	defer done()

	_, _, err := a.AnchorCheckpoint(context.Background(), cp)
	if err == nil {
		t.Fatal("accepted a receipt for a digest we never submitted. Nothing " +
			"downstream would catch this: the chain would name a log entry that " +
			"attests to something else, and every hash in it would still verify")
	}
	if !strings.Contains(err.Error(), other) {
		t.Errorf("the error should name the mismatched digest: %v", err)
	}
}

func TestAnchorer_RefusesAReceiptClaimingAThirdParty(t *testing.T) {
	t.Parallel()
	cp := anchorTestCheckpoint(t)

	a, done := anchorerAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		okReceipt(w, cp.Hash, "third-party-submitted", "rekor-1")
	})
	defer done()

	_, _, err := a.AnchorCheckpoint(context.Background(), cp)
	if err == nil {
		t.Fatal("accepted a receipt claiming an independent submitter. Mesedi and " +
			"Verdifax are one legal entity, so that receipt is false — and it " +
			"would be false in every receipt afterwards")
	}
	// The operator has to be able to act on it.
	if !strings.Contains(err.Error(), "submitter-class") {
		t.Errorf("the error should name the admin route that fixes it: %v", err)
	}
}

// ── failure handling ─────────────────────────────────────────────────

func TestAnchorer_FailureModes(t *testing.T) {
	t.Parallel()
	cp := anchorTestCheckpoint(t)

	cases := []struct {
		name    string
		handler http.HandlerFunc
		wantIn  string
	}{
		{
			name: "unclassified key returns 409",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ok": false, "error": "this API key has no submitter classification",
				})
			},
			wantIn: "409",
		},
		{
			name: "unauthorised",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "bad key"})
			},
			wantIn: "401",
		},
		{
			// A proxy 502 returns HTML. Reporting "invalid JSON" would
			// send whoever reads the log looking in the wrong place.
			name: "proxy returns HTML",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
			},
			wantIn: "502",
		},
		{
			name: "accepted but no log entry id",
			handler: func(w http.ResponseWriter, r *http.Request) {
				okReceipt(w, cp.Hash, expectedProvenance, "")
			},
			wantIn: "no log entry id",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, done := anchorerAgainst(t, tc.handler)
			defer done()
			entryID, _, err := a.AnchorCheckpoint(context.Background(), cp)
			if err == nil {
				t.Fatalf("expected a failure, got entry id %q", entryID)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.wantIn)
			}
			if entryID != "" {
				t.Errorf("a failure returned a log entry id %q; the scheduler would "+
					"record an anchor that did not happen", entryID)
			}
		})
	}
}

// The API key must never appear in an error. Errors get logged, and a
// logged credential is a leaked credential.
func TestAnchorer_ErrorsNeverContainTheAPIKey(t *testing.T) {
	t.Parallel()
	cp := anchorTestCheckpoint(t)
	const secret = "sk-super-secret-value"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"error":"nope"}`))
	}))
	defer srv.Close()

	a := NewVerdifaxAnchorer(srv.URL, secret, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, _, err := a.AnchorCheckpoint(context.Background(), cp)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the API key appears in an error that will be logged: %v", err)
	}
}

// ── configuration ────────────────────────────────────────────────────

// Nil rather than a half-configured client. The scheduler reads nil as
// "stall visibly at genesis", which is right for a deployment with no
// transparency log — whereas a client that exists but cannot
// authenticate fails every interval forever and looks like an outage.
func TestNewVerdifaxAnchorer_NilWhenUnconfigured(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cases := []struct{ name, url, key string }{
		{"no url", "", "k"},
		{"no key", "https://api.verdifax.com", ""},
		{"neither", "", ""},
		{"whitespace only", "   ", "  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if a := NewVerdifaxAnchorer(tc.url, tc.key, logger); a != nil {
				t.Error("returned a half-configured anchorer instead of nil")
			}
		})
	}
	// And a trailing slash must not produce a double slash in the path.
	a := NewVerdifaxAnchorer("https://api.verdifax.com/", "k", logger)
	if a == nil {
		t.Fatal("nil for a valid configuration")
	}
	if strings.HasSuffix(a.BaseURL, "/") {
		t.Errorf("BaseURL %q keeps its trailing slash, which would request //attest",
			a.BaseURL)
	}
}

// It must satisfy the interface the scheduler depends on.
var _ CheckpointAnchorer = (*VerdifaxAnchorer)(nil)
