// Magic-link sign-in handlers (commit 2).
//
// Two endpoints:
//
//   POST /magic-link/start
//     Body: { email }
//     Behavior: rate-limit by IP, mint a random 32-byte token,
//     hash + persist, email the verify URL to the customer. ALWAYS
//     returns 202 even if no account exists for the email (we do
//     not want this endpoint to act as an oracle leaking which
//     emails are registered customers).
//
//   GET /magic-link/verify?token=<raw>
//     Behavior: SHA-256 the raw token, look up the row, reject if
//     missing / expired / used, mark used, then HAND OFF to the
//     dashboard server -- this endpoint returns a redirect to the
//     dashboard's /api/auth/magic-link/handoff?token_id=<id>&email=<email>
//     route, which calls /signin on the backend and writes the
//     resulting session-grade key into a cookie. (The verify endpoint
//     itself does NOT call /signin because /signin requires the
//     server-to-server secret which lives on the dashboard server,
//     not in the verify response path.)
//
// The dashboard server's /api/auth/magic-link/handoff route is the
// natural mirror of the OAuth callback route: same cookie-write
// pattern, same login-success redirect to /login?status=sso-success.

package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"mesedi/backend/internal/mail"
	"mesedi/backend/internal/store"
)

// MagicLinkStartRequest is the body of POST /magic-link/start.
type MagicLinkStartRequest struct {
	Email string `json:"email"`
}

// MagicLinkStartResponse is what /magic-link/start returns. We
// intentionally always echo ok=true regardless of whether the email
// has an account; the response shape MUST NOT differ between hit
// and miss to avoid acting as an email enumeration oracle.
type MagicLinkStartResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// Rate limit per IP. Tighter than signup (3/hour vs 5/hour for
// signup) because the magic-link send fires a real email; abuse
// blast radius is higher.
const (
	magicLinkRateLimitWindow = time.Hour
	magicLinkRateLimitMax    = 3
)

var (
	magicLinkIPMu     sync.Mutex
	magicLinkIPCounts = map[string][]time.Time{}
)

func magicLinkCheckIPLimit(ip string) bool {
	magicLinkIPMu.Lock()
	defer magicLinkIPMu.Unlock()
	cutoff := time.Now().Add(-magicLinkRateLimitWindow)
	recent := magicLinkIPCounts[ip]
	kept := recent[:0]
	for _, t := range recent {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	magicLinkIPCounts[ip] = kept
	return len(kept) >= magicLinkRateLimitMax
}

func magicLinkRecordIPHit(ip string) {
	magicLinkIPMu.Lock()
	defer magicLinkIPMu.Unlock()
	magicLinkIPCounts[ip] = append(magicLinkIPCounts[ip], time.Now())
}

// hashMagicLinkToken returns the SHA-256 hex of a raw token. Same
// scheme as api/auth.go's HashAPIKey -- hex is friendlier for DB
// inspection than base64, and the constant-length output simplifies
// index sizing.
func hashMagicLinkToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// mintMagicLinkToken returns a fresh 32-byte URL-safe random token.
// 32 bytes = 256 bits of entropy, the same budget as a session
// cookie; base64url so the token rides cleanly in a URL.
func mintMagicLinkToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// HandleMagicLinkStart accepts POST /magic-link/start. Always 202s
// even on miss to avoid email-enumeration; logs the miss server-side.
func (h *Handlers) HandleMagicLinkStart(w http.ResponseWriter, r *http.Request) {
	ip := extractClientIP(r)
	if magicLinkCheckIPLimit(ip) {
		writeError(w, http.StatusTooManyRequests,
			"too many magic-link requests from this IP. Try again in an hour.")
		return
	}

	var req MagicLinkStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" || len(email) > 254 || !emailRegex.MatchString(email) {
		writeError(w, http.StatusBadRequest, "email format is invalid")
		return
	}

	// Look up the project. If absent, still return 202 + log -- this
	// keeps /magic-link/start from acting as an oracle that confirms
	// which emails are real customers.
	project, err := h.Store.GetMostRecentProjectByOwnerEmail(r.Context(), email)
	if errors.Is(err, store.ErrNotFound) {
		h.Logger.Info("magic-link: no account for email (silent 202)",
			"email", email, "ip", ip)
		magicLinkRecordIPHit(ip)
		writeJSON(w, http.StatusAccepted, MagicLinkStartResponse{
			OK:      true,
			Message: "If an account exists for that email, a sign-in link is on the way.",
		})
		return
	}
	if err != nil {
		h.Logger.Error("magic-link: lookup project failed",
			"error", err.Error(), "email", email)
		writeError(w, http.StatusInternalServerError,
			"failed to look up account: "+err.Error())
		return
	}

	// Mint + persist token (store the hash, never the raw).
	rawToken, err := mintMagicLinkToken()
	if err != nil {
		h.Logger.Error("magic-link: mint token failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError,
			"failed to mint sign-in token: "+err.Error())
		return
	}
	now := time.Now().UTC()
	tokenRecord := &store.MagicLinkToken{
		TokenID:   fmt.Sprintf("mlt_%d", now.UnixNano()),
		TokenHash: hashMagicLinkToken(rawToken),
		Email:     email,
		CreatedAt: now,
		ExpiresAt: now.Add(store.MagicLinkTTL),
		RequestIP: ip,
	}
	if err := h.Store.CreateMagicLinkToken(r.Context(), tokenRecord); err != nil {
		h.Logger.Error("magic-link: persist token failed",
			"error", err.Error(), "email", email)
		writeError(w, http.StatusInternalServerError,
			"failed to persist sign-in token: "+err.Error())
		return
	}

	// Build the sign-in URL pointing at the dashboard server's
	// handoff route. The handoff route reads the token, calls
	// /magic-link/verify on the backend to burn it + fetch the
	// email, then calls /signin to mint a session-grade key, and
	// finally redirects the customer to /login?status=sso-success
	// with the cookie set. Splitting verify (burn-the-token,
	// backend) from handoff (mint-the-session-key, dashboard) keeps
	// the trust boundaries clean: the backend never has to know
	// the dashboard URL beyond the configured DashboardURL field.
	signInURL := h.DashboardURL
	if signInURL == "" {
		signInURL = "https://app.mesedi.ai"
	}
	signInURL = strings.TrimRight(signInURL, "/") +
		"/api/auth/magic-link/handoff?token=" + rawToken

	// Best-effort send. The customer's UX is the same whether or
	// not Resend is up -- they see "check your email" -- so we do
	// not fail the request on a mailer error. We do log so an
	// operator notices a Resend outage.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := h.Mailer.SendMagicLink(ctx, mail.MagicLinkInput{
			ToEmail:   email,
			SignInURL: signInURL,
			ExpiresAt: tokenRecord.ExpiresAt,
		}); err != nil {
			h.Logger.Warn("magic-link: send email failed (token persisted, customer will not receive link)",
				"error", err.Error(), "email", email)
		}
	}()

	magicLinkRecordIPHit(ip)
	h.Logger.Info("magic-link: sent",
		"email", email,
		"project_id", project.ProjectID,
		"token_id", tokenRecord.TokenID,
		"ip", ip,
	)
	writeJSON(w, http.StatusAccepted, MagicLinkStartResponse{
		OK:      true,
		Message: "If an account exists for that email, a sign-in link is on the way.",
	})
}

// MagicLinkVerifyResponse is what /magic-link/verify returns to the
// dashboard server's handoff route on success. The dashboard server
// turns this into a /signin call.
type MagicLinkVerifyResponse struct {
	OK    bool   `json:"ok"`
	Email string `json:"email"`
}

// HandleMagicLinkVerify accepts GET /magic-link/verify?token=<raw>.
// Burns the token (one click only) and returns the email the
// dashboard server should call /signin with. Like /signin itself,
// this endpoint is gated by the MESEDI_SIGNIN_SECRET so only the
// dashboard server can call it. (If we let the browser call verify
// directly, a flow that succeeds without the dashboard server's
// cookie-write step would leak the email back unauthenticated.)
func (h *Handlers) HandleMagicLinkVerify(w http.ResponseWriter, r *http.Request) {
	if h.SigninSecret == "" {
		writeError(w, http.StatusServiceUnavailable,
			"magic-link verify not configured (MESEDI_SIGNIN_SECRET unset)")
		return
	}
	if r.Header.Get(signinHeaderSecret) != h.SigninSecret {
		writeError(w, http.StatusUnauthorized,
			"magic-link verify requires server-to-server credential")
		return
	}
	raw := strings.TrimSpace(r.URL.Query().Get("token"))
	if raw == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	tokenHash := hashMagicLinkToken(raw)
	record, err := h.Store.GetMagicLinkTokenByHash(r.Context(), tokenHash)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "sign-in link is invalid")
		return
	}
	if err != nil {
		h.Logger.Error("magic-link verify: lookup failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "lookup failed: "+err.Error())
		return
	}
	if !record.UsedAt.IsZero() {
		writeError(w, http.StatusGone, "sign-in link has already been used")
		return
	}
	if time.Now().UTC().After(record.ExpiresAt) {
		writeError(w, http.StatusGone, "sign-in link has expired")
		return
	}
	// Mark used. ErrNotFound here means a concurrent verify burned
	// the token between our read and write; surface as "already
	// used" so the dashboard server can render the same UX.
	if err := h.Store.MarkMagicLinkTokenUsed(r.Context(), record.TokenID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusGone, "sign-in link has already been used")
			return
		}
		h.Logger.Error("magic-link verify: mark used failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "burn token failed: "+err.Error())
		return
	}
	// Clicking the magic link proves the customer owns the inbox, so
	// flip the email-verified bit too. Best-effort: a DB hiccup
	// must not block the sign-in handoff about to happen.
	if err := h.Store.MarkEmailVerified(r.Context(), record.Email, "magic_link"); err != nil {
		h.Logger.Warn("magic-link verify: mark email verified failed (sign-in still proceeds)",
			"error", err.Error(), "email", record.Email)
	}

	h.Logger.Info("magic-link verify ok",
		"token_id", record.TokenID,
		"email", record.Email,
	)
	writeJSON(w, http.StatusOK, MagicLinkVerifyResponse{
		OK:    true,
		Email: record.Email,
	})
}
