package api

// DirectRekorAnchorer puts each checkpoint on Sigstore's public Rekor v1
// log itself, signed with Mesedi's own key.
//
// Until 2026-11-16 a third-party service signed and submitted the entry;
// that service has been switched off, and Mesedi now does the same job
// with nothing in between (MESEDI-DIRECT-ANCHORING-DESIGN-2026-10-21,
// sections 3.1 to 3.4). Request and response shapes follow Sigstore's
// published API definition, rekor openapi.yaml: POST /api/v1/log/entries
// to submit a hashedrekord v0.0.1 entry, GET /api/v1/log/entries/{uuid}
// to read one back, GET /api/v1/log/proof for a consistency proof.
//
// WHAT IS SIGNED. The leaf preimage mesedi.anchor.v1.<seq>.<hash> is
// hashed with SHA-256; the log records that hash, and the entry carries
// Mesedi's signature over it and Mesedi's public key. The checkpoint hash
// sits inside the preimage, so the verifier's existing binding check
// applies unchanged, and the verifier can now also check WHO signed.
//
// NO DUPLICATES, BY CONSTRUCTION. The signature is deterministic (RFC
// 6979: crypto/ecdsa signs deterministically when given no random
// source), so one checkpoint always produces a byte-identical entry. A
// retry after a lost answer resubmits that identical entry, and the log
// answers 409 Conflict with the location of the entry it already holds,
// which is then adopted. The design's first plan, searching the log's
// index by hash, was dropped: Sigstore marks that endpoint deprecated
// and its results "may be incomplete".

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest"
	"mesedi/backend/internal/store"
)

// DefaultRekorURL is Sigstore's public Rekor v1 log.
const DefaultRekorURL = "https://rekor.sigstore.dev"

// AnchorPreimagePrefix starts every leaf preimage Mesedi signs. The
// verifier refuses a direct anchor whose preimage does not start with it.
const AnchorPreimagePrefix = "mesedi.anchor.v1."

const (
	directAnchorTimeout     = 30 * time.Second
	directMaxResponseBytes  = 1 << 20
	directAnchorUserAgent   = "mesedi-anchorer/1"
	hashedRekordAPIVersion  = "0.0.1"
	hashedRekordKind        = "hashedrekord"
	rekorEntriesPath        = "/api/v1/log/entries"
	rekorConsistencyPath    = "/api/v1/log/proof"
	rekorConflictStatusCode = http.StatusConflict
)

// DirectRekorAnchorer implements CheckpointAnchor against Rekor v1.
type DirectRekorAnchorer struct {
	RekorURL   string
	Key        *ecdsa.PrivateKey
	HTTPClient *http.Client
	Logger     *slog.Logger

	publicKeyPEM []byte
}

// NewDirectRekorAnchorer parses keyPEM (a PKCS#8 or SEC 1 ECDSA P-256
// private key) and returns an anchorer, or an error naming what is wrong
// with the key. An empty rekorURL means DefaultRekorURL.
func NewDirectRekorAnchorer(rekorURL string, keyPEM []byte, logger *slog.Logger) (*DirectRekorAnchorer, error) {
	key, err := ParseAnchorSigningKey(keyPEM)
	if err != nil {
		return nil, err
	}
	pub, err := AnchorPublicKeyPEM(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(rekorURL) == "" {
		rekorURL = DefaultRekorURL
	}
	if _, err := url.ParseRequestURI(rekorURL); err != nil {
		return nil, fmt.Errorf("direct anchor: log address %q is not a URL: %w", rekorURL, err)
	}
	return &DirectRekorAnchorer{
		RekorURL:     strings.TrimRight(rekorURL, "/"),
		Key:          key,
		Logger:       logger,
		publicKeyPEM: pub,
	}, nil
}

// ParseAnchorSigningKey reads an ECDSA P-256 private key from PEM.
// Errors never include the key material.
func ParseAnchorSigningKey(keyPEM []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(bytes.TrimSpace(keyPEM))
	if block == nil {
		return nil, errors.New("direct anchor: signing key is not PEM")
	}
	var key *ecdsa.PrivateKey
	switch block.Type {
	case "EC PRIVATE KEY":
		k, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("direct anchor: signing key is not a valid EC private key")
		}
		key = k
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("direct anchor: signing key is not a valid PKCS#8 private key")
		}
		k, ok := parsed.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("direct anchor: signing key is not an ECDSA key")
		}
		key = k
	default:
		return nil, fmt.Errorf("direct anchor: signing key has PEM type %q; want an EC private key", block.Type)
	}
	if key.Curve != elliptic.P256() {
		return nil, errors.New("direct anchor: signing key is not on curve P-256")
	}
	return key, nil
}

// AnchorPublicKeyPEM is the PKIX PEM the entry carries and the verifier
// pins.
func AnchorPublicKeyPEM(pub *ecdsa.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("direct anchor: encode public key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

// AnchorKeyFingerprint is the SHA-256 of the public key's DER encoding,
// in lowercase hex: the value published and pinned for a signing key.
func AnchorKeyFingerprint(pub *ecdsa.PublicKey) string {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// AnchorPreimage is the exact string whose SHA-256 the log records.
func AnchorPreimage(cp attest.Checkpoint) string {
	return fmt.Sprintf("%s%d.%s", AnchorPreimagePrefix, cp.Seq, cp.Hash)
}

func (a *DirectRekorAnchorer) client() *http.Client {
	if a.HTTPClient != nil {
		return a.HTTPClient
	}
	return &http.Client{Timeout: directAnchorTimeout}
}

// storedAnchorProof is what gets stored in checkpoints.anchor_proof_json
// and handed to mesedi-verify.
//
// It exists because the three parts are useless separately. The proof
// walks a Merkle path to a root; entry_body is where the walk STARTS,
// since the RFC 6962 leaf is sha256(0x00 || base64decode(entry_body))
// and not the leaf hash; and log_id says whose key signed the checkpoint
// the walk ends at. A verifier missing any one of them either cannot
// run or runs against an assumption.
//
// Field names are unchanged since the anchors made before 2026-11-16, so
// every stored proof, old or new, has one shape for the verifier.
type storedAnchorProof struct {
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

// rekorLogEntry is one value of Rekor's LogEntry object. Untagged on
// purpose: Rekor's keys are camelCase (logID, logIndex), and encoding/json
// matches untagged fields case-insensitively, so this reads them without
// redeclaring someone else's naming as ours.
type rekorLogEntry struct {
	LogID          string
	LogIndex       int64
	Body           string
	IntegratedTime int64
	Verification   struct {
		InclusionProof json.RawMessage
	}
}

// rekorInclusionProof is Rekor's InclusionProof, read the same way.
type rekorInclusionProof struct {
	LogIndex   int64
	RootHash   string
	TreeSize   int64
	Hashes     []string
	Checkpoint string
}

// consistencyProofStored is what the verifier reads: the hashes Rekor
// returns plus the two tree sizes Rekor's response leaves out. Untagged,
// so its keys are FirstSize, LastSize and Hashes, as the verifier expects.
type consistencyProofStored struct {
	FirstSize int64
	LastSize  int64
	Hashes    []string
}

// AnchorCheckpoint signs and submits cp, or adopts the entry already
// submitted for it, and returns everything needed to verify it offline.
func (a *DirectRekorAnchorer) AnchorCheckpoint(
	ctx context.Context, cp attest.Checkpoint, previous TreeHead,
) (store.CheckpointAnchor, error) {
	preimage := AnchorPreimage(cp)
	digest := sha256.Sum256([]byte(preimage))
	sig, err := a.Key.Sign(nil, digest[:], crypto.SHA256)
	if err != nil {
		return store.CheckpointAnchor{}, fmt.Errorf("direct anchor: sign checkpoint %d: %w", cp.Seq, err)
	}
	proposed := hashedRekordJSON(digest[:], sig, a.publicKeyPEM)

	entry, err := a.submit(ctx, proposed)
	if err != nil {
		return store.CheckpointAnchor{}, fmt.Errorf("direct anchor: checkpoint %d: %w", cp.Seq, err)
	}
	if err := a.checkEntry(entry, digest[:]); err != nil {
		return store.CheckpointAnchor{}, fmt.Errorf("direct anchor: checkpoint %d: %w", cp.Seq, err)
	}

	var incl rekorInclusionProof
	if err := json.Unmarshal(entry.Verification.InclusionProof, &incl); err != nil {
		return store.CheckpointAnchor{}, fmt.Errorf("direct anchor: checkpoint %d: unreadable inclusion proof: %w", cp.Seq, err)
	}
	consistency := a.consistencyProof(ctx, cp.Seq, previous, incl)

	proof, err := json.Marshal(storedAnchorProof{
		LogID:            entry.LogID,
		EntryBody:        entry.Body,
		InclusionProof:   entry.Verification.InclusionProof,
		ConsistencyProof: consistency,
	})
	if err != nil {
		return store.CheckpointAnchor{}, fmt.Errorf("direct anchor: checkpoint %d: encode proof: %w", cp.Seq, err)
	}
	a.Logger.Info("direct_anchorer: checkpoint anchored",
		"seq", cp.Seq, "log_index", entry.LogIndex, "log_id", entry.LogID,
		"consistency_proof", len(consistency) > 0)
	return store.CheckpointAnchor{
		Anchored:        true,
		LogEntryID:      strconv.FormatInt(entry.LogIndex, 10),
		LedgerBackend:   "rekor",
		LeafPreimage:    preimage,
		AnchorProofJSON: string(proof),
		AnchorKind:      store.AnchorKindMesediDirect,
	}, nil
}

// hashedRekordJSON builds the proposed entry. Built as text rather than
// from a tagged struct: Rekor's keys are camelCase, and every value here
// is hex or base64, which needs no escaping beyond strconv.Quote.
func hashedRekordJSON(digest, sig, publicKeyPEM []byte) []byte {
	return []byte(`{"apiVersion":` + strconv.Quote(hashedRekordAPIVersion) +
		`,"kind":` + strconv.Quote(hashedRekordKind) +
		`,"spec":{"signature":{"content":` + strconv.Quote(base64.StdEncoding.EncodeToString(sig)) +
		`,"publicKey":{"content":` + strconv.Quote(base64.StdEncoding.EncodeToString(publicKeyPEM)) + `}}` +
		`,"data":{"hash":{"algorithm":"sha256","value":` + strconv.Quote(hex.EncodeToString(digest)) + `}}}}`)
}

// submit posts the entry. On 409 Conflict the identical entry is already
// in the log, and the one at the returned location is read instead.
func (a *DirectRekorAnchorer) submit(ctx context.Context, proposed []byte) (rekorLogEntry, error) {
	status, header, body, err := a.do(ctx, http.MethodPost, a.RekorURL+rekorEntriesPath, proposed)
	if err != nil {
		return rekorLogEntry{}, err
	}
	switch {
	case status == http.StatusCreated:
		return singleEntry(body)
	case status == rekorConflictStatusCode:
		loc := header.Get("Location")
		if loc == "" {
			return rekorLogEntry{}, errors.New("the log says the entry already exists but gives no location")
		}
		target, err := a.resolveLocation(loc)
		if err != nil {
			return rekorLogEntry{}, err
		}
		a.Logger.Info("direct_anchorer: entry already in the log, adopting it", "location", target)
		s2, _, b2, err := a.do(ctx, http.MethodGet, target, nil)
		if err != nil {
			return rekorLogEntry{}, err
		}
		if s2 != http.StatusOK {
			return rekorLogEntry{}, fmt.Errorf("reading the existing entry: HTTP %d", s2)
		}
		return singleEntry(b2)
	default:
		return rekorLogEntry{}, fmt.Errorf("the log answered HTTP %d: %s", status, rekorErrorMessage(body))
	}
}

// resolveLocation accepts only a location on the configured log, so a
// redirect cannot send the anchorer to read evidence from elsewhere.
func (a *DirectRekorAnchorer) resolveLocation(loc string) (string, error) {
	base, err := url.Parse(a.RekorURL)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(loc)
	if err != nil {
		return "", fmt.Errorf("the log's location %q is not a URL", loc)
	}
	u := base.ResolveReference(ref)
	if u.Scheme != base.Scheme || u.Host != base.Host || !strings.HasPrefix(u.Path, rekorEntriesPath+"/") {
		return "", fmt.Errorf("the log pointed to %q, which is not an entry on %s", u.String(), a.RekorURL)
	}
	return u.String(), nil
}

func (a *DirectRekorAnchorer) do(ctx context.Context, method, target string, body []byte) (int, http.Header, []byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, directAnchorTimeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, target, rd)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", directAnchorUserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client().Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("could not reach the log: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, directMaxResponseBytes))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("read the log's answer: %w", err)
	}
	return resp.StatusCode, resp.Header, raw, nil
}

// singleEntry reads Rekor's LogEntry object, which maps one entry UUID to
// the entry, and refuses anything other than exactly one.
func singleEntry(raw []byte) (rekorLogEntry, error) {
	var byUUID map[string]rekorLogEntry
	if err := json.Unmarshal(raw, &byUUID); err != nil {
		return rekorLogEntry{}, fmt.Errorf("the log's answer is not a log entry: %w", err)
	}
	if len(byUUID) != 1 {
		return rekorLogEntry{}, fmt.Errorf("the log returned %d entries; want exactly one", len(byUUID))
	}
	for _, e := range byUUID {
		return e, nil
	}
	return rekorLogEntry{}, errors.New("unreachable")
}

func rekorErrorMessage(raw []byte) string {
	var e struct {
		Message string
	}
	if json.Unmarshal(raw, &e) == nil && e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("no error message (%d bytes)", len(raw))
}

// checkEntry refuses an entry that is not this checkpoint's, not signed
// by this key, or whose inclusion proof does not lead to its own root.
// The log is trusted for nothing it can be checked on.
func (a *DirectRekorAnchorer) checkEntry(e rekorLogEntry, digest []byte) error {
	if e.LogID == "" || e.Body == "" || len(e.Verification.InclusionProof) == 0 {
		return errors.New("the log's answer lacks a log id, an entry body or an inclusion proof")
	}
	bodyBytes, err := base64.StdEncoding.DecodeString(e.Body)
	if err != nil {
		return fmt.Errorf("the entry body is not base64: %w", err)
	}
	var body struct {
		Kind string
		Spec struct {
			Signature struct {
				Content   string
				PublicKey struct {
					Content string
				}
			}
			Data struct {
				Hash struct {
					Algorithm string
					Value     string
				}
			}
		}
	}
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		return fmt.Errorf("the entry body is not JSON: %w", err)
	}
	if body.Kind != hashedRekordKind {
		return fmt.Errorf("the entry is a %q, not a %s", body.Kind, hashedRekordKind)
	}
	if body.Spec.Data.Hash.Algorithm != "sha256" || !strings.EqualFold(body.Spec.Data.Hash.Value, hex.EncodeToString(digest)) {
		return errors.New("the entry records a different hash from this checkpoint's preimage")
	}
	pub, err := base64.StdEncoding.DecodeString(body.Spec.Signature.PublicKey.Content)
	if err != nil || !bytes.Equal(bytes.TrimSpace(pub), bytes.TrimSpace(a.publicKeyPEM)) {
		return errors.New("the entry is not signed with this anchorer's key")
	}
	sig, err := base64.StdEncoding.DecodeString(body.Spec.Signature.Content)
	if err != nil || !ecdsa.VerifyASN1(&a.Key.PublicKey, digest, sig) {
		return errors.New("the entry's signature does not verify against this anchorer's key")
	}
	var incl rekorInclusionProof
	if err := json.Unmarshal(e.Verification.InclusionProof, &incl); err != nil {
		return fmt.Errorf("unreadable inclusion proof: %w", err)
	}
	leaf := sha256.Sum256(append([]byte{0x00}, bodyBytes...))
	ok, err := attest.VerifyInclusion(attest.InclusionProof{
		LeafIndex: int(incl.LogIndex),
		LeafHash:  hex.EncodeToString(leaf[:]),
		TreeSize:  int(incl.TreeSize),
		Root:      strings.ToLower(incl.RootHash),
		Path:      lowerAll(incl.Hashes),
	})
	if err != nil || !ok {
		return fmt.Errorf("the inclusion proof does not lead from this entry to the root the log reports (%v)", err)
	}
	if incl.Checkpoint == "" {
		return errors.New("the inclusion proof carries no signed checkpoint")
	}
	return nil
}

func lowerAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	return out
}

// consistencyProof asks the log to prove its tree at this anchor contains
// its tree at the previous anchor, checks that proof against the two
// roots the log signed (RFC 9162 section 2.1.4.2), and returns it with
// both sizes for the verifier. The log's own rootHash field is NOT used:
// on the real log it is the root of the tree as it is now, not at the
// size asked about (found by the real-log run of 2026-11-16). Optional: a
// missing or failing proof is logged, never fatal, because refusing would
// stall the chain over a check the verifier reports as unproven anyway.
func (a *DirectRekorAnchorer) consistencyProof(ctx context.Context, seq uint64, prev TreeHead, incl rekorInclusionProof) json.RawMessage {
	if prev.Size <= 0 || prev.Root == "" || incl.TreeSize <= prev.Size {
		return nil
	}
	target := fmt.Sprintf("%s%s?firstSize=%d&lastSize=%d", a.RekorURL, rekorConsistencyPath, prev.Size, incl.TreeSize)
	status, _, body, err := a.do(ctx, http.MethodGet, target, nil)
	if err != nil || status != http.StatusOK {
		a.Logger.Warn("direct_anchorer: no consistency proof", "seq", seq, "status", status, "error", fmt.Sprint(err))
		return nil
	}
	var got struct {
		Hashes []string
	}
	if err := json.Unmarshal(body, &got); err != nil {
		a.Logger.Warn("direct_anchorer: unreadable consistency proof", "seq", seq)
		return nil
	}
	if err := verifyConsistencyRFC9162(prev.Size, prev.Root, incl.TreeSize, incl.RootHash, got.Hashes); err != nil {
		a.Logger.Warn("direct_anchorer: consistency proof does not hold", "seq", seq, "error", err.Error())
		return nil
	}
	out, err := json.Marshal(consistencyProofStored{FirstSize: prev.Size, LastSize: incl.TreeSize, Hashes: got.Hashes})
	if err != nil {
		return nil
	}
	return out
}
