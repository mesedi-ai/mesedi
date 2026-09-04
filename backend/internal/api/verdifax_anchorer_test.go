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
	"crypto/sha256"
	"encoding/hex"
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

// anchorLeafPreimage builds a receipt's leaf preimage the way Verdifax
// does: a domain tag, an envelope id, the submitted digest, a binding
// hash and a nonce, joined with ".". The shape matters — the anchorer
// searches it for the checkpoint hash — so a placeholder here would
// make every test below vacuous.
func anchorLeafPreimage(digest string) string {
	return "verdifax.ledger.input.v2.attest:1:checkpoint/1/t." + digest + ".bind.7"
}

func anchorLeafHash(preimage string) string {
	sum := sha256.Sum256([]byte(preimage))
	return hex.EncodeToString(sum[:])
}

// okReceipt writes a well-formed receipt for the given digest.
func okReceipt(w http.ResponseWriter, digest, provenance, entryID string) {
	preimage := anchorLeafPreimage(digest)
	okReceiptWithLeaf(w, digest, provenance, entryID, preimage, anchorLeafHash(preimage))
}

// okReceiptWithLeaf writes a receipt with the leaf fields set
// explicitly, so a test can produce one that is well-formed in every
// other respect but not verifiable.
func okReceiptWithLeaf(w http.ResponseWriter, digest, provenance, entryID,
	leafPreimage, leafHash string,
) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":             true,
		"attestation_id": 42,
		"digest":         digest,
		"ledger_backend": "mock",
		"log_entry_id":   entryID,
		"leaf_preimage":  leafPreimage,
		"leaf_hash":      leafHash,
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

	anchor, err := a.AnchorCheckpoint(context.Background(), cp)
	if err != nil {
		t.Fatalf("AnchorCheckpoint: %v", err)
	}
	if anchor.LogEntryID != "rekor-9001" || anchor.LedgerBackend != "mock" {
		t.Errorf("got (%q, %q), want (rekor-9001, mock)",
			anchor.LogEntryID, anchor.LedgerBackend)
	}
	// The preimage must survive the trip. Without it the chain records a
	// log entry nobody can tie back to the checkpoint.
	if !strings.Contains(anchor.LeafPreimage, cp.Hash) {
		t.Errorf("the anchor did not carry a leaf preimage containing the "+
			"checkpoint hash, so it could not be verified: %q", anchor.LeafPreimage)
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

// ── CHECK 3: the receipt must be checkable by a third party ──────────
//
// Checks 1 and 2 ask Verdifax to vouch for itself, which catches a
// misconfigured endpoint and not a dishonest one. This one is different:
// it is the only check whose failure means an auditor could not confirm
// the anchor against the log. Every checkpoint anchored before it
// existed names a real Sigstore entry that nobody — this company
// included — can tie back to the checkpoint that produced it.

func TestAnchorer_RefusesAReceiptWithNoLeafPreimage(t *testing.T) {
	t.Parallel()
	cp := anchorTestCheckpoint(t)

	a, done := anchorerAgainst(t, func(w http.ResponseWriter, _ *http.Request) {
		okReceiptWithLeaf(w, cp.Hash, expectedProvenance, "rekor-1", "", "")
	})
	defer done()

	_, err := a.AnchorCheckpoint(context.Background(), cp)
	if err == nil {
		t.Fatal("accepted a receipt with no leaf preimage. The log records a hash " +
			"of a string that was not returned, so this anchor could never be " +
			"checked — which is exactly the state that shipped and went unnoticed " +
			"until an independent verifier was pointed at production")
	}
	if !strings.Contains(err.Error(), "preimage") {
		t.Errorf("the error should name what is missing: %v", err)
	}
}

// The preimage must produce the leaf Verdifax says it anchored. If it
// does not, one of the two values is wrong and recording either would
// be recording a fiction.
func TestAnchorer_RefusesAnInternallyInconsistentReceipt(t *testing.T) {
	t.Parallel()
	cp := anchorTestCheckpoint(t)

	a, done := anchorerAgainst(t, func(w http.ResponseWriter, _ *http.Request) {
		okReceiptWithLeaf(w, cp.Hash, expectedProvenance, "rekor-1",
			anchorLeafPreimage(cp.Hash), strings.Repeat("ab", 32))
	})
	defer done()

	_, err := a.AnchorCheckpoint(context.Background(), cp)
	if err == nil {
		t.Fatal("accepted a receipt whose preimage does not hash to the leaf it reports")
	}
	if !strings.Contains(err.Error(), "inconsistent") {
		t.Errorf("the error should say the receipt contradicts itself: %v", err)
	}
}

// The anchored leaf must commit to THIS checkpoint. A well-formed
// receipt for somebody else's leaf is the subtlest of the three: every
// field is valid, the hashes all agree, and the entry describes a
// different record entirely.
func TestAnchorer_RefusesALeafThatDoesNotContainTheCheckpointHash(t *testing.T) {
	t.Parallel()
	cp := anchorTestCheckpoint(t)
	elsewhere := anchorLeafPreimage(strings.Repeat("ef", 32))

	a, done := anchorerAgainst(t, func(w http.ResponseWriter, _ *http.Request) {
		okReceiptWithLeaf(w, cp.Hash, expectedProvenance, "rekor-1",
			elsewhere, anchorLeafHash(elsewhere))
	})
	defer done()

	_, err := a.AnchorCheckpoint(context.Background(), cp)
	if err == nil {
		t.Fatal("accepted an internally consistent receipt whose leaf does not " +
			"contain this checkpoint's hash; the log entry would describe a " +
			"different record")
	}
	if !strings.Contains(err.Error(), cp.Hash) {
		t.Errorf("the error should name the hash that is missing from the leaf: %v", err)
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

	_, err := a.AnchorCheckpoint(context.Background(), cp)
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

	_, err := a.AnchorCheckpoint(context.Background(), cp)
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
			anchor, err := a.AnchorCheckpoint(context.Background(), cp)
			entryID := anchor.LogEntryID
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
	_, err := a.AnchorCheckpoint(context.Background(), cp)
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
