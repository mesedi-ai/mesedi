// Email-verification handlers (#232 pre-launch).
//
// Three endpoints back the "verify your email before the dashboard
// unlocks" flow for raw-email signups:
//
//   POST /api/email-verify/confirm
//     Body: { token }
//     Behavior: looks the token up, ensures it has not expired and
//     has not been used, marks the email verified (method='email_link'),
//     burns the token. Returns 200 with the email so the dashboard
//     can redirect the user back to /login or /app with a friendly
//     success state. Idempotent on the "expired" / "already used" /
//     "not found" cases: all three return 410 with a single human-
//     readable message ("this verification link is no longer valid")
//     because differentiating leaks signal we don't want to give an
//     attacker who hands the URL to themselves.
//
//   POST /api/email-verify/resend
//     Body: { email }
//     Behavior: rate-limited by email (1 per 60s). Looks up the
//     account by email — 404 means "no account" but we DO NOT echo
//     that (we always 202 to avoid an email-existence oracle).
//     Mints a new token, sends a fresh "verify your email" email.
//     Public endpoint; no auth required.
//
//   GET /api/me/email-verification-status
//     Auth: customer bearer token (api key in Authorization header).
//     Returns { verified: bool, email: <owner_email> } so the
//     dashboard can render its interstitial decision client-side.
//
// SSO + magic-link sign-ins don't use these endpoints — they call
// MarkEmailVerified directly from the relevant callback (HandleSignin)
// with the appropriate method label.

package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"mesedi/backend/internal/mail"
	"mesedi/backend/internal/store"
)

// EmailVerifyTokenLen is the size in raw bytes of the verification
// token; 32 bytes -> 43 url-safe base64 characters (no padding) ->
// fits in an email link without wrapping. 256 bits of entropy is
// more than enough for a one-shot 24-hour TTL.
const EmailVerifyTokenLen = 32

// EmailVerifyTokenTTL is how long an issued token stays valid. 24
// hours covers the customer-signed-up-before-bed -> clicked-next-
// morning case while keeping the replay window short.
const EmailVerifyTokenTTL = 24 * time.Hour

// resendEmailVerifyWindow + resendEmailVerifyMax bound how often a
// single email can ask for a fresh verification link. 1 per 60s is
// permissive enough for the legitimate "I didn't see the email,
// resend" case but tight enough that an attacker cannot use the
// resend endpoint to spam an inbox.
const (
	resendEmailVerifyWindow = 60 * time.Second
	resendEmailVerifyMax    = 1
)

var (
	resendEmailVerifyMu     sync.Mutex
	resendEmailVerifyCounts = map[string][]time.Time{}
)

// resendEmailVerifyCheckLimit returns true if the given normalized
// email has hit the rate limit; prunes the timestamp slice as a
// side effect.
func resendEmailVerifyCheckLimit(email string) bool {
	resendEmailVerifyMu.Lock()
	defer resendEmailVerifyMu.Unlock()
	cutoff := time.Now().Add(-resendEmailVerifyWindow)
	recent := resendEmailVerifyCounts[email]
	kept := recent[:0]
	for _, t := range recent {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	resendEmailVerifyCounts[email] = kept
	return len(kept) >= resendEmailVerifyMax
}

// resendEmailVerifyRecordHit appends a timestamp; called only after
// the resend succeeds so a 4xx doesn't burn the customer's quota.
func resendEmailVerifyRecordHit(email string) {
	resendEmailVerifyMu.Lock()
	defer resendEmailVerifyMu.Unlock()
	resendEmailVerifyCounts[email] = append(
		resendEmailVerifyCounts[email], time.Now())
}

// MintEmailVerifyToken returns a fresh url-safe base64 token. Pure
// function — does not touch the store. Caller persists.
func MintEmailVerifyToken() (string, error) {
	b := make([]byte, EmailVerifyTokenLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// IssueAndSendVerifyEmail is the shared helper called by both signup
// (for the initial verification email) and the resend endpoint. It
// mints a token, persists it, builds the click-through URL, and
// fires the email out-of-band. The verifyBaseURL argument is the
// dashboard origin (e.g. https://app.mesedi.ai); the actual landing
// route is /verify-email/confirm/<token> which the dashboard renders.
//
// Errors from token mint or persistence are returned (caller decides
// to surface). Email-send failures are logged and swallowed: a Resend
// outage must not block signup or a resend response.
func (h *Handlers) IssueAndSendVerifyEmail(
	ctx context.Context, email, projectID, projectName string,
) error {
	rawToken, err := MintEmailVerifyToken()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if err := h.Store.CreateEmailVerificationToken(ctx, &store.EmailVerificationToken{
		Token:     rawToken,
		Email:     email,
		ProjectID: projectID,
		CreatedAt: now,
		ExpiresAt: now.Add(EmailVerifyTokenTTL),
	}); err != nil {
		return err
	}

	dashboardURL := h.DashboardURL
	if dashboardURL == "" {
		dashboardURL = "https://app.mesedi.ai"
	}
	verifyURL := strings.TrimRight(dashboardURL, "/") +
		"/verify-email/confirm/" + rawToken

	// Fire the verify-augmented welcome email. Background goroutine
	// with a bounded timeout matches the existing sendWelcomeEmail
	// pattern. We send the welcome+verify combined per the design
	// decision in #232 (one email, less inbox noise).
	docsURL := h.DocsURL
	if docsURL == "" {
		docsURL = marketingOrigin(dashboardURL) + "/docs/quickstart"
	}
	in := mail.WelcomeInput{
		ToEmail:      email,
		ProjectName:  projectName,
		APIKeyPrefix: "", // resend doesn't have the key prefix; signup overrides via sendWelcomeEmail
		DashboardURL: dashboardURL + "/app",
		DocsURL:      docsURL,
		VerifyURL:    verifyURL,
	}
	go func() {
		ctxBG, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := h.Mailer.SendWelcome(ctxBG, in); err != nil {
			h.Logger.Warn("email-verify: send failed",
				"error", err.Error(), "to", email)
		}
	}()
	return nil
}

// EmailVerifyConfirmRequest is the body for POST /api/email-verify/confirm.
type EmailVerifyConfirmRequest struct {
	Token string `json:"token"`
}

// EmailVerifyConfirmResponse is the success body for the confirm
// endpoint. Includes the email so the dashboard knows whose status
// just flipped.
type EmailVerifyConfirmResponse struct {
	OK    bool   `json:"ok"`
	Email string `json:"email"`
}

// HandleEmailVerifyConfirm is the POST /api/email-verify/confirm
// handler. Public endpoint (no bearer required) because the
// recipient may not be signed in when they click the link. The
// token itself is the credential.
func (h *Handlers) HandleEmailVerifyConfirm(w http.ResponseWriter, r *http.Request) {
	var req EmailVerifyConfirmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	token := strings.TrimSpace(req.Token)
	if token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}

	record, err := h.Store.GetEmailVerificationToken(r.Context(), token)
	if err != nil {
		if errors.Is(err, store.ErrEmailTokenNotFound) {
			writeError(w, http.StatusGone,
				"this verification link is no longer valid")
			return
		}
		h.Logger.Error("email-verify confirm: read token failed",
			"error", err.Error())
		writeError(w, http.StatusInternalServerError,
			"failed to read verification token: "+err.Error())
		return
	}
	if record.UsedAt != nil {
		writeError(w, http.StatusGone,
			"this verification link is no longer valid")
		return
	}
	if time.Now().UTC().After(record.ExpiresAt) {
		writeError(w, http.StatusGone,
			"this verification link is no longer valid")
		return
	}

	if err := h.Store.MarkEmailVerified(
		r.Context(), record.Email, "email_link",
	); err != nil {
		h.Logger.Error("email-verify confirm: mark verified failed",
			"error", err.Error(), "email", record.Email)
		writeError(w, http.StatusInternalServerError,
			"failed to mark email verified: "+err.Error())
		return
	}
	if err := h.Store.MarkEmailVerificationTokenUsed(
		r.Context(), token,
	); err != nil {
		// Verification already recorded; the unused-token cleanup is
		// best-effort. Log + continue so the customer gets the
		// success they earned.
		h.Logger.Warn("email-verify confirm: mark token used failed (verification still recorded)",
			"error", err.Error(), "email", record.Email)
	}

	h.Logger.Info("email-verify confirm ok",
		"email", record.Email, "project_id", record.ProjectID)
	writeJSON(w, http.StatusOK, EmailVerifyConfirmResponse{
		OK: true, Email: record.Email,
	})
}

// EmailVerifyResendRequest is the body for POST /api/email-verify/resend.
type EmailVerifyResendRequest struct {
	Email string `json:"email"`
}

// HandleEmailVerifyResend is the POST /api/email-verify/resend
// handler. Public endpoint, always 202'd to avoid leaking which
// emails are registered.
func (h *Handlers) HandleEmailVerifyResend(w http.ResponseWriter, r *http.Request) {
	var req EmailVerifyResendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" {
		writeError(w, http.StatusBadRequest, "email is required")
		return
	}
	if !emailRegex.MatchString(email) {
		writeError(w, http.StatusBadRequest, "email format is invalid")
		return
	}

	// Rate limit. We use 202 even on rate-limit so the public path
	// never surfaces "this email was here recently" signal.
	if resendEmailVerifyCheckLimit(email) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"ok":true,"sent":false,"reason":"rate_limited"}`))
		return
	}

	// Look up the most recent project for this email. 404 => silent
	// 202 to avoid the existence oracle.
	project, err := h.Store.GetMostRecentProjectByOwnerEmail(r.Context(), email)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"ok":true,"sent":false}`))
			return
		}
		h.Logger.Error("email-verify resend: lookup project failed",
			"error", err.Error(), "email", email)
		writeError(w, http.StatusInternalServerError,
			"failed to look up account: "+err.Error())
		return
	}

	// If already verified, no-op silently. The dashboard interstitial
	// will refresh and unlock on its own poll.
	verified, err := h.Store.IsEmailVerified(r.Context(), email)
	if err != nil {
		h.Logger.Error("email-verify resend: read verified failed",
			"error", err.Error(), "email", email)
		writeError(w, http.StatusInternalServerError,
			"failed to check verified state: "+err.Error())
		return
	}
	if verified {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"ok":true,"sent":false,"reason":"already_verified"}`))
		return
	}

	if err := h.IssueAndSendVerifyEmail(
		r.Context(), email, project.ProjectID, project.Name,
	); err != nil {
		h.Logger.Error("email-verify resend: issue token failed",
			"error", err.Error(), "email", email)
		writeError(w, http.StatusInternalServerError,
			"failed to issue verification token: "+err.Error())
		return
	}
	resendEmailVerifyRecordHit(email)
	h.Logger.Info("email-verify resend ok",
		"email", email, "project_id", project.ProjectID)
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"ok":true,"sent":true}`))
}

// EmailVerifyStatusResponse is what GET /api/me/email-verification-status
// returns. The dashboard reads this once per layout mount and uses
// the verified flag to decide whether to render the interstitial.
// Method is the verified_emails.method label ("email_link",
// "magic_link", "sso_google", "sso_github", "grandfathered"); empty
// string when verified=false. The settings page reads it to suppress
// the "VERIFIED" chip on SSO-attested accounts where the label would
// be redundant.
type EmailVerifyStatusResponse struct {
	OK       bool   `json:"ok"`
	Verified bool   `json:"verified"`
	Email    string `json:"email"`
	Method   string `json:"method"`
}

// HandleEmailVerificationStatus is the GET /api/me/email-verification-status
// handler. Requires customer bearer auth (project context is set by
// the existing auth middleware). Cheap read, called frequently from
// the dashboard mount.
func (h *Handlers) HandleEmailVerificationStatus(w http.ResponseWriter, r *http.Request) {
	projectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	project, err := h.Store.GetProject(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("email-verify status: read project failed",
			"error", err.Error(), "project_id", projectID)
		writeError(w, http.StatusInternalServerError,
			"failed to read project: "+err.Error())
		return
	}
	verified, err := h.Store.IsEmailVerified(r.Context(), project.OwnerEmail)
	if err != nil {
		h.Logger.Error("email-verify status: read verified failed",
			"error", err.Error(), "project_id", projectID)
		writeError(w, http.StatusInternalServerError,
			"failed to check verified state: "+err.Error())
		return
	}
	// Fetch the method label alongside the verified flag so the
	// dashboard can decide whether to render the VERIFIED chip on
	// the settings page (we suppress it for SSO since the IdP-attested
	// flow makes the label redundant). Best-effort: a method-lookup
	// failure does not block the verified flag from reaching the
	// dashboard.
	var method string
	if verified {
		m, mErr := h.Store.GetEmailVerificationMethod(r.Context(), project.OwnerEmail)
		if mErr != nil {
			h.Logger.Warn("email-verify status: read method failed",
				"error", mErr.Error(), "project_id", projectID)
		} else {
			method = m
		}
	}
	writeJSON(w, http.StatusOK, EmailVerifyStatusResponse{
		OK: true, Verified: verified, Email: project.OwnerEmail, Method: method,
	})
}
