package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The offline-verification proof: log_id, entry_body and the inclusion
// proof, captured at anchor time and stored with the checkpoint.
//
// TWO OPPOSITE MISTAKES ARE POSSIBLE HERE and these tests exist to
// catch both, because each one looks like caution from the other side.
//
// Recording a PARTIAL proof is the first. A proof with no entry body
// makes the Merkle walk start from the wrong value, so the walk fails ,
// and a verifier reporting a failed walk is saying "this does not
// match", which is a claim about tampering. The truth is "this cannot
// be checked". Those must not be confused, so a partial envelope is not
// stored at all.
//
// REFUSING THE ANCHOR when the proof is absent is the second, and it is
// the worse of the two. The scheduler treats an error from the anchorer
// as "not anchored" and retries on every tick, so a refusal here stops
// checkpointing for every tenant. That is task #32 exactly: one
// event-less execution halting the whole chain. A missing proof costs
// offline verification and nothing else, the anchor is still fully
// checkable by asking the log, and it must never cost the chain.

const testInclusionProof = `{"logIndex":2718374165,"rootHash":"dd44",` +
	`"treeSize":2718374166,"hashes":["ee55","ff66"],"checkpoint":"rekor.sigstore.dev\n1\n"}`

func TestBuildAnchorProof_RequiresAllThreeParts(t *testing.T) {
	full := verdifaxAttestResponse{
		LogID:          "c0d23d6ad406973f9559f3ba2d1ca01f84147d8ffc5b8445c224f98b9591801d",
		EntryBody:      "eyJhcGlWZXJzaW9uIjoiMC4wLjEifQ==",
		InclusionProof: json.RawMessage(testInclusionProof),
	}

	for _, tc := range []struct {
		name string
		drop func(*verdifaxAttestResponse)
		why  string
	}{
		{"no inclusion proof", func(r *verdifaxAttestResponse) { r.InclusionProof = nil },
			"there is no path to walk"},
		{"no entry body", func(r *verdifaxAttestResponse) { r.EntryBody = "" },
			"the walk would start from the wrong leaf and fail, and a failed walk " +
				"reads as tampering rather than as a missing field"},
		{"no log id", func(r *verdifaxAttestResponse) { r.LogID = "" },
			"the verifier could not tell which log signed the checkpoint the proof " +
				"ends at, and would have to assume the one it has pinned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := full
			tc.drop(&r)
			if got := buildAnchorProof(r); got != "" {
				t.Errorf("a partial proof was assembled (%s): %s", tc.why, got)
			}
		})
	}

	got := buildAnchorProof(full)
	if got == "" {
		t.Fatal("a complete receipt produced no proof envelope")
	}
	var env struct {
		LogID          string          `json:"log_id"`
		EntryBody      string          `json:"entry_body"`
		InclusionProof json.RawMessage `json:"inclusion_proof"`
	}
	if err := json.Unmarshal([]byte(got), &env); err != nil {
		t.Fatalf("the envelope is not valid JSON: %v", err)
	}
	if env.LogID != full.LogID || env.EntryBody != full.EntryBody {
		t.Errorf("envelope lost a field: %+v", env)
	}
	// The proof must survive as the log produced it. Mesedi re-encoding
	// somebody else's evidence would silently drop any field Sigstore or
	// Verdifax adds later.
	var a, b map[string]any
	if err := json.Unmarshal(env.InclusionProof, &a); err != nil {
		t.Fatalf("inclusion proof did not survive as JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(testInclusionProof), &b); err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Errorf("the inclusion proof lost fields in transit: got %d, sent %d",
			len(a), len(b))
	}
	if a["rootHash"] != "dd44" {
		t.Errorf("the inclusion proof's root hash did not survive: %v", a["rootHash"])
	}
}

// receiptWithProof writes a well-formed receipt that also carries the
// three offline-verification fields.
func receiptWithProof(w http.ResponseWriter, digest, provenance, entryID string) {
	preimage := anchorLeafPreimage(digest)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":              true,
		"attestation_id":  42,
		"digest":          digest,
		"ledger_backend":  "rekor",
		"log_entry_id":    entryID,
		"leaf_preimage":   preimage,
		"leaf_hash":       anchorLeafHash(preimage),
		"provenance":      provenance,
		"log_id":          "c0d23d6ad406973f9559f3ba2d1ca01f84147d8ffc5b8445c224f98b9591801d",
		"entry_body":      "eyJhcGlWZXJzaW9uIjoiMC4wLjEifQ==",
		"inclusion_proof": json.RawMessage(testInclusionProof),
	})
}

func TestAnchorer_CapturesTheProofFromTheReceipt(t *testing.T) {
	cp := anchorTestCheckpoint(t)
	a, done := anchorerAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		receiptWithProof(w, cp.Hash, expectedProvenance, "2718374165")
	})
	defer done()

	anchor, err := a.AnchorCheckpoint(t.Context(), cp, 0)
	if err != nil {
		t.Fatalf("AnchorCheckpoint: %v", err)
	}
	if anchor.AnchorProofJSON == "" {
		t.Fatal("the receipt carried a complete proof and the anchor recorded none; " +
			"this checkpoint would be verifiable only by contacting the log")
	}
	for _, want := range []string{"log_id", "entry_body", "inclusion_proof", "dd44"} {
		if !strings.Contains(anchor.AnchorProofJSON, want) {
			t.Errorf("the stored proof is missing %q: %s", want, anchor.AnchorProofJSON)
		}
	}
	// The proof must not have displaced anything that was already
	// working.
	if anchor.LeafPreimage == "" {
		t.Error("the leaf preimage was lost while adding the proof")
	}
	if anchor.LogEntryID != "2718374165" {
		t.Errorf("log entry id = %q", anchor.LogEntryID)
	}
}

// The chain must keep moving when the proof is absent. This is the
// regression guard for the failure that matters more than the feature.
func TestAnchorer_AnchorsWithoutAProofRatherThanHaltingTheChain(t *testing.T) {
	cp := anchorTestCheckpoint(t)
	a, done := anchorerAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		// A receipt from a Verdifax older than the proof fields, which is
		// exactly what production returns until it is redeployed.
		okReceipt(w, cp.Hash, expectedProvenance, "2717214602")
	})
	defer done()

	anchor, err := a.AnchorCheckpoint(t.Context(), cp, 0)
	if err != nil {
		t.Fatalf("a receipt with no offline proof halted the anchor: %v.\n"+
			"A missing proof costs offline verification only, the anchor is still "+
			"checkable against the log, and refusing it stops checkpointing for "+
			"every tenant on every tick", err)
	}
	if !anchor.Anchored {
		t.Error("the checkpoint was not recorded as anchored")
	}
	if anchor.AnchorProofJSON != "" {
		t.Errorf("a proof was invented from a receipt that carried none: %s",
			anchor.AnchorProofJSON)
	}
}

// A receipt carrying some but not all of the proof material must be
// recorded as having none, not as having a proof that cannot be walked.
func TestAnchorer_DoesNotStoreAHalfProof(t *testing.T) {
	cp := anchorTestCheckpoint(t)
	a, done := anchorerAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		preimage := anchorLeafPreimage(cp.Hash)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "digest": cp.Hash, "ledger_backend": "rekor",
			"log_entry_id": "2718374165", "leaf_preimage": preimage,
			"leaf_hash": anchorLeafHash(preimage), "provenance": expectedProvenance,
			// Proof present, entry body absent: unwalkable.
			"inclusion_proof": json.RawMessage(testInclusionProof),
			"log_id":          "c0d23d6ad406973f9559f3ba2d1ca01f84147d8ffc5b8445c224f98b9591801d",
		})
	})
	defer done()

	anchor, err := a.AnchorCheckpoint(t.Context(), cp, 0)
	if err != nil {
		t.Fatalf("a partial proof halted the anchor: %v", err)
	}
	if anchor.AnchorProofJSON != "" {
		t.Errorf("an unwalkable proof was stored; a verifier would report it as a "+
			"MISMATCH when the truth is that it cannot be checked: %s",
			anchor.AnchorProofJSON)
	}
}

func TestRawJSONOrNil_DropsWhatAVerifierCouldNotUse(t *testing.T) {
	if got := rawJSONOrNil(""); got != nil {
		t.Errorf("empty produced %q, want nil so the field is omitted entirely", got)
	}
	if got := rawJSONOrNil(`{"log_id":`); got != nil {
		t.Errorf("malformed JSON was passed through as %q. It would either corrupt "+
			"the enclosing export or reach a verifier as a broken proof, and the "+
			"only true statement about a proof that will not parse is that there "+
			"is not one", got)
	}
	if got := rawJSONOrNil(`{"log_id":"x"}`); string(got) != `{"log_id":"x"}` {
		t.Errorf("valid JSON was altered in transit: %s", got)
	}
}
