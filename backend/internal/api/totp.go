// Customer-facing TOTP / two-factor authentication handlers.
//
// The HTTP surface this file exposes:
//
//   - GET  /me/2fa/status              — is 2FA enabled for the calling user?
//   - POST /me/2fa/setup-init          — generate a fresh secret + QR
//   - POST /me/2fa/setup-verify        — confirm the customer scanned + enrolled
//   - POST /me/2fa/disable             — wipe TOTP + backup codes after a code check
//   - POST /me/2fa/regenerate-codes    — replace the backup-code set
//   - POST /auth/2fa-verify            — server-to-server, completes a paused signin
//
// All /me/2fa/* endpoints sit behind the normal cookie-auth middleware
// (session present + project context set). The /auth/2fa-verify
// endpoint is shared-secret server-to-server, like /signin and
// /auth/logout — the dashboard's Worker calls it after the customer
// enters their 6-digit code on the post-signin prompt page.
//
// Secret storage: the raw TOTP secret is only ever seen by the
// backend in `setup-verify` (when the customer sends it back so we
// can verify their code matches) and in the verify path (when we
// decrypt for validation). At rest it is AES-256-GCM ciphertext in
// `user_totp.secret_encrypted`. See totp_crypto.go for the helper.
//
// Backup codes: shown once at enrollment, regenerable via the
// dedicated endpoint which voids the old set. Storage is SHA-256
// hashed (the raw code never lives in the DB) so a database leak
// does not yield usable codes. Industry-standard convention.

package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"net/http"
	"strings"
	"time"

	"github.com/pquerna/otp/totp"

	"mesedi/backend/internal/store"
)

// totpQRPixelSize is the QR image edge length returned to the
// dashboard. 256px gives a visually crisp render at the standard
// 192px CSS slot without forcing the customer to enlarge to scan.
const totpQRPixelSize = 256

const (
	// totpIssuer is the label that shows up in the customer's
	// authenticator app next to the code. Customers see "Mesedi
	// (their-email@example.com)" in their app once enrolled.
	totpIssuer = "Mesedi"

	// backupCodeCount is how many backup codes we mint at enrollment
	// and on regenerate. 10 matches GitHub / Google / 1Password
	// convention; enough that losing-phone has real recovery slack
	// without being unwieldy.
	backupCodeCount = 10

	// backupCodeRawBytes is the entropy of each backup code. 8 bytes
	// = 16 hex chars per code, 64 bits of entropy. Fine for a
	// rate-limited verify endpoint where brute force needs millions
	// of attempts per second.
	backupCodeRawBytes = 8

	// pendingTokenRawBytes is the entropy of a pending 2FA token. 32
	// bytes = 256 bits, far above what a brute force could exhaust in
	// the 5-minute TTL.
	pendingTokenRawBytes = 32

	// pendingTokenTTL is how long a pending 2FA token lives between
	// the /signin response and the matching /auth/2fa-verify. Long
	// enough for the customer to fumble with their phone, short
	// enough that a stolen token cannot be replayed days later.
	pendingTokenTTL = 5 * time.Minute
)

// hashSHA256Hex returns the lowercase hex SHA-256 of s. Used to mint
// the storage-layer hashes for backup codes and pending tokens (the
// raw token only lives in the customer's browser).
func hashSHA256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// randomHexString returns `n` random bytes hex-encoded. Used to mint
// backup codes and pending 2FA tokens.
func randomHexString(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// ────────────────────────────────────────────────────────────────────
// GET /me/2fa/status
// ────────────────────────────────────────────────────────────────────

// TOTPStatusResponse is the wire shape for GET /me/2fa/status.
type TOTPStatusResponse struct {
	Enabled              bool `json:"enabled"`
	BackupCodesRemaining int  `json:"backup_codes_remaining"`
}

// HandleTOTPStatus returns whether 2FA is enabled for the current
// user and how many backup codes remain. The dashboard's settings
// page reads this to decide whether to render "Enable 2FA" or
// "Disable 2FA".
func (h *Handlers) HandleTOTPStatus(w http.ResponseWriter, r *http.Request) {
	userID, ok := UserIDFromContext(r.Context())
	if !ok || userID == "" {
		writeError(w, http.StatusUnauthorized, "no user context")
		return
	}
	_, err := h.Store.GetUserTOTP(r.Context(), userID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "get totp: "+err.Error())
		return
	}
	enabled := err == nil
	remaining := 0
	if enabled {
		remaining, err = h.Store.CountUnusedBackupCodes(r.Context(), userID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "count backup codes: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, TOTPStatusResponse{
		Enabled:              enabled,
		BackupCodesRemaining: remaining,
	})
}

// ────────────────────────────────────────────────────────────────────
// POST /me/2fa/setup-init
// ────────────────────────────────────────────────────────────────────

// TOTPSetupInitResponse is the wire shape for the first enrollment
// step. The dashboard renders QRPNGBase64 as an <img> for the
// customer to scan; SecretBase32 is the same secret in human-
// transcribable form for app variants that take the secret directly.
// OtpAuthURL is included as a fallback / debugging aid (e.g. deep-
// link from a help doc) but the dashboard does not render it.
//
// Generating the QR server-side rather than in the dashboard keeps the
// frontend free of a QR-library dependency (one less supply-chain
// surface), and the otpauth URL embedded in the image is the same
// public-by-design value the dashboard already knows — we are not
// leaking anything new.
type TOTPSetupInitResponse struct {
	SecretBase32 string `json:"secret_base32"`
	OtpAuthURL   string `json:"otpauth_url"`
	QRPNGBase64  string `json:"qr_png_base64"`
	Issuer       string `json:"issuer"`
}

// HandleTOTPSetupInit generates a fresh TOTP secret for the calling
// user and returns it. The secret is NOT persisted at this step — it
// only becomes durable when the customer proves they scanned it by
// sending back a matching code via setup-verify. Bouncing here means
// the customer can re-init as many times as they want without
// leaving half-enrolled rows in the DB.
func (h *Handlers) HandleTOTPSetupInit(w http.ResponseWriter, r *http.Request) {
	userID, ok := UserIDFromContext(r.Context())
	if !ok || userID == "" {
		writeError(w, http.StatusUnauthorized, "no user context")
		return
	}
	if len(h.TOTPEncryptionKey) == 0 {
		writeError(w, http.StatusServiceUnavailable,
			"2fa not configured (MESEDI_TOTP_ENCRYPTION_KEY unset)")
		return
	}
	// If the customer already has 2FA enabled, refuse to mint a new
	// secret. They must disable first. This prevents an accidental
	// double-enroll where the customer thinks they're rotating and
	// instead bricks their existing app pairing.
	if _, err := h.Store.GetUserTOTP(r.Context(), userID); err == nil {
		writeError(w, http.StatusConflict,
			"2fa already enabled; disable it first if you want to re-enroll")
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "get existing totp: "+err.Error())
		return
	}
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      totpIssuer,
		AccountName: userID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "generate totp secret: "+err.Error())
		return
	}
	// Render the otpauth URL as a PNG QR code and base64-encode for
	// data: URL embedding in the dashboard. If rendering fails we
	// surface a 500 rather than ship a half-broken init — the
	// dashboard relies on the QR for the primary scan UX.
	img, err := key.Image(totpQRPixelSize, totpQRPixelSize)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "render qr image: "+err.Error())
		return
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		writeError(w, http.StatusInternalServerError, "encode qr png: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, TOTPSetupInitResponse{
		SecretBase32: key.Secret(),
		OtpAuthURL:   key.URL(),
		QRPNGBase64:  base64.StdEncoding.EncodeToString(buf.Bytes()),
		Issuer:       totpIssuer,
	})
}

// ────────────────────────────────────────────────────────────────────
// POST /me/2fa/setup-verify
// ────────────────────────────────────────────────────────────────────

// TOTPSetupVerifyRequest carries the customer's proof of enrollment:
// they POST back the secret they just received plus a fresh code
// from their authenticator app.
type TOTPSetupVerifyRequest struct {
	SecretBase32 string `json:"secret_base32"`
	Code         string `json:"code"`
}

// TOTPSetupVerifyResponse hands the customer the freshly minted
// backup codes. SHOWN ONCE — the storage layer holds only hashes.
// Customer must save these somewhere outside Mesedi.
type TOTPSetupVerifyResponse struct {
	OK          bool     `json:"ok"`
	BackupCodes []string `json:"backup_codes"`
}

// HandleTOTPSetupVerify finalises enrollment. The customer sends
// back the secret they received in setup-init plus a 6-digit code
// from their authenticator app. We verify the code matches the
// secret, encrypt the secret and persist it, mint backup codes, and
// upgrade the current session to passed_2fa=1 so the customer is
// not immediately kicked out by their own enrollment.
func (h *Handlers) HandleTOTPSetupVerify(w http.ResponseWriter, r *http.Request) {
	userID, ok := UserIDFromContext(r.Context())
	if !ok || userID == "" {
		writeError(w, http.StatusUnauthorized, "no user context")
		return
	}
	if len(h.TOTPEncryptionKey) == 0 {
		writeError(w, http.StatusServiceUnavailable, "2fa not configured")
		return
	}
	var req TOTPSetupVerifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	req.SecretBase32 = strings.TrimSpace(req.SecretBase32)
	req.Code = strings.TrimSpace(req.Code)
	if req.SecretBase32 == "" || req.Code == "" {
		writeError(w, http.StatusBadRequest, "secret_base32 and code are required")
		return
	}
	// Defensive: refuse to re-enroll if a row already exists. Catches
	// a race between two browser tabs both finishing enrollment.
	if _, err := h.Store.GetUserTOTP(r.Context(), userID); err == nil {
		writeError(w, http.StatusConflict, "2fa already enabled")
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "get existing totp: "+err.Error())
		return
	}
	if !totp.Validate(req.Code, req.SecretBase32) {
		writeError(w, http.StatusBadRequest, "invalid code (did you scan the QR with the right account?)")
		return
	}
	encrypted, err := encryptTOTPSecret(h.TOTPEncryptionKey, []byte(req.SecretBase32))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encrypt secret: "+err.Error())
		return
	}
	now := time.Now().UTC()
	if err := h.Store.UpsertUserTOTP(r.Context(), &store.UserTOTP{
		UserID:          userID,
		SecretEncrypted: encrypted,
		CreatedAt:       now,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "store totp: "+err.Error())
		return
	}
	rawCodes, dbCodes, gerr := mintBackupCodeSet(userID, now)
	if gerr != nil {
		writeError(w, http.StatusInternalServerError, "mint backup codes: "+gerr.Error())
		return
	}
	if err := h.Store.CreateBackupCodes(r.Context(), dbCodes); err != nil {
		writeError(w, http.StatusInternalServerError, "store backup codes: "+err.Error())
		return
	}
	// Upgrade the current session so the customer is not kicked out.
	if tokenHash := sessionTokenHashFromRequest(r); tokenHash != "" {
		if uerr := h.Store.SetSessionPassed2FA(r.Context(), tokenHash); uerr != nil {
			h.Logger.Warn("setup-verify: SetSessionPassed2FA failed (customer may be logged out)",
				"error", uerr.Error(), "user_id", userID)
		}
	}
	h.Logger.Info("2fa enrolled", "user_id", userID)
	writeJSON(w, http.StatusOK, TOTPSetupVerifyResponse{
		OK:          true,
		BackupCodes: rawCodes,
	})
}

// ────────────────────────────────────────────────────────────────────
// POST /me/2fa/disable
// ────────────────────────────────────────────────────────────────────

// TOTPDisableRequest carries the customer's proof of identity: a
// fresh TOTP code OR an unused backup code.
type TOTPDisableRequest struct {
	Code string `json:"code"`
}

// HandleTOTPDisable wipes the TOTP secret + every backup code after
// verifying the customer can still produce a valid code. Requiring
// the code prevents a session-hijack attacker from silently turning
// off 2FA.
func (h *Handlers) HandleTOTPDisable(w http.ResponseWriter, r *http.Request) {
	userID, ok := UserIDFromContext(r.Context())
	if !ok || userID == "" {
		writeError(w, http.StatusUnauthorized, "no user context")
		return
	}
	if len(h.TOTPEncryptionKey) == 0 {
		writeError(w, http.StatusServiceUnavailable, "2fa not configured")
		return
	}
	var req TOTPDisableRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	req.Code = strings.TrimSpace(req.Code)
	if req.Code == "" {
		writeError(w, http.StatusBadRequest, "code is required")
		return
	}
	rec, err := h.Store.GetUserTOTP(r.Context(), userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "2fa not enabled")
			return
		}
		writeError(w, http.StatusInternalServerError, "get totp: "+err.Error())
		return
	}
	valid, verr := h.verifyTOTPOrBackupCode(r.Context(), userID, req.Code, rec.SecretEncrypted)
	if verr != nil {
		writeError(w, http.StatusInternalServerError, "verify: "+verr.Error())
		return
	}
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid code")
		return
	}
	if err := h.Store.DeleteUserTOTP(r.Context(), userID); err != nil {
		writeError(w, http.StatusInternalServerError, "delete totp: "+err.Error())
		return
	}
	if err := h.Store.DeleteBackupCodesForUser(r.Context(), userID); err != nil {
		writeError(w, http.StatusInternalServerError, "delete backup codes: "+err.Error())
		return
	}
	h.Logger.Info("2fa disabled", "user_id", userID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ────────────────────────────────────────────────────────────────────
// POST /me/2fa/regenerate-codes
// ────────────────────────────────────────────────────────────────────

// TOTPRegenerateRequest carries a current code so a session-hijack
// attacker cannot silently rotate backup codes (and burn the
// legitimate customer's offline recovery path).
type TOTPRegenerateRequest struct {
	Code string `json:"code"`
}

// TOTPRegenerateResponse hands the customer a fresh set of backup
// codes. SHOWN ONCE.
type TOTPRegenerateResponse struct {
	OK          bool     `json:"ok"`
	BackupCodes []string `json:"backup_codes"`
}

// HandleTOTPRegenerateBackupCodes voids the existing backup codes
// and mints a new set. Requires a fresh TOTP code (NOT a backup
// code, which would consume one slot for the regeneration itself).
func (h *Handlers) HandleTOTPRegenerateBackupCodes(w http.ResponseWriter, r *http.Request) {
	userID, ok := UserIDFromContext(r.Context())
	if !ok || userID == "" {
		writeError(w, http.StatusUnauthorized, "no user context")
		return
	}
	if len(h.TOTPEncryptionKey) == 0 {
		writeError(w, http.StatusServiceUnavailable, "2fa not configured")
		return
	}
	var req TOTPRegenerateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	req.Code = strings.TrimSpace(req.Code)
	if req.Code == "" {
		writeError(w, http.StatusBadRequest, "code is required")
		return
	}
	rec, err := h.Store.GetUserTOTP(r.Context(), userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "2fa not enabled")
			return
		}
		writeError(w, http.StatusInternalServerError, "get totp: "+err.Error())
		return
	}
	// TOTP only (no backup-code fallback) so the regeneration itself
	// cannot consume a backup code slot.
	plaintext, derr := decryptTOTPSecret(h.TOTPEncryptionKey, rec.SecretEncrypted)
	if derr != nil {
		writeError(w, http.StatusInternalServerError, "decrypt secret: "+derr.Error())
		return
	}
	if !totp.Validate(req.Code, string(plaintext)) {
		writeError(w, http.StatusBadRequest, "invalid code")
		return
	}
	if err := h.Store.DeleteBackupCodesForUser(r.Context(), userID); err != nil {
		writeError(w, http.StatusInternalServerError, "delete old backup codes: "+err.Error())
		return
	}
	now := time.Now().UTC()
	rawCodes, dbCodes, gerr := mintBackupCodeSet(userID, now)
	if gerr != nil {
		writeError(w, http.StatusInternalServerError, "mint backup codes: "+gerr.Error())
		return
	}
	if err := h.Store.CreateBackupCodes(r.Context(), dbCodes); err != nil {
		writeError(w, http.StatusInternalServerError, "store backup codes: "+err.Error())
		return
	}
	h.Logger.Info("2fa backup codes regenerated", "user_id", userID)
	writeJSON(w, http.StatusOK, TOTPRegenerateResponse{
		OK:          true,
		BackupCodes: rawCodes,
	})
}

// ────────────────────────────────────────────────────────────────────
// POST /auth/2fa-verify
// ────────────────────────────────────────────────────────────────────

// TwoFactorVerifyRequest is the body the dashboard Worker POSTs
// after the customer enters their code on the prompt page. It
// carries the pending token minted by HandleSignin plus the
// customer-entered code.
type TwoFactorVerifyRequest struct {
	PendingToken string `json:"pending_token"`
	Code         string `json:"code"`
	UserAgent    string `json:"user_agent,omitempty"`
	IPAddress    string `json:"ip_address,omitempty"`
}

// HandleTwoFactorVerify completes a paused signin by minting the
// real session AFTER successfully verifying the customer's TOTP or
// backup code. Wire contract mirrors HandleSignin: shared secret in
// the X-Mesedi-Signin-Secret header, response shape mirrors
// SigninResponse so the dashboard Worker can reuse the same cookie-
// write code path it already runs.
func (h *Handlers) HandleTwoFactorVerify(w http.ResponseWriter, r *http.Request) {
	if h.SigninSecret == "" {
		writeError(w, http.StatusServiceUnavailable,
			"2fa-verify endpoint not configured (MESEDI_SIGNIN_SECRET unset)")
		return
	}
	presented := r.Header.Get(signinHeaderSecret)
	if subtle.ConstantTimeCompare([]byte(presented), []byte(h.SigninSecret)) != 1 {
		writeError(w, http.StatusUnauthorized, "2fa-verify requires server-to-server credential")
		return
	}
	if len(h.TOTPEncryptionKey) == 0 {
		writeError(w, http.StatusServiceUnavailable, "2fa not configured")
		return
	}
	var req TwoFactorVerifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	req.PendingToken = strings.TrimSpace(req.PendingToken)
	req.Code = strings.TrimSpace(req.Code)
	if req.PendingToken == "" || req.Code == "" {
		writeError(w, http.StatusBadRequest, "pending_token and code are required")
		return
	}
	tokenHash := hashSHA256Hex(req.PendingToken)
	pending, err := h.Store.GetPending2FAToken(r.Context(), tokenHash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "invalid or expired pending token")
			return
		}
		writeError(w, http.StatusInternalServerError, "get pending token: "+err.Error())
		return
	}
	now := time.Now().UTC()
	if !pending.UsedAt.IsZero() {
		writeError(w, http.StatusUnauthorized, "pending token already used")
		return
	}
	if now.After(pending.ExpiresAt) {
		writeError(w, http.StatusUnauthorized, "pending token expired (sign in again)")
		return
	}
	rec, err := h.Store.GetUserTOTP(r.Context(), pending.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get user totp: "+err.Error())
		return
	}
	valid, verr := h.verifyTOTPOrBackupCode(r.Context(), pending.UserID, req.Code, rec.SecretEncrypted)
	if verr != nil {
		writeError(w, http.StatusInternalServerError, "verify: "+verr.Error())
		return
	}
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid code")
		return
	}
	// Atomic mark-used; a race between two browser tabs racing on the
	// same token loses one side here without double-mint.
	if err := h.Store.MarkPending2FATokenUsed(r.Context(), tokenHash, now); err != nil {
		writeError(w, http.StatusUnauthorized, "pending token already used")
		return
	}
	// Find the customer's active project. The signin flow already
	// resolved the project for us when it minted the pending token
	// but we don't carry the project_id on the token itself —
	// instead re-resolve here so a token cannot be used to take
	// over a project the customer doesn't own.
	project, perr := h.Store.GetMostRecentProjectByOwnerEmail(r.Context(), pending.UserID)
	if perr != nil {
		writeError(w, http.StatusInternalServerError, "resolve project for user: "+perr.Error())
		return
	}
	// Mint the session token. Mirrors HandleSignin's -Batch-2
	// path but PassedTwoFactor is true from the start.
	rawSess, sessHash, mintErr := MintSessionToken()
	if mintErr != nil {
		writeError(w, http.StatusInternalServerError, "mint session token: "+mintErr.Error())
		return
	}
	sessionExpiresAt := now.Add(SessionTTL)
	sess := &store.Session{
		TokenHash:       sessHash,
		UserID:          pending.UserID,
		ProjectID:       project.ProjectID,
		CreatedAt:       now,
		ExpiresAt:       sessionExpiresAt,
		LastUsedAt:      now,
		UserAgent:       req.UserAgent,
		IPAddress:       req.IPAddress,
		PassedTwoFactor: true,
	}
	if perr := h.Store.CreateSession(r.Context(), sess); perr != nil {
		writeError(w, http.StatusInternalServerError, "persist session: "+perr.Error())
		return
	}
	// Touch user_totp.last_used_at so the admin "share recent use"
	// view shows the verified-recently signal.
	_ = h.Store.TouchUserTOTP(r.Context(), pending.UserID, now)
	h.Logger.Info("2fa verified, session minted",
		"user_id", pending.UserID,
		"project_id", project.ProjectID,
	)
	writeJSON(w, http.StatusOK, SigninResponse{
		OK:               true,
		ProjectID:        project.ProjectID,
		ProjectName:      project.Name,
		SessionToken:     rawSess,
		SessionExpiresAt: sessionExpiresAt.UTC().Format(time.RFC3339Nano),
	})
}

// ────────────────────────────────────────────────────────────────────
// Helpers
// ────────────────────────────────────────────────────────────────────

// verifyTOTPOrBackupCode returns (true, nil) iff `code` matches
// either the current TOTP value for `encryptedSecret` OR an unused
// backup code for `userID`. On a backup-code match, the row is
// atomically marked used (one-time use). Order: TOTP first because
// it is the common path; backup codes second as the fallback.
func (h *Handlers) verifyTOTPOrBackupCode(
	ctx context.Context,
	userID, code string,
	encryptedSecret []byte,
) (bool, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return false, nil
	}
	if len(encryptedSecret) > 0 && len(h.TOTPEncryptionKey) > 0 {
		plaintext, derr := decryptTOTPSecret(h.TOTPEncryptionKey, encryptedSecret)
		if derr == nil {
			if totp.Validate(code, string(plaintext)) {
				return true, nil
			}
		}
	}
	// Backup-code attempt. Hash on our side; the storage layer only
	// has hashes. ConsumeBackupCode atomically marks used + returns
	// ErrNotFound on no-match-or-already-used.
	codeHash := hashSHA256Hex(code)
	if err := h.Store.ConsumeBackupCode(ctx, userID, codeHash, time.Now().UTC()); err == nil {
		return true, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, fmt.Errorf("consume backup code: %w", err)
	}
	return false, nil
}

// mintBackupCodeSet generates a fresh slate of backup codes for the
// customer. Returns the raw codes (to surface to the customer once)
// and the corresponding storage rows (the hashes that go into the
// DB).
func mintBackupCodeSet(userID string, now time.Time) ([]string, []*store.BackupCode, error) {
	rawCodes := make([]string, backupCodeCount)
	dbCodes := make([]*store.BackupCode, backupCodeCount)
	for i := 0; i < backupCodeCount; i++ {
		raw, err := randomHexString(backupCodeRawBytes)
		if err != nil {
			return nil, nil, fmt.Errorf("random backup code: %w", err)
		}
		rawCodes[i] = raw
		dbCodes[i] = &store.BackupCode{
			CodeHash:  hashSHA256Hex(raw),
			UserID:    userID,
			CreatedAt: now,
		}
	}
	return rawCodes, dbCodes, nil
}

// mintPending2FAToken mints a fresh pending token for the signin
// 2FA fork. Returns the raw token (shown to the customer once) and
// the storage row.
func mintPending2FAToken(userID string, now time.Time) (string, *store.Pending2FAToken, error) {
	raw, err := randomHexString(pendingTokenRawBytes)
	if err != nil {
		return "", nil, fmt.Errorf("random pending token: %w", err)
	}
	return raw, &store.Pending2FAToken{
		TokenHash: hashSHA256Hex(raw),
		UserID:    userID,
		CreatedAt: now,
		ExpiresAt: now.Add(pendingTokenTTL),
	}, nil
}

// sessionTokenHashFromRequest reads the session cookie off the
// request and returns its SHA-256 hex. Empty string if no cookie or
// the cookie value is empty. Used by HandleTOTPSetupVerify to
// upgrade the current session's passed_2fa flag.
func sessionTokenHashFromRequest(r *http.Request) string {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		return ""
	}
	return hashSHA256Hex(cookie.Value)
}
