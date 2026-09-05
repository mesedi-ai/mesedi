package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"mesedi/backend/internal/attest"
)

// Offline verification of a checkpoint's anchor, using the Merkle
// inclusion proof captured at anchor time instead of a live call to the
// transparency log.
//
// WHAT THIS ADDS: the export becomes self-contained evidence. An auditor
// on a closed network can verify it, and a checkpoint stays verifiable
// even if the log it was written to is later retired (task #22) — the
// signature over the tree head does not expire when the server does.
//
// THE STEP THAT MAKES IT MEAN ANYTHING, AND THE ONE EASIEST TO OMIT
//
// VerifyAnchor answers exactly one question: is this log entry committed
// under a root the log signed? It does NOT answer whether the entry has
// anything to do with our checkpoint. Read rekorproof.go: when EntryBody
// is set it computes the Merkle leaf from the body and never looks at
// LeafHashHex at all. So a proof lifted wholesale from an unrelated
// Rekor entry passes VerifyAnchor completely — the proof is genuine, it
// simply is not about us.
//
// Closing that hole takes one comparison, done here in bindEntryToLeaf:
// the entry's own spec.data.hash.value must equal sha256(leaf preimage).
// Chained with the caller's existing check that the preimage contains
// the checkpoint hash, the result is a complete path with no gap:
//
//	checkpoint hash → inside leaf preimage        (caller)
//	leaf preimage   → sha256 → entry's data hash  (bindEntryToLeaf)
//	entry body      → Merkle leaf → signed root   (VerifyAnchor)
//	signed root     → Rekor's pinned public key   (VerifyAnchor)
//
// Drop the middle link and every remaining check still passes, on
// somebody else's entry. That is the same defect verdifax-verify shipped
// with (task #27): the recipe was documented and never executed.

// anchorProof is the envelope Mesedi stores in
// checkpoints.anchor_proof_json and emits as ExportedInterval.AnchorProof.
type anchorProof struct {
	LogID          string          `json:"log_id"`
	EntryBody      string          `json:"entry_body"`
	InclusionProof json.RawMessage `json:"inclusion_proof"`
}

// wireInclusionProof mirrors the proof as Verdifax serialises it.
//
// Deliberately WITHOUT json tags. Verdifax's pkg/ledger.InclusionProof
// has no tags either, so its field names on the wire are whatever Go's
// default marshalling produces — an accident, not a designed contract.
// Leaving the tags off here means encoding/json falls back to
// case-insensitive matching, so this keeps parsing if that struct ever
// gains explicit lower-case tags. It would NOT survive a rename to
// snake_case, which is why the source side pins the names with a test.
type wireInclusionProof struct {
	LogIndex   int64
	RootHash   string
	TreeSize   int64
	Hashes     []string
	Checkpoint string
}

// The entry body is decoded with rekor.go's hashedRekordBody rather than
// a second copy of the same struct. Deliberate: both paths must agree on
// what "the digest this entry records" means, and two declarations of
// that would be two places to change when Rekor's schema moves.

// offlineResult is what an offline attempt concluded.
//
// Decided=false means "this did not settle the question" and the caller
// should fall back to asking the log. Note carries why, so a proof that
// was present but unusable does not vanish from the report — silence
// there would read as though no proof had been offered.
type offlineResult struct {
	Decided bool
	Status  string
	Detail  string
	Note    string
}

// verifyAnchorOffline checks a checkpoint's anchor using only the export
// and the Rekor public key embedded in this binary.
//
// The caller must already have established that the preimage is present
// and contains the checkpoint hash; this function assumes both and
// checks what remains.
func verifyAnchorOffline(iv attest.ExportedInterval) offlineResult {
	if len(iv.AnchorProof) == 0 {
		return offlineResult{}
	}

	var env anchorProof
	if err := json.Unmarshal(iv.AnchorProof, &env); err != nil {
		return offlineResult{Note: "the export carries an anchor proof that is not " +
			"valid JSON, so it was ignored"}
	}
	if env.EntryBody == "" || len(env.InclusionProof) == 0 {
		return offlineResult{Note: "the export's anchor proof is incomplete " +
			"(no entry body or no inclusion proof), so it could not be checked offline"}
	}

	// A proof from a log this binary does not have the key for is not a
	// finding about the record. Fall through and let the online path try,
	// rather than reporting a failure that would be about our key.
	embedded, err := EmbeddedLogID()
	if err != nil {
		return offlineResult{Note: fmt.Sprintf(
			"this binary's embedded Rekor key could not be parsed (%v), so offline "+
				"verification was not attempted", err)}
	}
	if env.LogID != "" && !strings.EqualFold(env.LogID, embedded) {
		return offlineResult{Note: fmt.Sprintf(
			"the anchor proof was issued by log %s, but this binary carries the key "+
				"for log %s. Offline verification was skipped; this is a statement "+
				"about the verifier, NOT about the record",
			shorten(env.LogID), shorten(embedded))}
	}

	var proof wireInclusionProof
	if err := json.Unmarshal(env.InclusionProof, &proof); err != nil {
		return offlineResult{Note: "the anchor proof's inclusion proof could not be " +
			"parsed, so it was ignored"}
	}

	// THE BINDING STEP. See the file comment: without this the rest
	// proves somebody else's entry is in the log.
	if res, ok := bindEntryToLeaf(env.EntryBody, iv.LeafPreimage); !ok {
		return res
	}

	if err := VerifyAnchor(AnchorInput{
		LeafHashHex:   leafHashOf(iv.LeafPreimage),
		EntryBody:     env.EntryBody,
		LogIndex:      proof.LogIndex,
		TreeSize:      proof.TreeSize,
		RootHashHex:   proof.RootHash,
		InclusionPath: proof.Hashes,
		Checkpoint:    proof.Checkpoint,
		LogID:         env.LogID,
	}); err != nil {
		return offlineResult{
			Decided: true,
			Status:  StatusFailed,
			Detail: fmt.Sprintf("the inclusion proof carried in this export does not "+
				"verify against Sigstore's published key: %v", err),
		}
	}

	return offlineResult{
		Decided: true,
		Status:  StatusVerified,
		Detail: fmt.Sprintf("the leaf committing to this checkpoint is proven present "+
			"in the log at index %d, under a tree head of %d entries that Sigstore "+
			"signed. Checked offline against a key compiled into this binary; the "+
			"log was not contacted", proof.LogIndex, proof.TreeSize),
	}
}

// bindEntryToLeaf confirms the log entry is ABOUT this checkpoint's leaf.
//
// Returns ok=true when the binding holds. When it does not, the returned
// result is a decided FAILURE and not a fall-through: a proof that
// verifies against an entry describing something else is a contradiction
// inside the export itself, and asking the log about it would not make
// the contradiction go away.
func bindEntryToLeaf(entryBodyB64, leafPreimage string) (offlineResult, bool) {
	raw, err := base64.StdEncoding.DecodeString(entryBodyB64)
	if err != nil {
		return offlineResult{Note: "the anchor proof's entry body is not valid " +
			"base64, so it could not be checked offline"}, false
	}
	var body hashedRekordBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return offlineResult{Note: "the anchor proof's entry body is not valid JSON, " +
			"so it could not be checked offline"}, false
	}

	recorded := body.Spec.Data.Hash.Value
	if recorded == "" {
		return offlineResult{Note: "the anchor proof's entry body records no data " +
			"hash, so there is nothing to bind it to this checkpoint"}, false
	}
	if alg := body.Spec.Data.Hash.Algorithm; alg != "" && !strings.EqualFold(alg, "sha256") {
		return offlineResult{Note: fmt.Sprintf(
			"the log entry hashes with %q, not sha256, so this verifier cannot "+
				"reproduce it", alg)}, false
	}

	computed := leafHashOf(leafPreimage)
	if !strings.EqualFold(recorded, computed) {
		return offlineResult{
			Decided: true,
			Status:  StatusFailed,
			Detail: fmt.Sprintf("the log entry proven present records %s, but this "+
				"checkpoint's leaf hashes to %s. The proof is about a different "+
				"record, so it says nothing about this checkpoint",
				shorten(recorded), shorten(computed)),
		}, false
	}
	return offlineResult{}, true
}

func leafHashOf(preimage string) string {
	sum := sha256.Sum256([]byte(preimage))
	return hex.EncodeToString(sum[:])
}
