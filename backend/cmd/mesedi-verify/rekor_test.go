package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mesedi/backend/internal/attest"
)

// A fake Rekor, because the failure paths are the point.
//
// The happy path against the real log is worth running by hand, but the
// cases that matter to an auditor are the ones where the log DISAGREES
// with the export, and those cannot be produced on demand from the real
// service. A stub is the only way to exercise them at all, so the choice
// is a stub or no coverage of the findings the tool exists to make.

func rekorBody(t *testing.T, kind, algorithm, hash string) string {
	t.Helper()
	body := map[string]any{
		"apiVersion": "0.0.1",
		"kind":       kind,
		"spec": map[string]any{
			"data": map[string]any{
				"hash": map[string]any{"algorithm": algorithm, "value": hash},
			},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// fakeRekor serves one entry per log index.
func fakeRekor(t *testing.T, entries map[string]map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := r.URL.Query().Get("logIndex")
		e, ok := entries[idx]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"some-entry-uuid": e})
	}))
	t.Cleanup(srv.Close)
	return srv
}

const testHash = "6ee5b1397f6ef72b6cb09dc6090a8759bbad0cb86cd0324768940ea3bec587d4"

func TestRekorLookupReturnsTheRecordedDigest(t *testing.T) {
	srv := fakeRekor(t, map[string]map[string]any{
		"42": {
			"body":           rekorBody(t, "hashedrekord", "sha256", testHash),
			"logIndex":       42,
			"integratedTime": 1788475734,
			"logID":          "c0d23d6a",
		},
	})

	got, integrated, err := newRekorClient(srv.URL).lookup(context.Background(), "42")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got != testHash {
		t.Errorf("recorded digest = %s, want %s", got, testHash)
	}
	if integrated.IsZero() {
		t.Error("integrated time was not read from the entry")
	}
}

// The finding that matters most: the checkpoint names an index the log
// does not have. It must be reported as "never published", not as a
// generic HTTP error, because those lead a reader to different places.
func TestRekorLookupNamesAMissingEntryPlainly(t *testing.T) {
	srv := fakeRekor(t, map[string]map[string]any{})

	_, _, err := newRekorClient(srv.URL).lookup(context.Background(), "999")
	if err == nil {
		t.Fatal("a missing log entry was not reported")
	}
	// Asserting on the substance, not on an exact sentence: the message
	// must name the index and state that the claimed publication did not
	// happen. Rewording is fine; going vague is not.
	for _, want := range []string{"no entry at index", "claims to have been published"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("a missing entry should say so plainly (missing %q), got: %v", want, err)
		}
	}
}

func TestRekorLookupRejectsMalformedResponses(t *testing.T) {
	cases := []struct {
		name      string
		entry     map[string]any
		wantMatch string
	}{
		{
			name: "index does not match what was asked for",
			entry: map[string]any{
				"body": rekorBody(t, "hashedrekord", "sha256", testHash), "logIndex": 7,
			},
			wantMatch: "answered with index",
		},
		{
			name: "not a hashedrekord",
			entry: map[string]any{
				"body": rekorBody(t, "intoto", "sha256", testHash), "logIndex": 42,
			},
			wantMatch: "not a hashedrekord",
		},
		{
			name: "different digest algorithm",
			entry: map[string]any{
				"body": rekorBody(t, "hashedrekord", "sha512", testHash), "logIndex": 42,
			},
			wantMatch: "sha256",
		},
		{
			name:      "body is not base64",
			entry:     map[string]any{"body": "!!!not base64!!!", "logIndex": 42},
			wantMatch: "base64",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeRekor(t, map[string]map[string]any{"42": tc.entry})
			_, _, err := newRekorClient(srv.URL).lookup(context.Background(), "42")
			if err == nil {
				t.Fatal("a malformed entry was accepted")
			}
			if !strings.Contains(err.Error(), tc.wantMatch) {
				t.Errorf("want an error mentioning %q, got: %v", tc.wantMatch, err)
			}
		})
	}
}

func TestRekorLookupRejectsANonIndexEntryID(t *testing.T) {
	srv := fakeRekor(t, map[string]map[string]any{})
	_, _, err := newRekorClient(srv.URL).lookup(context.Background(), "not-a-number")
	if err == nil || !strings.Contains(err.Error(), "not a log index") {
		t.Errorf("a non-numeric entry id should be refused before any request, got: %v", err)
	}
}

// leafFor builds a canonical leaf preimage the way Verdifax does, with
// the checkpoint hash in the digest position, and returns it alongside
// the sha256 the log would record for it.
//
// The verifier hashes the preimage rather than comparing the checkpoint
// hash directly, because the log never records the checkpoint hash, it
// records sha256 of a leaf containing it. Building the fixture the same
// way the producer does is what keeps these tests honest.
func leafFor(cpHash string) (preimage, logValue string) {
	preimage = "verdifax.ledger.input.v2.attest:1:checkpoint/1/t." + cpHash + ".bind.7"
	sum := sha256.Sum256([]byte(preimage))
	return preimage, hex.EncodeToString(sum[:])
}

func anchored(seq uint64, cpHash, entryID string) attest.ExportedInterval {
	preimage, _ := leafFor(cpHash)
	return attest.ExportedInterval{
		Checkpoint:    attest.Checkpoint{Seq: seq, Hash: cpHash},
		LogEntryID:    entryID,
		LedgerBackend: "rekor",
		LeafPreimage:  preimage,
	}
}

// The whole reason this binary contacts the log: an export whose
// checkpoints are internally perfect but whose leaves are NOT the ones
// published must fail. This is the case a lying-but-consistent export
// produces, and the only check that catches it.
func TestResolveLogEntriesCatchesALeafTheLogDoesNotHave(t *testing.T) {
	srv := fakeRekor(t, map[string]map[string]any{
		"100": {
			// The log holds a different leaf entirely.
			"body":     rekorBody(t, "hashedrekord", "sha256", strings.Repeat("a", 64)),
			"logIndex": 100,
		},
	})

	export := attest.ChainExport{
		Intervals: []attest.ExportedInterval{anchored(1, testHash, "100")},
	}

	results := resolveLogEntries(export, srv.URL, false)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if !results[0].failed() {
		t.Fatalf("a checkpoint whose leaf is not the one in the log was not reported "+
			"as a failure (status %q)", results[0].Status)
	}
	if !strings.Contains(results[0].Detail, "not the same") {
		t.Errorf("the mismatch should be stated in plain language, got: %s", results[0].Detail)
	}
}

// The happy path, end to end: a leaf built the way the producer builds
// it, published under the hash the producer would publish, verifies.
func TestResolveLogEntriesAcceptsAGenuinelyAnchoredCheckpoint(t *testing.T) {
	_, logValue := leafFor(testHash)
	srv := fakeRekor(t, map[string]map[string]any{
		"7": {"body": rekorBody(t, "hashedrekord", "sha256", logValue), "logIndex": 7},
	})

	export := attest.ChainExport{
		Intervals: []attest.ExportedInterval{anchored(1, testHash, "7")},
	}
	got := resolveLogEntries(export, srv.URL, false)[0]
	if !got.verified() {
		t.Fatalf("a correctly anchored checkpoint did not verify: status=%q detail=%s",
			got.Status, got.Detail)
	}
}

// A leaf that IS in the log but does not mention this checkpoint is a
// failure, not a pass. Caught before the network call, because whatever
// the log holds is irrelevant to a checkpoint the leaf never named.
func TestResolveLogEntriesRejectsALeafForADifferentCheckpoint(t *testing.T) {
	srv := fakeRekor(t, map[string]map[string]any{})

	iv := anchored(1, testHash, "100")
	iv.LeafPreimage, _ = leafFor(strings.Repeat("b", 64)) // somebody else's checkpoint

	got := resolveLogEntries(attest.ChainExport{
		Intervals: []attest.ExportedInterval{iv},
	}, srv.URL, false)[0]

	if !got.failed() {
		t.Fatalf("a leaf that does not name this checkpoint was not a failure "+
			"(status %q)", got.Status)
	}
}

// THE DISTINCTION THIS WHOLE FILE TURNS ON.
//
// A checkpoint with no preimage cannot be checked. It must NOT be
// reported as a failure: the record is not known to be wrong, it is
// merely no longer checkable. Calling that tampering would be a
// falsehood in the opposite direction from the one this tool prevents,
// and it is the state every checkpoint anchored before 2026-09-04 is
// permanently in.
func TestResolveLogEntriesReportsAMissingPreimageAsUnverifiableNotFailed(t *testing.T) {
	srv := fakeRekor(t, map[string]map[string]any{})

	iv := anchored(1, testHash, "100")
	iv.LeafPreimage = ""

	got := resolveLogEntries(attest.ChainExport{
		Intervals: []attest.ExportedInterval{iv},
	}, srv.URL, false)[0]

	if got.failed() {
		t.Fatal("a checkpoint that merely cannot be checked was reported as a " +
			"failure, which accuses the record of something it is not known to have done")
	}
	if !got.unverifiable() {
		t.Fatalf("want status %q, got %q", StatusUnverifiable, got.Status)
	}
	if !strings.Contains(got.Detail, "NOT evidence of tampering") {
		t.Errorf("the detail must say plainly that this is not tampering, got: %s",
			got.Detail)
	}
}

// A mock-ledger anchor makes no public-log claim at all. Looking up its
// synthetic id would fail, and reporting that failure would be a finding
// about a claim nobody made.
func TestResolveLogEntriesReportsAMockAnchorAsUnverifiable(t *testing.T) {
	srv := fakeRekor(t, map[string]map[string]any{})

	iv := anchored(1, testHash, "rekor-859bf267c91b8645")
	iv.LedgerBackend = "mock"

	got := resolveLogEntries(attest.ChainExport{
		Intervals: []attest.ExportedInterval{iv},
	}, srv.URL, false)[0]

	if !got.unverifiable() {
		t.Fatalf("a mock-ledger anchor should be unverifiable, got %q: %s",
			got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "not a finding") {
		t.Errorf("the detail should say this is not a finding about the record, got: %s",
			got.Detail)
	}
}

// One unreachable or missing entry must not hide the state of the others.
// "One checkpoint is missing from the log" and "none of them are in it"
// are very different findings.
func TestResolveLogEntriesReportsEveryCheckpoint(t *testing.T) {
	_, logValue := leafFor(testHash)
	srv := fakeRekor(t, map[string]map[string]any{
		"1": {"body": rekorBody(t, "hashedrekord", "sha256", logValue), "logIndex": 1},
		"3": {"body": rekorBody(t, "hashedrekord", "sha256", logValue), "logIndex": 3},
	})

	noEntry := anchored(4, testHash, "")
	export := attest.ChainExport{Intervals: []attest.ExportedInterval{
		anchored(1, testHash, "1"),
		anchored(2, testHash, "2"), // absent from the log
		anchored(3, testHash, "3"),
		noEntry,
	}}

	results := resolveLogEntries(export, srv.URL, false)
	if len(results) != 4 {
		t.Fatalf("every checkpoint should be reported; got %d of 4", len(results))
	}
	want := []string{StatusVerified, StatusFailed, StatusVerified, StatusFailed}
	for i, w := range want {
		if results[i].Status != w {
			t.Errorf("checkpoint %d: status=%q, want %q (%s)",
				results[i].Seq, results[i].Status, w, results[i].Detail)
		}
	}
	// The one with no entry id at all gets its own explanation.
	if !strings.Contains(results[3].Detail, "no sign it ever was") {
		t.Errorf("a checkpoint with no log entry should say there is no sign it was "+
			"published, got: %s", results[3].Detail)
	}
}

func TestRekorLookupSurfacesAnUnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "upstream exploded")
	}))
	t.Cleanup(srv.Close)

	_, _, err := newRekorClient(srv.URL).lookup(context.Background(), "1")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("a 500 from the log should be surfaced, got: %v", err)
	}
}
