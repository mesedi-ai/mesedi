package api

// Report chaining and optional signing: ingest, run completion, and
// the signing-key endpoints. The fingerprint and the chain check live
// in the evidence module (attest/report_chain.go) so the checker reads
// runs with the same code.
//
// At ingest, before secret redaction rewrites any payload, each report
// that carries chain_prev is fingerprinted as received, and a signature
// on it is checked against the project's registered keys. A run with a
// signed report is a signed run: an unsigned report for it, or one with
// a bad signature, is refused. Reports from SDKs that predate chaining
// carry no chain_prev and are accepted as before.
//
// At completion the run's links must form one unbroken path from its
// genesis value. A break, or an unsigned report in a signed run, opens
// a record_integrity group, and the verdict (unchained, chained, signed
// or broken) is recorded for the run.
//
// Signing keys are registered only by a signed-in admin through the
// dashboard, never with an API key: an agent may hold the project's API
// key, and a key it could register would let it sign its own reports.
//
//	GET    /me/report-signing-keys
//	POST   /me/report-signing-keys        {"key_id": "...", "public_key": "<base64 Ed25519>"}
//	DELETE /me/report-signing-keys/{key_id}   (revokes; the key stays listed)

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/mesedi-ai/mesedi/backend/attest"
	"github.com/mesedi-ai/mesedi/backend/attest/events"
	"mesedi/backend/internal/store"
)

// Record-integrity signatures for chain findings.
const (
	RecordIntegritySigChainBroken      = "record_integrity:report_chain_broken"
	RecordIntegritySigSignatureMissing = "record_integrity:report_signature_missing"
)

var (
	hexFingerprint = regexp.MustCompile(`^[0-9a-f]{64}$`)
	signingKeyID   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// prepareReportChain checks the chain fields of an accepted batch. It
// returns the reports to keep, the chain rows to store for them, and
// how many were refused. Store errors refuse nothing: a lookup that
// fails keeps the report and leaves the completion check to judge it,
// because dropping a customer's data on a database hiccup is worse.
func (h *Handlers) prepareReportChain(
	ctx context.Context,
	projectID string,
	batch []events.Event,
) (kept []events.Event, rows []store.ReportChainRow, refused int) {
	signedRun := map[string]bool{}
	signedChecked := map[string]bool{}
	isSigned := func(execID string) bool {
		if !signedChecked[execID] {
			signedChecked[execID] = true
			if ok, err := h.Store.ExecutionHasSignedReports(ctx, execID); err == nil && ok {
				signedRun[execID] = true
			}
		}
		return signedRun[execID]
	}
	keys := map[string]ed25519.PublicKey{}
	keyFor := func(keyID string) ed25519.PublicKey {
		if k, ok := keys[keyID]; ok {
			return k
		}
		var pub ed25519.PublicKey
		if rec, err := h.Store.GetReportSigningKey(ctx, projectID, keyID); err == nil && rec.RevokedAt == "" {
			if raw, err := base64.StdEncoding.DecodeString(rec.PublicKey); err == nil && len(raw) == ed25519.PublicKeySize {
				pub = ed25519.PublicKey(raw)
			}
		}
		keys[keyID] = pub
		return pub
	}
	refuse := func(e events.Event, reason string) {
		refused++
		h.Logger.Warn("report refused",
			"project_id", projectID, "execution_id", e.ExecutionID,
			"event_id", e.EventID, "reason", reason)
	}

	kept = make([]events.Event, 0, len(batch))
	for _, e := range batch {
		if e.ChainPrev == "" {
			if e.Signature != nil {
				refuse(e, "signed report without chain_prev: a signature covers the chain fingerprint")
				continue
			}
			if isSigned(e.ExecutionID) {
				refuse(e, "unchained report for a signed run")
				continue
			}
			kept = append(kept, e)
			continue
		}
		if !hexFingerprint.MatchString(e.ChainPrev) {
			refuse(e, "chain_prev is not 64 lowercase hex characters")
			continue
		}
		sum := sha256.Sum256(e.Payload)
		payloadSHA := hex.EncodeToString(sum[:])
		fingerprint := attest.ReportChainHash(&e, payloadSHA)
		row := store.ReportChainRow{
			EventID: e.EventID, ExecutionID: e.ExecutionID, ProjectID: projectID,
			PrevHash: e.ChainPrev, Hash: fingerprint, PayloadSHA256: payloadSHA,
		}
		if e.Signature != nil {
			if err := verifyReportSignature(e.Signature, fingerprint, keyFor); err != nil {
				refuse(e, err.Error())
				continue
			}
			row.KeyID, row.Signature = e.Signature.KeyID, e.Signature.Sig
			signedRun[e.ExecutionID], signedChecked[e.ExecutionID] = true, true
		} else if isSigned(e.ExecutionID) {
			refuse(e, "unsigned report for a signed run")
			continue
		}
		kept = append(kept, e)
		rows = append(rows, row)
	}
	return kept, rows, refused
}

func verifyReportSignature(
	sig *events.ReportSignature,
	fingerprint string,
	keyFor func(string) ed25519.PublicKey,
) error {
	if !signingKeyID.MatchString(sig.KeyID) {
		return errors.New("signature key_id is malformed")
	}
	pub := keyFor(sig.KeyID)
	if pub == nil {
		return fmt.Errorf("signature key %q is not registered to this project, or is revoked", sig.KeyID)
	}
	raw, err := base64.StdEncoding.DecodeString(sig.Sig)
	if err != nil || len(raw) != ed25519.SignatureSize {
		return errors.New("signature is not a base64 Ed25519 signature")
	}
	msg, _ := hex.DecodeString(fingerprint)
	if !ed25519.Verify(pub, msg, raw) {
		return errors.New("signature does not verify")
	}
	return nil
}

// runReportChainCheck judges a completed run's chain, records the
// verdict, and opens a record_integrity group when it is broken.
// Only rows whose report Mesedi actually stored count: a row saved for
// a batch whose events were then refused is not part of the record.
func (h *Handlers) runReportChainCheck(
	r *http.Request,
	executionID, projectID string,
	evts []*events.Event,
	end *events.ReportChainEnd,
) {
	rows, err := h.Store.ListReportChainRows(r.Context(), executionID)
	if err != nil {
		h.Logger.Warn("list report chain failed", "execution_id", executionID, "error", err.Error())
		return
	}
	stored := map[string]bool{}
	for _, e := range evts {
		if e != nil && !events.IsBackendMinted(e.EventType) {
			stored[e.EventID] = true
		}
	}
	var links []attest.ReportChainLink
	chainedIDs := map[string]bool{}
	for _, row := range rows {
		if !stored[row.EventID] {
			continue
		}
		chainedIDs[row.EventID] = true
		links = append(links, attest.ReportChainLink{
			EventID: row.EventID, Prev: row.PrevHash, Hash: row.Hash, Signed: row.Signature != "",
		})
	}
	unchained := 0
	for id := range stored {
		if !chainedIDs[id] {
			unchained++
		}
	}
	row := store.ReportChainStatusRow{ExecutionID: executionID, ProjectID: projectID}
	endHash := ""
	if end != nil && hexFingerprint.MatchString(end.Hash) {
		endHash = end.Hash
		row.EndHash = end.Hash
		if end.Signature != nil {
			row.EndKeyID, row.EndSignature = end.Signature.KeyID, end.Signature.Sig
		}
	}
	v := attest.VerifyReportChain(executionID, links, unchained, endHash)

	// A signed run's end must be signed too, over its own message, or a
	// forger holding the project key could cut the run short and declare
	// an earlier report its end.
	endUnsigned := ""
	if v.Signed {
		switch {
		case endHash == "":
			endUnsigned = "this signed run declared no end, so its last reports could be missing unseen"
		case row.EndSignature == "":
			endUnsigned = "this signed run's declared end is unsigned"
		default:
			pub := ed25519.PublicKey(nil)
			if rec, err := h.Store.GetReportSigningKey(r.Context(), projectID, row.EndKeyID); err == nil && rec.RevokedAt == "" {
				if raw, err := base64.StdEncoding.DecodeString(rec.PublicKey); err == nil && len(raw) == ed25519.PublicKeySize {
					pub = raw
				}
			}
			sigRaw, err := base64.StdEncoding.DecodeString(row.EndSignature)
			if pub == nil || err != nil || !ed25519.Verify(pub, attest.ReportChainEndMessage(executionID, endHash), sigRaw) {
				endUnsigned = "the signature on this signed run's declared end does not verify"
			}
		}
	}

	status, detail, sig := store.ReportChainUnchained, "", ""
	switch {
	case !v.Chained:
	case len(v.Broken) > 0:
		status, detail, sig = store.ReportChainBroken, strings.Join(v.Broken, "; "), RecordIntegritySigChainBroken
	case v.Signed && (v.Unsigned > 0 || endUnsigned != ""):
		status = store.ReportChainBroken
		parts := []string{}
		if v.Unsigned > 0 {
			parts = append(parts, fmt.Sprintf("%d report(s) in this signed run are unsigned", v.Unsigned))
		}
		if endUnsigned != "" {
			parts = append(parts, endUnsigned)
		}
		detail = strings.Join(parts, "; ")
		sig = RecordIntegritySigSignatureMissing
	case v.Signed:
		status = store.ReportChainSigned
	default:
		status = store.ReportChainChained
	}
	row.Status, row.Detail = status, detail
	if err := h.Store.SetReportChainStatus(r.Context(), row); err != nil {
		h.Logger.Warn("set report chain status failed", "execution_id", executionID, "error", err.Error())
	}
	if sig == "" {
		return
	}
	isNew, gErr := h.Store.GroupRecordIntegrity(r.Context(), executionID, projectID, sig)
	if gErr != nil {
		h.Logger.Warn("report-chain grouping failed (continuing)",
			"execution_id", executionID, "signature", sig, "error", gErr.Error())
	}
	h.maybeFireWebhook(r, projectID, store.FailureClassRecordIntegrity, sig, isNew, gErr)
}

// requireSignedInAdmin admits a request only from an admin signed in
// through the dashboard. A request made with an API key is refused
// whatever its role, because the agent may hold one.
func (h *Handlers) requireSignedInAdmin(w http.ResponseWriter, r *http.Request) bool {
	if keyID, viaKey := APIKeyIDFromContext(r.Context()); viaKey && keyID != "" {
		writeError(w, http.StatusForbidden,
			"report signing keys are managed only by an admin signed in to the dashboard; "+
				"an API key cannot, because the agent may hold one")
		return false
	}
	if _, signedIn := UserIDFromContext(r.Context()); !signedIn {
		writeError(w, http.StatusForbidden, "sign in to the dashboard to manage report signing keys")
		return false
	}
	return h.requireRole(w, r, "admin")
}

func (h *Handlers) HandleListReportSigningKeys(w http.ResponseWriter, r *http.Request) {
	projectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	keys, err := h.Store.ListReportSigningKeys(r.Context(), projectID)
	if err != nil {
		h.Logger.Error("list report signing keys failed", "project_id", projectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not list report signing keys")
		return
	}
	// ?limit=&offset= like the other list endpoints; a project holds few
	// keys, so the default page is all of them.
	limit := parseIntQuery(r, "limit", 100, 1, 500)
	offset := parseIntQuery(r, "offset", 0, 0, 1_000_000)
	total := len(keys)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"keys": keys[offset:end], "total": total, "limit": limit, "offset": offset,
	})
}

func (h *Handlers) HandleCreateReportSigningKey(w http.ResponseWriter, r *http.Request) {
	projectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	if !h.requireSignedInAdmin(w, r) {
		return
	}
	var body struct {
		KeyID     string `json:"key_id"`
		PublicKey string `json:"public_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !signingKeyID.MatchString(body.KeyID) {
		writeError(w, http.StatusBadRequest, "key_id must be 1 to 64 letters, digits, dot, underscore or hyphen")
		return
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(body.PublicKey))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		writeError(w, http.StatusBadRequest, "public_key must be a base64 Ed25519 public key (32 bytes)")
		return
	}
	publicKey := base64.StdEncoding.EncodeToString(raw)
	createdBy, _ := UserIDFromContext(r.Context())
	err = h.Store.InsertReportSigningKey(r.Context(), projectID, body.KeyID, publicKey, createdBy)
	if errors.Is(err, store.ErrReportSigningKeyExists) {
		// Idempotent for a retry: the same key_id with the same key,
		// still in use, answers as the first request did. A different
		// key, or a revoked one, under that id is refused.
		if prior, gerr := h.Store.GetReportSigningKey(r.Context(), projectID, body.KeyID); gerr == nil &&
			prior.PublicKey == publicKey && prior.RevokedAt == "" {
			writeJSON(w, http.StatusOK, map[string]any{"key_id": body.KeyID, "public_key": publicKey})
			return
		}
		writeError(w, http.StatusConflict, "that key_id is already registered; a new key needs a new key_id")
		return
	}
	if err != nil {
		h.Logger.Error("insert report signing key failed", "project_id", projectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not save the key")
		return
	}
	h.recordAuditEvent(r, "report_signing_key_added", "report_signing_key", body.KeyID,
		map[string]any{"public_key": publicKey})
	writeJSON(w, http.StatusCreated, map[string]any{"key_id": body.KeyID, "public_key": publicKey})
}

func (h *Handlers) HandleRevokeReportSigningKey(w http.ResponseWriter, r *http.Request) {
	projectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	if !h.requireSignedInAdmin(w, r) {
		return
	}
	keyID := r.PathValue("key_id")
	err := h.Store.RevokeReportSigningKey(r.Context(), projectID, keyID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no unrevoked key with that key_id")
		return
	}
	if err != nil {
		h.Logger.Error("revoke report signing key failed", "project_id", projectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not revoke the key")
		return
	}
	h.recordAuditEvent(r, "report_signing_key_revoked", "report_signing_key", keyID, nil)
	w.WriteHeader(http.StatusNoContent)
}
