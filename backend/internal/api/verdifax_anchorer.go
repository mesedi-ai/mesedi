package api

// VerdifaxAnchorer submits a checkpoint hash to Verdifax, which writes
// it to a public transparency log and returns where it landed.
//
// This is the last link in the chain, and it is a thin one on purpose.
// Verdifax receives a hash and nothing else: no payloads, no execution
// ids, not even a project id. It cannot interpret what the checkpoint
// covers, which is correct, because it is not supposed to. Its receipt
// says one thing, this exact digest existed at this time and has not
// changed since, and the independence in that claim comes from
// Sigstore's log, not from Verdifax.
//
// TWO RESPONSE CHECKS ARE THE POINT OF THIS FILE
//
//  1. The returned digest must equal the digest sent. A misconfigured
//     or hostile endpoint returning a receipt for a DIFFERENT digest
//     would otherwise be recorded as ours, and the chain would name a
//     log entry attesting to something else entirely. Nothing later
//     would catch it: every hash in the chain would still verify
//     against itself.
//
//  2. The returned provenance must be same-legal-entity-submitted.
//     Verdifax, LLC does business as Mesedi, one Delaware LLC, so a
//     receipt claiming a third party submitted this is false. If
//     Verdifax says otherwise, the API key is misclassified, and every
//     receipt it produces asserts an independent party that does not
//     exist. Refusing here turns that misconfiguration into a loud
//     failure at the first anchor instead of a quiet falsehood in
//     every receipt afterwards.
//
// NO INTERNAL RETRY. The scheduler already retries on the next tick,
// and every accepted submission in rekor mode costs real money, so
// retrying here would multiply the bill for one interval. A lost
// response produces a duplicate submission next tick, which Verdifax
// deliberately permits: its digest index is non-unique because the same
// digest anchored twice is two distinct events at two distinct times.
// Duplicates are visible in the log rather than corrupting.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"mesedi/backend/internal/attest"
	"mesedi/backend/internal/store"
)

const (
	// VerdifaxUserAgent identifies this client in Verdifax's logs.
	VerdifaxUserAgent = "Mesedi-CheckpointAnchor/1"

	// verdifaxAnchorTimeout bounds a single submission. An unbounded
	// call from the scheduler would stall the worker indefinitely, and a
	// stalled scheduler stops closing intervals, the failure the whole
	// chain exists to prevent. Generous because a real Sigstore write
	// is not fast, but finite.
	verdifaxAnchorTimeout = 30 * time.Second

	// verdifaxMaxResponseBytes caps what we will read back. A receipt is
	// small; anything large is a misconfigured endpoint, and reading it
	// unbounded into a scheduler's memory is how one bad response takes
	// down the worker.
	verdifaxMaxResponseBytes = 64 << 10

	// expectedProvenance is what a Mesedi submission must come back as.
	// See the header: anything else means the key is misclassified.
	//
	// Duplicated as a literal rather than imported from
	// verdifax-orchestrator's store package. That module is private and
	// Mesedi does not depend on it; importing it to share one string
	// would couple the two products' build graphs for no benefit. The
	// cost is that this literal must track store.ProvenanceSameLegal
	// EntitySubmitted, and the mitigation is that a drift fails LOUDLY
	// at the first anchor rather than silently, which is the direction
	// you want a mismatch to fail in.
	expectedProvenance = "same-legal-entity-submitted"

	// verdifaxKeyHeader is Verdifax's auth header, matching
	// auth.HeaderName in verdifax-orchestrator.
	verdifaxKeyHeader = "X-Verdifax-Key"
)

// VerdifaxAnchorer implements CheckpointAnchorer over HTTP.
type VerdifaxAnchorer struct {
	// BaseURL of the Verdifax orchestrator, e.g. https://api.verdifax.com
	BaseURL string

	// APIKey authenticates to Verdifax. SECRET: never logged, never
	// placed in an error message. Errors from this file quote status
	// codes and digests, never headers.
	APIKey string

	// HTTPClient is injected so tests do not reach the network. Nil
	// gets a client with an explicit timeout.
	HTTPClient *http.Client

	Logger *slog.Logger
}

// NewVerdifaxAnchorer builds an anchorer, or returns nil when it is not
// configured.
//
// Returning nil rather than a half-configured client is deliberate: the
// scheduler treats a nil anchorer as "stall visibly at genesis", which
// is the correct behaviour for a deployment with no transparency log.
// A client that exists but cannot authenticate would instead fail on
// every interval forever, which looks like an outage rather than like a
// deployment that was never given credentials.
func NewVerdifaxAnchorer(baseURL, apiKey string, logger *slog.Logger) *VerdifaxAnchorer {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	apiKey = strings.TrimSpace(apiKey)
	if baseURL == "" || apiKey == "" {
		return nil
	}
	return &VerdifaxAnchorer{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Logger:  logger,
	}
}

type verdifaxAttestRequest struct {
	Digest     string `json:"digest"`
	Algorithm  string `json:"algorithm"`
	SubjectRef string `json:"subject_ref,omitempty"`
	LeafCount  int    `json:"leaf_count,omitempty"`

	// PreviousTreeSize asks the log to prove it only grew since the
	// previous checkpoint was anchored.
	//
	// Every other value in this exchange is about one entry: is it in
	// the log, at the position it claims. None of them notice a log
	// that quietly rebuilt its tree with different contents, because
	// each entry in the rebuilt tree verifies perfectly against the
	// new signed head, including any that were altered.
	//
	// Zero for the first checkpoint, which has no earlier tree to be
	// consistent with, and zero whenever the previous anchor did not
	// record a tree size. Zero means the question is not being asked.
	PreviousTreeSize int64 `json:"previous_tree_size,omitempty"`
}

type verdifaxAttestResponse struct {
	OK            bool   `json:"ok"`
	AttestationID int64  `json:"attestation_id"`
	Digest        string `json:"digest"`
	LedgerBackend string `json:"ledger_backend"`
	LogEntryID    string `json:"log_entry_id"`

	// LeafHash and LeafPreimage are what make the anchor checkable by
	// someone who trusts neither company. The log does NOT record the
	// digest submitted; it records sha256 of a canonical leaf that
	// contains it. Both were being returned and discarded until
	// 2026-09-04, which is why every checkpoint anchored before then
	// names a log entry nobody can tie back to it.
	LeafHash     string `json:"leaf_hash"`
	LeafPreimage string `json:"leaf_preimage"`

	// The three values that let a checkpoint be verified WITHOUT asking
	// Sigstore anything. Present only when Verdifax anchored to a real
	// transparency log and is new enough to return them; absent means
	// offline verification is not available for this checkpoint, which
	// is weaker than the preimage being absent, an anchor with a
	// preimage and no proof is still checkable by fetching the entry.
	//
	// InclusionProof is kept as raw JSON and never decoded here. Mesedi
	// has no use for its interior and every reason not to re-encode
	// somebody else's evidence: a field Sigstore or Verdifax adds later
	// travels through intact rather than being silently dropped by a
	// struct that predates it.
	InclusionProof json.RawMessage `json:"inclusion_proof"`
	EntryBody      string          `json:"entry_body"`
	LogID          string          `json:"log_id"`

	// ConsistencyProof answers a different question from the three
	// above and is absent far more often: only when a previous tree
	// size was sent, the backend is a real log, and the log answered.
	// Kept as raw JSON for the same reason as InclusionProof.
	ConsistencyProof json.RawMessage `json:"consistency_proof"`

	Provenance string `json:"provenance"`
	Error      string `json:"error"`
}

// anchorProofEnvelope is what gets stored in checkpoints.anchor_proof_json
// and handed to mesedi-verify.
//
// It exists because the three parts are useless separately. The proof
// walks a Merkle path to a root; entry_body is where the walk STARTS,
// since the RFC 6962 leaf is sha256(0x00 || base64decode(entry_body))
// and not the leaf hash; and log_id says whose key signed the checkpoint
// the walk ends at. A verifier missing any one of them either cannot
// run or runs against an assumption.
//
// Field names match Verdifax's response, so the envelope reads as the
// receipt it came from rather than as a Mesedi reinterpretation of it.
type anchorProofEnvelope struct {
	LogID          string          `json:"log_id,omitempty"`
	EntryBody      string          `json:"entry_body,omitempty"`
	InclusionProof json.RawMessage `json:"inclusion_proof,omitempty"`

	// ConsistencyProof, when present, shows the tree this checkpoint
	// landed in contains the tree the PREVIOUS checkpoint landed in
	// unchanged, as a prefix.
	//
	// Unlike the three fields above, this one is optional in a way that
	// is not a degradation of the same claim: it answers a different
	// question. The others prove an entry is in a tree. This proves the
	// tree is the same tree, and its absence is reported separately for
	// that reason.
	ConsistencyProof json.RawMessage `json:"consistency_proof,omitempty"`
}

func (a *VerdifaxAnchorer) client() *http.Client {
	if a.HTTPClient != nil {
		return a.HTTPClient
	}
	return &http.Client{Timeout: verdifaxAnchorTimeout}
}

// AnchorCheckpoint submits cp.Hash and returns where it landed.
//
// previousTreeSize is the size of the log's tree when the PREVIOUS
// checkpoint was anchored, and asks the log to prove it only grew since
// then. Zero for the first checkpoint, and zero whenever the previous
// anchor recorded no tree size, which is every checkpoint anchored
// before 4 September 2026.
func (a *VerdifaxAnchorer) AnchorCheckpoint(
	ctx context.Context, cp attest.Checkpoint, previousTreeSize int64,
) (store.CheckpointAnchor, error) {
	// The digest sent is the CHECKPOINT hash, the value the whole chain
	// is built on. Verdifax validates it as 64 lowercase hex, which
	// CheckpointHash always produces.
	body, err := json.Marshal(verdifaxAttestRequest{
		Digest:    cp.Hash,
		Algorithm: cp.Format,
		// Opaque to Verdifax by design. Enough for an operator to
		// correlate a log entry back to an interval, and nothing more:
		// no project ids, no counts that would leak activity volume to
		// whoever can read the log.
		SubjectRef: fmt.Sprintf("checkpoint/%d/%s",
			cp.Seq, cp.IntervalStart.UTC().Format(time.RFC3339)),
		LeafCount:        cp.TenantLeafCount,
		PreviousTreeSize: previousTreeSize,
	})
	if err != nil {
		return store.CheckpointAnchor{}, fmt.Errorf("verdifax anchor: marshal request: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, verdifaxAnchorTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		a.BaseURL+"/attest", bytes.NewReader(body))
	if err != nil {
		return store.CheckpointAnchor{}, fmt.Errorf("verdifax anchor: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", VerdifaxUserAgent)
	// X-Verdifax-Key, NOT Authorization: Bearer. Verified against
	// auth.HeaderName in verdifax-orchestrator rather than assumed; the
	// Bearer guess would have returned 401 on every single anchor.
	req.Header.Set(verdifaxKeyHeader, a.APIKey)

	resp, err := a.client().Do(req)
	if err != nil {
		// Deliberately does not wrap the URL with credentials or echo
		// any header. Transport errors are noisy enough already.
		return store.CheckpointAnchor{}, fmt.Errorf("verdifax anchor: request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, verdifaxMaxResponseBytes))
	if err != nil {
		return store.CheckpointAnchor{}, fmt.Errorf("verdifax anchor: read response: %w", err)
	}

	var out verdifaxAttestResponse
	if jsonErr := json.Unmarshal(raw, &out); jsonErr != nil {
		// Status first: a 502 from a proxy returns HTML, and reporting
		// "invalid JSON" for that sends whoever reads the log looking in
		// entirely the wrong place.
		return store.CheckpointAnchor{}, fmt.Errorf(
			"verdifax anchor: HTTP %d with an unparseable body (%d bytes)",
			resp.StatusCode, len(raw))
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		msg := out.Error
		if msg == "" {
			msg = "no error message in response"
		}
		return store.CheckpointAnchor{}, fmt.Errorf("verdifax anchor: HTTP %d: %s", resp.StatusCode, msg)
	}

	// CHECK 1. The receipt must be for the digest we sent.
	if out.Digest != cp.Hash {
		return store.CheckpointAnchor{}, fmt.Errorf(
			"verdifax anchor: receipt is for digest %s but checkpoint %d hashes to "+
				"%s; recording it would make the chain name a log entry that "+
				"attests to something else",
			out.Digest, cp.Seq, cp.Hash)
	}

	// CHECK 2. The receipt must not claim an independent submitter.
	if out.Provenance != expectedProvenance {
		return store.CheckpointAnchor{}, fmt.Errorf(
			"verdifax anchor: receipt claims provenance %q, want %q. Mesedi and "+
				"Verdifax are the same legal entity, so this key is misclassified "+
				"and its receipts assert an independent party that does not exist. "+
				"Fix with POST /admin/keys/{id}/submitter-class",
			out.Provenance, expectedProvenance)
	}

	if out.LogEntryID == "" {
		return store.CheckpointAnchor{}, fmt.Errorf(
			"verdifax anchor: checkpoint %d accepted but no log entry id returned; "+
				"an anchor with nothing to point at cannot be verified", cp.Seq)
	}

	// CHECK 3. The receipt must be checkable by someone who trusts
	// neither company.
	//
	// Checks 1 and 2 both ask Verdifax to vouch for itself, which is
	// worth exactly as much as it sounds: they catch a misconfigured
	// endpoint, not a dishonest one. This check is different. The log
	// does not record cp.Hash, it records sha256 of a canonical leaf
	// containing it, so without the preimage there is no path at all
	// from a checkpoint to its log entry, and an auditor comparing the
	// two finds a mismatch every time and is right to.
	//
	// That was the state of this system until 2026-09-04. Every anchor
	// written before that names a real Sigstore entry that nobody, this
	// company included, can tie back to the checkpoint that produced it.
	// The nonce inside those preimages was generated in Verdifax's
	// handler and discarded, so they cannot be repaired.
	//
	// Fails closed. The scheduler treats an error here as "not anchored"
	// and retries next tick, which stalls the chain visibly. That is the
	// correct trade: a stalled chain is an outage someone notices today,
	// an unverifiable anchor is a discovery someone makes years from now
	// when the evidence is being relied on.
	if err := checkAnchorReceiptIsVerifiable(cp, out); err != nil {
		return store.CheckpointAnchor{}, err
	}

	// The proof is NOT required, and its absence must not fail the
	// anchor. A missing proof costs offline verification; refusing would
	// cost the chain, because the scheduler treats an error here as "not
	// anchored" and retries forever. Task #32 was one event-less
	// execution stopping checkpointing for every tenant, the same shape
	// of mistake, and not one to repeat for a degradation that leaves
	// the anchor perfectly checkable online.
	//
	// So: recorded when present, logged when absent, never fatal.
	proofJSON := buildAnchorProof(out)
	if proofJSON == "" && out.LedgerBackend == "rekor" {
		a.Logger.Warn("verdifax_anchorer: anchored to a real log but got no offline proof",
			"seq", cp.Seq,
			"log_entry_id", out.LogEntryID,
			"has_inclusion_proof", len(out.InclusionProof) > 0,
			"has_entry_body", out.EntryBody != "",
			"detail", "this checkpoint can be verified only by contacting the log")
	}

	a.Logger.Info("verdifax_anchorer: checkpoint anchored",
		"seq", cp.Seq,
		"attestation_id", out.AttestationID,
		"log_entry_id", out.LogEntryID,
		"ledger_backend", out.LedgerBackend,
		"offline_verifiable", proofJSON != "",
	)
	return store.CheckpointAnchor{
		Anchored:        true,
		LogEntryID:      out.LogEntryID,
		LedgerBackend:   out.LedgerBackend,
		LeafPreimage:    out.LeafPreimage,
		AnchorProofJSON: proofJSON,
	}, nil
}

// buildAnchorProof assembles the offline-verification envelope, or
// returns "" when the receipt does not carry enough to build one.
//
// A partial envelope is worse than none. A proof with no entry body
// makes the Merkle walk start from the wrong value and fail, and that
// failure looks exactly like tampering to whoever reads the report, a
// verifier should say "cannot be checked", not "does not match". So all
// three parts or nothing.
// The consistency proof is carried when present and simply omitted when
// not. It is deliberately NOT part of the all-three-or-nothing rule
// above: those three are one claim that breaks if any is missing, while
// this is a separate claim about the log rather than about the entry.
// Withholding an inclusion proof because no consistency proof came back
// would trade a check we can make for one we cannot.
func buildAnchorProof(out verdifaxAttestResponse) string {
	if len(out.InclusionProof) == 0 || out.EntryBody == "" || out.LogID == "" {
		return ""
	}
	b, err := json.Marshal(anchorProofEnvelope{
		LogID:            out.LogID,
		EntryBody:        out.EntryBody,
		InclusionProof:   out.InclusionProof,
		ConsistencyProof: out.ConsistencyProof,
	})
	if err != nil {
		// Unreachable for two strings and a json.RawMessage that was
		// already parsed. Returning "" rather than propagating keeps the
		// contract above: no proof is a degradation, never a failed
		// anchor.
		return ""
	}
	return string(b)
}

// checkAnchorReceiptIsVerifiable confirms the receipt carries what a
// third party needs to check it against the log.
//
// Split out so the three distinct failures can be exercised
// individually: a receipt with no preimage, a preimage that does not
// hash to the leaf Verdifax reported, and a preimage that does not
// contain this checkpoint's hash. They mean different things and lead
// whoever reads the log to different places.
func checkAnchorReceiptIsVerifiable(
	cp attest.Checkpoint, out verdifaxAttestResponse,
) error {
	if out.LeafPreimage == "" {
		return fmt.Errorf(
			"verdifax anchor: checkpoint %d got a receipt with no leaf preimage. "+
				"The log records a hash of a string that was not returned, so this "+
				"anchor could never be checked by an auditor. Refusing to record it",
			cp.Seq)
	}

	// The preimage must actually produce the leaf Verdifax says it
	// anchored. If it does not, one of the two values is wrong and
	// recording either would be recording a fiction.
	if out.LeafHash != "" {
		sum := sha256.Sum256([]byte(out.LeafPreimage))
		if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, out.LeafHash) {
			return fmt.Errorf(
				"verdifax anchor: checkpoint %d receipt is internally inconsistent: "+
					"the preimage returned hashes to %s but the receipt reports leaf "+
					"hash %s", cp.Seq, got, out.LeafHash)
		}
	}

	// And the leaf must commit to THIS checkpoint. Searched rather than
	// parsed: the preimage's fields are joined without length prefixing,
	// so it cannot be split back into parts unambiguously.
	if !strings.Contains(out.LeafPreimage, cp.Hash) {
		return fmt.Errorf(
			"verdifax anchor: checkpoint %d hashes to %s, which does not appear in "+
				"the leaf that was anchored. The log entry would describe something "+
				"other than this checkpoint", cp.Seq, cp.Hash)
	}
	return nil
}
