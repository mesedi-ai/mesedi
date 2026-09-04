package main

import (
	"context"
	"encoding/base64"
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

// The whole reason this binary contacts the log: an export whose
// checkpoints are internally perfect but whose hashes are NOT the ones
// published must fail. This is the case a lying-but-consistent export
// produces, and the only check that catches it.
func TestResolveLogEntriesCatchesAHashTheLogDoesNotHave(t *testing.T) {
	otherHash := strings.Repeat("a", 64)
	srv := fakeRekor(t, map[string]map[string]any{
		"100": {
			"body": rekorBody(t, "hashedrekord", "sha256", otherHash), "logIndex": 100,
		},
	})

	export := attest.ChainExport{
		Intervals: []attest.ExportedInterval{{
			Checkpoint: attest.Checkpoint{Seq: 1, Hash: testHash},
			LogEntryID: "100",
		}},
	}

	results := resolveLogEntries(export, srv.URL)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].OK {
		t.Fatal("a checkpoint whose hash is not the one in the log was accepted")
	}
	if !strings.Contains(results[0].Detail, "not the same") {
		t.Errorf("the mismatch should be stated in plain language, got: %s", results[0].Detail)
	}
}

// One unreachable or missing entry must not hide the state of the others.
// "One checkpoint is missing from the log" and "none of them are in it"
// are very different findings.
func TestResolveLogEntriesReportsEveryCheckpoint(t *testing.T) {
	srv := fakeRekor(t, map[string]map[string]any{
		"1": {"body": rekorBody(t, "hashedrekord", "sha256", testHash), "logIndex": 1},
		"3": {"body": rekorBody(t, "hashedrekord", "sha256", testHash), "logIndex": 3},
	})

	mk := func(seq uint64, entryID string) attest.ExportedInterval {
		return attest.ExportedInterval{
			Checkpoint: attest.Checkpoint{Seq: seq, Hash: testHash},
			LogEntryID: entryID,
		}
	}
	export := attest.ChainExport{Intervals: []attest.ExportedInterval{
		mk(1, "1"), mk(2, "2"), mk(3, "3"), mk(4, ""),
	}}

	results := resolveLogEntries(export, srv.URL)
	if len(results) != 4 {
		t.Fatalf("every checkpoint should be reported; got %d of 4", len(results))
	}
	want := []bool{true, false, true, false}
	for i, w := range want {
		if results[i].OK != w {
			t.Errorf("checkpoint %d: ok=%v, want %v (%s)",
				results[i].Seq, results[i].OK, w, results[i].Detail)
		}
	}
	// The one with no entry id at all gets its own explanation.
	if !strings.Contains(results[3].Detail, "never published") {
		t.Errorf("a checkpoint with no log entry should say it was never published, got: %s",
			results[3].Detail)
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
