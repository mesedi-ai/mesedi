package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"mesedi/backend/internal/attest"
)

// WHAT IS NOT TESTED HERE, AND WHY THAT IS THE POINT
//
// There is no test that produces StatusVerified, and one cannot be
// written. A verified result requires a valid ECDSA signature by
// Sigstore over a Merkle tree head, checked against the public key
// compiled into this binary. Synthesising one would mean holding
// Sigstore's private key. That this file cannot fake a pass is the
// property working: a forged proof does not verify.
//
// The consequence is that the green path is provable only against a real
// production checkpoint, not in CI. Everything else, every way a proof
// can be absent, malformed, from the wrong log, or about the wrong
// record, is covered below, and the one that matters most is
// TestOfflineRefusesAProofAboutADifferentRecord.

// rekorEntryBodyFor builds a base64 hashedrekord body that records the
// sha256 of the given preimage, the way Rekor stores one.
func rekorEntryBodyFor(preimage string) string {
	sum := sha256.Sum256([]byte(preimage))
	body := fmt.Sprintf(
		`{"apiVersion":"0.0.1","kind":"hashedrekord","spec":{"data":{"hash":`+
			`{"algorithm":"sha256","value":%q}},"signature":{"content":"AA=="}}}`,
		hex.EncodeToString(sum[:]))
	return base64.StdEncoding.EncodeToString([]byte(body))
}

func proofEnvelope(t *testing.T, logID, entryBody string, proof map[string]any) json.RawMessage {
	t.Helper()
	ip, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	env, err := json.Marshal(map[string]any{
		"log_id":          logID,
		"entry_body":      entryBody,
		"inclusion_proof": json.RawMessage(ip),
	})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func productionLogID(t *testing.T) string {
	t.Helper()
	id, err := EmbeddedLogID()
	if err != nil {
		t.Fatalf("EmbeddedLogID: %v", err)
	}
	return id
}

const testPreimage = "verdifax.ledger.input.v2.attest:1:checkpoint/12/t." +
	"1111111111111111111111111111111111111111111111111111111111111111.bind.7"

func intervalWithProof(preimage string, proof json.RawMessage) attest.ExportedInterval {
	return attest.ExportedInterval{
		LogEntryID:    "2718374165",
		LedgerBackend: "rekor",
		LeafPreimage:  preimage,
		AnchorProof:   proof,
	}
}

// THE ONE THAT MATTERS.
//
// A Merkle inclusion proof says an entry is in the log. It says nothing
// about whether that entry concerns your checkpoint, read rekorproof.go:
// when EntryBody is set, VerifyAnchor computes the leaf from the body and
// never looks at LeafHashHex. So a real, valid, unmodified proof lifted
// from an unrelated Rekor entry passes every cryptographic check in it.
//
// Only the binding step catches that. If this test ever starts passing
// for the wrong reason, the verifier is confidently green about somebody
// else's data, which is worse than being wrong: it is being wrong in a
// way an auditor has no way to notice.
func TestOfflineRefusesAProofAboutADifferentRecord(t *testing.T) {
	// A genuine entry body, for a completely different preimage.
	somebodyElse := rekorEntryBodyFor("verdifax.ledger.input.v2.env.SOMEONE-ELSE.bind.9")

	res := verifyAnchorOffline(intervalWithProof(testPreimage,
		proofEnvelope(t, productionLogID(t), somebodyElse, map[string]any{
			"LogIndex": 2718374165, "TreeSize": 2718374166,
			"RootHash": strings.Repeat("dd", 32), "Hashes": []string{},
			"Checkpoint": "rekor.sigstore.dev\n1\n",
		})))

	if !res.Decided {
		t.Fatal("a proof about a different record was left undecided; it would fall " +
			"through to a log lookup and could be reported as fine")
	}
	if res.Status != StatusFailed {
		t.Errorf("status = %q, want %q. The export's own proof points at an entry "+
			"describing something else, which is a contradiction inside the export "+
			"and not a reason to go ask the log", res.Status, StatusFailed)
	}
	if !strings.Contains(res.Detail, "different record") {
		t.Errorf("the detail does not tell the reader the proof is about another "+
			"record: %s", res.Detail)
	}
}

// The mirror of the above: when the entry body DOES record this
// checkpoint's leaf, the binding passes and the failure that remains is
// the cryptographic one, not a binding complaint. This is what proves
// the previous test is failing for the right reason.
func TestOfflineGetsPastTheBindingWhenTheEntryIsOurs(t *testing.T) {
	res := verifyAnchorOffline(intervalWithProof(testPreimage,
		proofEnvelope(t, productionLogID(t), rekorEntryBodyFor(testPreimage),
			map[string]any{
				"LogIndex": 2718374165, "TreeSize": 2718374166,
				"RootHash": strings.Repeat("dd", 32), "Hashes": []string{},
				"Checkpoint": "rekor.sigstore.dev\n1\n",
			})))

	if !res.Decided || res.Status != StatusFailed {
		t.Fatalf("expected a decided cryptographic failure, got %+v", res)
	}
	if strings.Contains(res.Detail, "different record") {
		t.Error("the binding rejected an entry that records exactly this checkpoint's " +
			"leaf; the binding check is too strict and would fail valid proofs")
	}
	if !strings.Contains(res.Detail, "does not check out") {
		t.Errorf("expected the failure to come from proof verification, got: %s",
			res.Detail)
	}
}

// Rekor's global entry id and the proof's in-tree position are
// different numbers for the same entry, on production, 2725800899 and
// 2603896637, a gap of ~122 million being the earlier shards. The
// report shows both. If it does not say they are different measures, an
// auditor reads the pair as the verifier having caught something.
//
// The explanation lives in the SECTION SUMMARY, not on each row. It used
// to be repeated in full on every checkpoint, which cost six lines per
// row and pushed the document past two pages while saying the same thing
// four times. Said once, it still has to be said.
func TestTheReportExplainsTheTwoIndexSpacesExactlyOnce(t *testing.T) {
	summary := logConfirmation(entriesFor(4, 4, 4))
	for _, want := range []string{
		"Position numbers count within the volume", // what the small number is
		"count across every volume",                // what the big number is
		"not meant to match",                       // that the difference is expected
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary no longer contains %q, so the report prints two "+
				"unequal numbers for one entry with nothing telling the reader they "+
				"are different measures:\n%s", want, summary)
		}
	}

	// And NOT on the individual rows, which is what made it four times as
	// long as it needed to be.
	src, err := os.ReadFile("offline.go")
	if err != nil {
		t.Fatalf("read offline.go: %v", err)
	}
	if strings.Contains(string(src), "count across every volume") {
		t.Error("the per-checkpoint detail repeats the two-index explanation. Said " +
			"once per row it costs six lines each and pushes the report past two pages")
	}

	// The equality check that must never be added.
	if strings.Contains(string(src), "proof.LogIndex != ") ||
		strings.Contains(string(src), "!= proof.LogIndex") {
		t.Error("something compares the proof's in-tree index against another index. " +
			"They live in different spaces and are never equal; such a check would " +
			"fail on every genuine Sigstore entry")
	}
}

// Everything that should leave the question OPEN rather than answer it.
// Each of these must fall through to a log lookup, because none of them
// is a statement about the record, they are statements about the proof
// or about this binary.
func TestOfflineFallsThroughRatherThanAccusing(t *testing.T) {
	good := rekorEntryBodyFor(testPreimage)
	logID := productionLogID(t)
	fullProof := map[string]any{
		"LogIndex": 1, "TreeSize": 2, "RootHash": strings.Repeat("dd", 32),
		"Hashes": []string{}, "Checkpoint": "x",
	}

	cases := []struct {
		name     string
		proof    json.RawMessage
		wantNote string
	}{
		{"no proof at all", nil, ""},
		{"proof is not JSON", json.RawMessage(`{not json`), "not valid JSON"},
		{"no entry body", proofEnvelope(t, logID, "", fullProof), "incomplete"},
		{"entry body is not base64", proofEnvelope(t, logID, "!!!!", fullProof),
			"not valid base64"},
		{"entry body is not JSON",
			proofEnvelope(t, logID, base64.StdEncoding.EncodeToString([]byte("hello")), fullProof),
			"not valid JSON"},
		{"entry body records no hash",
			proofEnvelope(t, logID, base64.StdEncoding.EncodeToString([]byte(`{"kind":"x"}`)), fullProof),
			"no data hash"},
		{"proof from a different log",
			proofEnvelope(t, strings.Repeat("ab", 32), good, fullProof),
			"NOT about the record"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := verifyAnchorOffline(intervalWithProof(testPreimage, tc.proof))
			if res.Decided {
				t.Fatalf("this settled the question when it should have fallen through "+
					"to a log lookup: %+v", res)
			}
			if tc.wantNote == "" {
				if res.Note != "" {
					t.Errorf("no proof was offered, yet a note was recorded: %s", res.Note)
				}
				return
			}
			if res.Note == "" {
				t.Fatal("an unusable proof was offered and silently ignored; the report " +
					"would look identical to one where no proof existed")
			}
			if !strings.Contains(res.Note, tc.wantNote) {
				t.Errorf("note does not say %q: %s", tc.wantNote, res.Note)
			}
		})
	}
}

// A proof from another log is a statement about this verifier's key, not
// about the record. Saying otherwise would accuse a record of something
// on the strength of which key happens to be compiled in.
func TestOfflineDoesNotBlameTheRecordForTheVerifiersKey(t *testing.T) {
	res := verifyAnchorOffline(intervalWithProof(testPreimage,
		proofEnvelope(t, strings.Repeat("ab", 32), rekorEntryBodyFor(testPreimage),
			map[string]any{"LogIndex": 1, "TreeSize": 2})))

	if res.Status == StatusFailed {
		t.Error("a proof from an unrecognised log was reported as a FAILURE, which " +
			"reads as a finding against the record")
	}
	if !strings.Contains(res.Note, "NOT about the record") {
		t.Errorf("the note does not make clear this is about the verifier: %s", res.Note)
	}
}

// Verdifax serialises pkg/ledger.InclusionProof with no JSON tags, so its
// field names on the wire are Go's defaults. This parser has no tags
// either, which makes encoding/json match case-insensitively, so it
// keeps working if that struct ever gains lower-case tags. Pinned here
// because the tolerance is a property of leaving tags off, and someone
// "tidying up" by adding them would remove it.
func TestOfflineParsesTheProofWhateverTheCaseOfItsKeys(t *testing.T) {
	for _, keys := range []map[string]any{
		{"LogIndex": 7, "TreeSize": 8, "RootHash": strings.Repeat("dd", 32),
			"Hashes": []string{}, "Checkpoint": "x"},
		{"logIndex": 7, "treeSize": 8, "rootHash": strings.Repeat("dd", 32),
			"hashes": []string{}, "checkpoint": "x"},
	} {
		res := verifyAnchorOffline(intervalWithProof(testPreimage,
			proofEnvelope(t, productionLogID(t), rekorEntryBodyFor(testPreimage), keys)))
		// Both must reach the cryptographic check, i.e. get past parsing.
		if !res.Decided {
			t.Errorf("proof with keys %v was not parsed: note=%q", keys, res.Note)
		}
	}
}

// An offline run must never reach the network. Passing a nil client
// rather than a boolean means an accidental lookup panics here instead
// of quietly dialling out during a run an operator asked to keep local.
func TestOfflineRunNeverDialsOut(t *testing.T) {
	export := attest.ChainExport{Intervals: []attest.ExportedInterval{
		intervalWithProof(testPreimage, nil),
	}}
	export.Intervals[0].Checkpoint.Hash =
		"1111111111111111111111111111111111111111111111111111111111111111"

	// A URL that would fail loudly if it were ever used.
	got := resolveLogEntries(export, "http://127.0.0.1:1/should-never-be-called", true)
	if len(got) != 1 {
		t.Fatalf("want 1 result, got %d", len(got))
	}
	if got[0].Status != StatusUnverifiable {
		t.Errorf("status = %q, want %q: no proof and no network means the checkpoint "+
			"was not checked, and saying anything else would misrepresent the run",
			got[0].Status, StatusUnverifiable)
	}
	if !strings.Contains(got[0].Detail, "--offline") {
		t.Errorf("the detail does not tell the reader how to get a real answer: %s",
			got[0].Detail)
	}
}
