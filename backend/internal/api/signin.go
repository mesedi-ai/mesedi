// Server-to-server sign-in handler (POST /signin, #196 commit 1).
//
// Architecture. The dashboard server (Cloudflare Workers running the
// Next.js routes under /api/oauth/*/callback and /api/magic-link/verify)
// has two ways it learns a verified email belongs to a real human:
//
//   1. SSO callback: provider exchanged code -> access token -> verified
//      email at Google/GitHub. Email ownership proven by the OAuth
//      handshake (the provider sent back a verified email).
//
//   2. Magic-link verify (commit 2): the customer clicked a one-time
//      link emailed to them; possession of the token in the URL proves
//      the email belongs to the person clicking.
//
// Once email ownership is proven, the dashboard server needs to put a
// valid bearer credential into the customer's browser so subsequent
// /me/* dashboard calls authenticate as that customer's project. The
// dashboard server cannot mint API keys itself (it does not have
// database access; the backend on Fly does). So it calls this
// endpoint, hands us the email + source, gets back a fresh API key,
// and ships that key to the browser via a short-lived cookie.
//
// Trust boundary. /signin is a public HTTP endpoint on the Mesedi
// backend (api.mesedi.ai), and the email parameter is whatever the
// caller hands us -- there is no second factor here. The dashboard
// server's OAuth callback / magic-link verify ARE the trust boundary:
// they call /signin only after the email-ownership proof completes.
// To stop random unauthenticated callers from minting login keys
// for any email by hitting /signin directly, we require a shared
// secret in the X-Mesedi-Signin-Secret header. Constant-time compare.
// Empty SigninSecret (not configured) disables the endpoint with 503.
//
// What gets minted. A fresh mesedi_sk_* key tagged source='sso_login'
// or source='magic_link' (controlled by request body) with
// expires_at = now + 7 days. The store layer filters these
// session-grade rows out of the customer-facing /admin/api-keys
// listing (#196 design note) so the customer never sees them.
//
// What does NOT happen here. We do not revoke existing keys: the
// customer's signup key + any manually-minted SDK keys keep working
// untouched. We do not return the customer's original key: keys are
// stored as SHA-256 hashes, so it is not recoverable. We do not
// create new projects: if the email has no project, return 404 with a
// clear "no account for this email" error, and the dashboard caller
// surfaces that as a /signup redirect instead.
//
// Audit log. Every successful /signin writes one audit_events row
// (#207) with action='sso_login' (or 'magic_link'), actor_email=the
// signed-in email, actor_key_id=the freshly-minted key id, and
// metadata_json describing the source. Failures (404, 401) do NOT
// write audit rows; we only audit successful auth events for now.

package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"mesedi/backend/internal/store"
)

// SigninRequest is the wire shape POST /signin accepts. Source must
// be one of the two session-grade APIKeySource* constants; any other
// value (including "manual" / "signup") is rejected to prevent the
// dashboard server from accidentally minting a long-lived key via the
// session endpoint.
type SigninRequest struct {
	Email  string `json:"email"`
	Source string `json:"source"`
}

// SigninResponse is the wire shape returned on a successful /signin.
// Mirrors SignupResponse so the dashboard server's OAuth callback and
// magic-link verify can share a single cookie-write code path.
type SigninResponse struct {
	OK          bool   `json:"ok"`
	APIKey      string `json:"api_key"`
	ProjectID   string `json:"project_id"`
	ProjectName string `json:"project_name"`
	KeyPrefix   string `json:"key_prefix"`
	Warning     string `json:"warning"`
	// SessionToken is the raw HttpOnly cookie value the dashboard
	// Worker writes onto its browser-facing response (#213 Batch 2).
	// The backend persists only the SHA-256 hash in the sessions
	// table; this raw value is shown exactly once, in this response.
	// Empty during the Batch 2 transition window for callers older
	// than the Batch 3 Worker code, which still consume APIKey from
	// the same payload. Batch 3 removes APIKey from this response.
	SessionToken string `json:"session_token,omitempty"`
	// SessionExpiresAt mirrors SessionToken's expires_at column from
	// the sessions table as an RFC 3339 string. Lets the Worker set
	// the cookie Max-Age without re-deriving the TTL on its side.
	SessionExpiresAt string `json:"session_expires_at,omitempty"`
}

// signinHeaderSecret is the name of the request header the dashboard
// server uses to present the shared secret. Distinct from
// "Authorization" so it cannot be mistaken for a customer bearer
// token in logs / WAF rules / load-balancer headers stripping.
const signinHeaderSecret = "X-Mesedi-Signin-Secret"

// HandleSignin is the POST /signin handler. See the file-level doc
// comment for the trust model and the wire contract.
func (h *Handlers) HandleSignin(w http.ResponseWriter, r *http.Request) {
	// 1. Gate the endpoint on the shared secret being configured.
	//    Local dev without MESEDI_SIGNIN_SECRET set must NOT silently
	//    accept all callers; we 503 so the dashboard server's OAuth
	//    callback surfaces a "backend not configured" error and we
	//    notice quickly during smoke tests.
	if h.SigninSecret == "" {
		writeError(w, http.StatusServiceUnavailable,
			"signin endpoint not configured (MESEDI_SIGNIN_SECRET unset)")
		return
	}

	// 2. Constant-time compare the request's header secret against
	//    the configured value. Equal-length compare avoids the
	//    length-channel leak from naive string equality. If the
	//    header is missing or wrong, return 401 with a deliberately
	//    vague message; we do NOT want to confirm "the header was
	//    present but the value was wrong" because that helps brute
	//    force the secret one character at a time.
	presented := r.Header.Get(signinHeaderSecret)
	if subtle.ConstantTimeCompare([]byte(presented), []byte(h.SigninSecret)) != 1 {
		writeError(w, http.StatusUnauthorized, "signin requires server-to-server credential")
		return
	}

	// 3. Decode + validate body. Conservative caps on field lengths
	//    so a malformed dashboard call cannot blow up the JSON
	//    parser or thunder through the email regex.
	var req SigninRequest
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
	source := strings.TrimSpace(req.Source)
	if source != store.APIKeySourceSSOLogin && source != store.APIKeySourceMagicLink {
		// Reject any source other than the two session-grade values.
		// In particular, refuse "signup" / "manual" so the dashboard
		// server cannot accidentally mint a long-lived key here.
		writeError(w, http.StatusBadRequest,
			"source must be 'sso_login' or 'magic_link'")
		return
	}

	// 4. Look up the most recent project for this email. 404 is the
	//    expected "no account" response; the dashboard server's
	//    OAuth callback maps this to a friendly redirect such as
	//    /signup?reason=no-account-for-email.
	project, err := h.Store.GetMostRecentProjectByOwnerEmail(r.Context(), email)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no account exists for that email")
		return
	}
	if err != nil {
		h.Logger.Error("signin: lookup project failed",
			"error", err.Error(), "email", email)
		writeError(w, http.StatusInternalServerError,
			"failed to look up account: "+err.Error())
		return
	}

	// 5. Mint a fresh API key bound to that project. Tagged with
	//    source = request.source and expires_at = now + 7 days so
	//    the store layer hides it from the customer-facing listing
	//    AND the existing time-boxed-credentials check rejects the
	//    bearer once the window closes. We persist the source-tagged
	//    name onto the key record so a future debug session sees
	//    "Google SSO sign-in" rather than the generic "Signup key"
	//    when this key was the bearer for some misbehaving request.
	rawKey, hash, prefix, err := MintAPIKey()
	if err != nil {
		h.Logger.Error("signin: mint key failed",
			"error", err.Error(), "email", email)
		writeError(w, http.StatusInternalServerError,
			"failed to mint API key: "+err.Error())
		return
	}
	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(store.APIKeyLoginExpiryDays) * 24 * time.Hour)
	keyName := signinKeyName(source)
	keyID := fmt.Sprintf("key-%s-%d", prefix[len("mesedi_sk_"):], now.UnixNano())
	keyRecord := &store.APIKey{
		KeyID:     keyID,
		ProjectID: project.ProjectID,
		KeyHash:   hash,
		KeyPrefix: prefix,
		Name:      keyName,
		UserID:    email,
		Source:    source,
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339Nano),
	}
	if err := h.Store.CreateAPIKey(r.Context(), keyRecord); err != nil {
		h.Logger.Error("signin: persist key failed",
			"error", err.Error(), "email", email, "project_id", project.ProjectID)
		writeError(w, http.StatusInternalServerError,
			"failed to persist API key: "+err.Error())
		return
	}

	// 5b. Mark the email verified (#232). SSO callers reach signin
	//     only after the IdP has attested the email; magic-link
	//     callers reach signin only after the customer clicked the
	//     link in their inbox. Either way, ownership is proved.
	//     Best-effort: a transient DB error must not block sign-in
	//     (the customer still gets their session key); the next
	//     successful sign-in will re-mark.
	if err := h.Store.MarkEmailVerified(r.Context(), email, source); err != nil {
		h.Logger.Warn("signin: mark email verified failed (sign-in still succeeded)",
			"error", err.Error(), "email", email, "source", source)
	}

	// 6. Best-effort audit log. Failure here MUST NOT block the
	//    signin -- the customer would be left in a half-completed
	//    state with a key minted but a 500 response, which is worse
	//    than silently missing an audit row. The retention job will
	//    surface any missing rows during routine consistency checks.
	auditAction := source // "sso_login" or "magic_link"
	auditEvent := &store.AuditEvent{
		EventID:      fmt.Sprintf("evt_%d", now.UnixNano()),
		ProjectID:    project.ProjectID,
		ActorKeyID:   keyID,
		ActorKeyName: keyName,
		ActorEmail:   email,
		Action:       auditAction,
		TargetType:   "api_key",
		TargetID:     keyID,
		MetadataJSON: fmt.Sprintf(`{"source":%q,"expires_at":%q}`,
			source, keyRecord.ExpiresAt),
		CreatedAt: now,
	}
	if err := h.Store.CreateAuditEvent(r.Context(), auditEvent); err != nil {
		h.Logger.Warn("signin: audit log write failed (signin still succeeded)",
			"error", err.Error(), "email", email, "project_id", project.ProjectID)
	}

	// 6b. #213 Batch 2: also create a real session row + return its
	//     raw token to the Worker. The dashboard's Worker writes the
	//     raw token into a browser-side HttpOnly cookie in Batch 3;
	//     once the dashboard is fully cut over we drop the API key
	//     from this response. Best-effort: a session-create failure
	//     does NOT roll back the signin because the API-key path
	//     still works for legacy / SDK consumers.
	var sessionToken string
	var sessionExpiresAt time.Time
	rawSess, sessHash, sessErr := MintSessionToken()
	if sessErr != nil {
		h.Logger.Warn("signin: mint session token failed (API-key path still works)",
			"error", sessErr.Error(), "email", email, "project_id", project.ProjectID)
	} else {
		sessionExpiresAt = now.Add(SessionTTL)
		sess := &store.Session{
			TokenHash:  sessHash,
			UserID:     email,
			ProjectID:  project.ProjectID,
			CreatedAt:  now,
			ExpiresAt:  sessionExpiresAt,
			LastUsedAt: now,
			UserAgent:  r.Header.Get("User-Agent"),
			IPAddress:  extractClientIP(r),
		}
		if persistErr := h.Store.CreateSession(r.Context(), sess); persistErr != nil {
			h.Logger.Warn("signin: persist session failed (API-key path still works)",
				"error", persistErr.Error(), "email", email, "project_id", project.ProjectID)
		} else {
			sessionToken = rawSess
		}
	}

	h.Logger.Info("signin ok",
		"project_id", project.ProjectID,
		"key_prefix", prefix,
		"email", email,
		"source", source,
		"session_created", sessionToken != "",
	)

	// 7. Return the fresh key + the fresh session token to the
	//    dashboard server. The dashboard server writes these into
	//    short-lived cookies that the welcome / login page reads
	//    exactly once. The raw values never live in URL query
	//    strings, Referer headers, or server logs. The two paths
	//    coexist during the #213 cutover; Batch 3 drops APIKey.
	resp := SigninResponse{
		OK:          true,
		APIKey:      rawKey,
		ProjectID:   project.ProjectID,
		ProjectName: project.Name,
		KeyPrefix:   prefix,
		Warning: "Session-grade key. Expires in " +
			fmt.Sprint(store.APIKeyLoginExpiryDays) +
			" days. Hidden from the API keys list.",
	}
	if sessionToken != "" {
		resp.SessionToken = sessionToken
		resp.SessionExpiresAt = sessionExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusCreated, resp)
}

// signinKeyName produces a human-readable name for the api_keys.name
// column so future operators reading /admin/api-keys (or the audit
// log) immediately see how the key was minted. Customer-facing
// listings hide these rows entirely (see ListAPIKeysForProject's
// filter), so the name is operator-only.
func signinKeyName(source string) string {
	switch source {
	case store.APIKeySourceSSOLogin:
		return "SSO sign-in"
	case store.APIKeySourceMagicLink:
		return "Magic-link sign-in"
	default:
		// Defensive: HandleSignin already validated the source above,
		// so this branch is unreachable. If a future refactor opens
		// the constraint, fall back to a non-empty name rather than
		// inserting an empty string.
		return "Session-grade sign-in"
	}
}
