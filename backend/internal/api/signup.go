// Self-serve signup handler (POST /signup, public, IP-rate-limited).
//
// The v0.1 signup model creates a project per signing-up email and
// returns a single fresh API key for that project. There is no users
// table at this layer: the email lives on projects.owner_email and is
// the only identifier needed for the Cloud Hobby tier. Real user
// accounts with passwords + email verification land when multi-user
// teams are required.
//
// The endpoint is intentionally public (no auth) so a browser visiting
// /signup can create an account without first having a key. Abuse is
// blocked by an in-process IP rate limit (5 signups per IP per hour),
// which is sufficient for the pre-launch threat model. Production-grade
// abuse defense (Cloudflare Turnstile / hCaptcha + email verification +
// disposable-domain blocking) layers on later before any Show HN moment.
//
// Returned API key is shown ONCE in the response body. The frontend
// stashes it in localStorage and renders a one-time "save this key"
// screen before redirecting to /app.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"mesedi/backend/internal/mail"
	"mesedi/backend/internal/store"
)

// SignupRequest is the wire shape for POST /signup.
type SignupRequest struct {
	Email       string `json:"email"`
	ProjectName string `json:"project_name,omitempty"`
}

// SignupResponse is the wire shape returned on a successful signup.
// The api_key field is the ONLY moment the raw key is ever surfaced;
// the caller must store it immediately. ProjectName is echoed back so
// the welcome screen can confirm what was created without a follow-up
// GET /project round-trip.
type SignupResponse struct {
	OK          bool   `json:"ok"`
	ProjectID   string `json:"project_id"`
	ProjectName string `json:"project_name"`
	APIKey      string `json:"api_key"`
	KeyPrefix   string `json:"key_prefix"`
	Warning     string `json:"warning"`
}

// emailRegex is the conservative RFC-5322-subset used for v0.1 validation.
// It does not catch every invalid edge case (no email regex does) but it
// rejects the obvious junk: missing @, missing TLD, whitespace, control
// characters, multiple @ signs.
var emailRegex = regexp.MustCompile(`^[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}$`)

// signupIPLimiter is a tiny in-process IP-keyed rate limiter for the
// signup endpoint. Keyed on the client IP, tracks the timestamps of
// recent successful signups; rejects when an IP has 5 or more signups
// in the last hour. The map grows unboundedly in theory but in practice
// the working set is small (one entry per IP that has signed up
// recently) and entries naturally fall out of the 1-hour window.
var (
	signupIPMu     sync.Mutex
	signupIPCounts = map[string][]time.Time{}
)

const (
	signupRateLimitWindow = time.Hour
	signupRateLimitMax    = 5
)

// signupCheckIPLimit returns true if the given IP has too many recent
// signups. It also prunes timestamps outside the window as a side
// effect (no separate cleanup goroutine needed for v0.1 scale).
func signupCheckIPLimit(ip string) bool {
	signupIPMu.Lock()
	defer signupIPMu.Unlock()
	cutoff := time.Now().Add(-signupRateLimitWindow)
	recent := signupIPCounts[ip]
	kept := recent[:0]
	for _, t := range recent {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	signupIPCounts[ip] = kept
	return len(kept) >= signupRateLimitMax
}

// signupRecordIPHit appends the current time to the IP's signup log.
// Called only after a signup completes successfully so failed attempts
// (validation errors, etc.) don't burn rate-limit quota.
func signupRecordIPHit(ip string) {
	signupIPMu.Lock()
	defer signupIPMu.Unlock()
	signupIPCounts[ip] = append(signupIPCounts[ip], time.Now())
}

// extractClientIP returns the client IP from the request, preferring
// the leftmost X-Forwarded-For entry if Mesedi is running behind a
// proxy (Fly.io, Cloudflare). Falls back to RemoteAddr for direct
// connections.
func extractClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if comma := strings.IndexByte(xff, ','); comma >= 0 {
			return strings.TrimSpace(xff[:comma])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// HandleSignup is the public POST /signup handler. Body is a JSON
// SignupRequest. On success returns 201 + SignupResponse. On
// validation failure returns 400 with a human-readable message. On
// rate-limit hit returns 429.
func (h *Handlers) HandleSignup(w http.ResponseWriter, r *http.Request) {
	// 1. Rate limit by client IP.
	ip := extractClientIP(r)
	if signupCheckIPLimit(ip) {
		writeError(w, http.StatusTooManyRequests, "too many signups from this IP. Try again in an hour.")
		return
	}

	// 2. Decode + validate body.
	var req SignupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" {
		writeError(w, http.StatusBadRequest, "email is required")
		return
	}
	if len(email) > 254 {
		writeError(w, http.StatusBadRequest, "email is too long")
		return
	}
	if !emailRegex.MatchString(email) {
		writeError(w, http.StatusBadRequest, "email format is invalid")
		return
	}
	projectName := strings.TrimSpace(req.ProjectName)
	if projectName == "" {
		projectName = "Default project"
	}
	if len(projectName) > 80 {
		writeError(w, http.StatusBadRequest, "project name must be 80 characters or fewer")
		return
	}

	// 3. Create the project.
	now := time.Now().UTC()
	projectID := fmt.Sprintf("proj_%d", now.UnixNano())
	project := &store.Project{
		ProjectID:  projectID,
		Name:       projectName,
		OwnerEmail: email,
		CreatedAt:  now,
	}
	if err := h.Store.CreateProject(r.Context(), project); err != nil {
		h.Logger.Error("signup: create project failed", "error", err.Error(), "email", email)
		writeError(w, http.StatusInternalServerError, "failed to create project: "+err.Error())
		return
	}

	// 4. Mint the first API key for this project.
	rawKey, hash, prefix, err := MintAPIKey()
	if err != nil {
		h.Logger.Error("signup: mint key failed", "error", err.Error(), "project_id", projectID)
		writeError(w, http.StatusInternalServerError, "failed to mint API key: "+err.Error())
		return
	}
	keyID := fmt.Sprintf("key-%s-%d", prefix[len("mesedi_sk_"):], now.UnixNano())
	keyRecord := &store.APIKey{
		KeyID:     keyID,
		ProjectID: projectID,
		KeyHash:   hash,
		KeyPrefix: prefix,
		Name:      "Signup key",
	}
	if err := h.Store.CreateAPIKey(r.Context(), keyRecord); err != nil {
		h.Logger.Error("signup: persist key failed", "error", err.Error(), "project_id", projectID)
		// Project was created but key was not. Leave the orphan project
		// in place; the user can retry signup with a different email or
		// contact support. The alternative (rolling back the project)
		// requires a Store-level transaction surface that does not
		// exist yet at v0.1.
		writeError(w, http.StatusInternalServerError, "failed to persist API key: "+err.Error())
		return
	}

	// 5. Burn one rate-limit slot only on success. Failed attempts
	//    above (bad email, etc.) do not count against the IP quota.
	signupRecordIPHit(ip)

	h.Logger.Info("signup ok",
		"project_id", projectID,
		"key_prefix", prefix,
		"email", email,
		"ip", ip,
	)

	// 6. Fire the welcome email out-of-band. Non-blocking: if Resend
	//    is slow or down, the user still gets their key. NoopMailer
	//    in local dev makes this a no-op.
	h.sendWelcomeEmail(email, projectName, prefix)

	writeJSON(w, http.StatusCreated, SignupResponse{
		OK:          true,
		ProjectID:   projectID,
		ProjectName: projectName,
		APIKey:      rawKey,
		KeyPrefix:   prefix,
		Warning:     "Store this api_key now. It will never be shown again.",
	})
}

// sendWelcomeEmail dispatches the welcome template on a background
// goroutine with a bounded context. Errors are logged, never surfaced
// to the signing-up user; a Resend outage must not block a successful
// signup.
func (h *Handlers) sendWelcomeEmail(toEmail, projectName, keyPrefix string) {
	dashboardURL := h.DashboardURL
	if dashboardURL == "" {
		dashboardURL = "https://mesedi.vercel.app"
	}
	docsURL := h.DocsURL
	if docsURL == "" {
		docsURL = dashboardURL + "/docs/quickstart"
	}

	in := mail.WelcomeInput{
		ToEmail:      toEmail,
		ProjectName:  projectName,
		APIKeyPrefix: keyPrefix,
		DashboardURL: dashboardURL + "/app",
		DocsURL:      docsURL,
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := h.Mailer.SendWelcome(ctx, in); err != nil {
			h.Logger.Warn("signup: welcome email failed",
				"error", err.Error(),
				"to", toEmail,
			)
		}
	}()
}
