// Automated abuse and issue detection (#172).
//
// Backs the ToS commitment to "Suspension or termination for cause"
// with a 24-hour notification window. The state machine for any
// single abuse signal:
//
//   detected   -> a detector calls Record... and writes a row
//   notified   -> background worker emails the project owner with
//                 24h notice (notified_at stamped)
//   suspended  -> background worker, 24h after notified_at, flips
//                 projects.suspended_at + suspension_reason; auth
//                 middleware then rejects requests for the project
//                 with 403
//   resolved   -> human operator dismisses via /admin/abuse/<id>/resolve
//                 (optionally reactivating a suspended project)
//
// Detectors today (see Record... methods):
//   - rate_limit_sustained: 30+ 429s in a 60min rolling window
//   - key_leak: same API key seen from 10+ distinct IPs in 60min
//   - oversized_payload: 5+ requests over 1MB body in 60min
//   - suspicious_signup: rapid same-IP signups (5+ in 1h, fires once
//     per IP per day)
//
// Storage model: every detector keeps an in-memory rolling counter
// per project (cheap, restart-safe semantics described per-method)
// and writes a persistent row in abuse_signals when the threshold
// crosses. The persistent table is what the worker reads from. State
// resets on backend restart but already-detected signals stay in the
// DB and continue progressing through the state machine.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"mesedi/backend/internal/mail"
	"mesedi/backend/internal/store"
)

// Abuse-detection thresholds. Tuned generously: a real
// runaway-loop client comfortably blows past these in a few
// minutes; a well-behaved client never approaches them.
const (
	abuseWindow = 60 * time.Minute

	// Rate-limit detector: 30+ 429s in a 60-minute window.
	rateLimitAbuseThreshold = 30

	// Key-leak detector: 10+ distinct IPs for the same key in 60min.
	// A legitimate prod deploy fans out across at most 2-5 IPs
	// (multiple instances, blue/green). 10 distinct IPs in an hour is
	// the classic "key was leaked and is being abused" pattern.
	keyLeakThreshold = 10

	// Oversized-payload detector: 5+ requests with body > 1MB in
	// 60min. Legitimate event batches are < 100KB; consistent
	// megabyte+ posts are either a misuse or an attempt to fill the
	// volume.
	oversizedPayloadThreshold     = 5
	oversizedPayloadByteThreshold = 1_000_000

	// Suspicious-signup detector: 5+ signups from the same IP in
	// 60min. Disposable-domain detection is on the deferred list;
	// rate of signup is a cheap proxy.
	suspiciousSignupThreshold = 5

	// Cooldown between repeat signals for the same key. Prevents
	// alert spam from a sustained event.
	abuseSignalCooldown = 60 * time.Minute

	// Background worker cadence. Every loop scans unresolved signals
	// and progresses any past the next state-machine transition.
	worker24h         = 24 * time.Hour
	workerScanCadence = 5 * time.Minute
)

// AbuseDetector owns the in-memory rolling counters for each detector
// kind plus the persistence + alert side effects. One detector per
// backend process; carry it through Handlers.
type AbuseDetector struct {
	mu sync.Mutex

	// Per-project state for the rate-limit detector.
	rateLimitProjects map[string]*timeWindowState

	// Per-key state for the key-leak detector. Map of key prefix
	// (not the full key, we never store that) to the rolling IP set.
	keyLeakKeys map[string]*ipWindowState

	// Per-project state for the oversized-payload detector.
	oversizedPayloadProjects map[string]*timeWindowState

	// Per-IP state for the suspicious-signup detector.
	signupIPs map[string]*timeWindowState

	// Per-project signal cooldown.
	signalCooldown map[string]time.Time

	logger *slog.Logger
	store  store.Store
}

type timeWindowState struct {
	hits []time.Time
}

type ipWindowState struct {
	// ips is a map from IP string to most-recent-hit time, so pruning
	// the window naturally trims the distinct-IP count.
	ips map[string]time.Time
}

// NewAbuseDetector is the constructor. store may be nil in tests or
// in local-dev runs without persistence; when nil, the detector still
// fires alerts but skips the DB insert.
func NewAbuseDetector(logger *slog.Logger, s store.Store) *AbuseDetector {
	return &AbuseDetector{
		rateLimitProjects:        make(map[string]*timeWindowState),
		keyLeakKeys:              make(map[string]*ipWindowState),
		oversizedPayloadProjects: make(map[string]*timeWindowState),
		signupIPs:                make(map[string]*timeWindowState),
		signalCooldown:           make(map[string]time.Time),
		logger:                   logger,
		store:                    s,
	}
}

// RecordRateLimit feeds one 429 event into the detector. Called by
// the rate-limit middleware on every 429 response.
func (d *AbuseDetector) RecordRateLimit(ctx context.Context, projectID, method, path string) {
	if projectID == "" {
		return
	}
	now := time.Now()

	count := d.touchTimeWindow(d.rateLimitProjects, projectID, now)
	if count < rateLimitAbuseThreshold {
		return
	}
	cooldownKey := "rate_limit:" + projectID
	if !d.checkAndSetCooldown(cooldownKey, now) {
		return
	}

	detail, _ := json.Marshal(map[string]any{
		"count":       count,
		"window_mins": int(abuseWindow.Minutes()),
		"method":      method,
		"path":        path,
	})
	d.fire(ctx, store.AbuseSignal{
		ProjectID:  projectID,
		Kind:       "rate_limit_sustained",
		Severity:   "warning",
		Detail:     string(detail),
		DetectedAt: now,
	})
}

// RecordRequestForKeyLeak feeds one authenticated request into the
// detector. Called by the auth middleware after a key has been
// validated. keyPrefix is the public part (mesedi_sk_abc123); the
// detector never sees the raw key.
func (d *AbuseDetector) RecordRequestForKeyLeak(ctx context.Context, projectID, keyPrefix, ip string) {
	if projectID == "" || keyPrefix == "" || ip == "" {
		return
	}
	now := time.Now()

	distinctIPs := d.touchIPWindow(keyPrefix, ip, now)
	if distinctIPs < keyLeakThreshold {
		return
	}
	cooldownKey := "key_leak:" + keyPrefix
	if !d.checkAndSetCooldown(cooldownKey, now) {
		return
	}

	detail, _ := json.Marshal(map[string]any{
		"key_prefix":   keyPrefix,
		"distinct_ips": distinctIPs,
		"window_mins":  int(abuseWindow.Minutes()),
	})
	d.fire(ctx, store.AbuseSignal{
		ProjectID:  projectID,
		Kind:       "key_leak",
		Severity:   "critical",
		Detail:     string(detail),
		DetectedAt: now,
	})
}

// RecordOversizedPayload feeds one >1MB request into the detector.
// Called by the request-size middleware on every body that exceeds
// the threshold.
func (d *AbuseDetector) RecordOversizedPayload(ctx context.Context, projectID string, bytesSeen int64, method, path string) {
	if projectID == "" {
		return
	}
	now := time.Now()

	count := d.touchTimeWindow(d.oversizedPayloadProjects, projectID, now)
	if count < oversizedPayloadThreshold {
		return
	}
	cooldownKey := "oversized:" + projectID
	if !d.checkAndSetCooldown(cooldownKey, now) {
		return
	}

	detail, _ := json.Marshal(map[string]any{
		"count":       count,
		"window_mins": int(abuseWindow.Minutes()),
		"bytes":       bytesSeen,
		"method":      method,
		"path":        path,
	})
	d.fire(ctx, store.AbuseSignal{
		ProjectID:  projectID,
		Kind:       "oversized_payload",
		Severity:   "warning",
		Detail:     string(detail),
		DetectedAt: now,
	})
}

// RecordSignupFromIP feeds one successful signup into the detector.
// Called by HandleSignup after the project + key have been minted.
// Unlike the other detectors, this one is IP-scoped (not
// project-scoped): a single bad actor signing up many emails shows
// up as one IP creating many projects in quick succession. The
// signal records the IP in the Detail field; the persistent row's
// project_id is the most-recently-created one (least useful, but
// makes the row link to a real project for the admin UI).
func (d *AbuseDetector) RecordSignupFromIP(ctx context.Context, ip, newProjectID, email string) {
	if ip == "" || newProjectID == "" {
		return
	}
	now := time.Now()

	count := d.touchTimeWindow(d.signupIPs, ip, now)
	if count < suspiciousSignupThreshold {
		return
	}
	cooldownKey := "signup:" + ip
	if !d.checkAndSetCooldown(cooldownKey, now) {
		return
	}

	detail, _ := json.Marshal(map[string]any{
		"ip":          ip,
		"count":       count,
		"window_mins": int(abuseWindow.Minutes()),
		"latest_email": email,
	})
	d.fire(ctx, store.AbuseSignal{
		ProjectID:  newProjectID,
		Kind:       "suspicious_signup",
		Severity:   "warning",
		Detail:     string(detail),
		DetectedAt: now,
	})
}

// touchTimeWindow appends a timestamp to the given map's per-key
// rolling window, prunes the entries outside the window, and returns
// the post-prune count. Caller must hold no other lock; this method
// acquires d.mu.
func (d *AbuseDetector) touchTimeWindow(m map[string]*timeWindowState, key string, now time.Time) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	state, ok := m[key]
	if !ok {
		state = &timeWindowState{}
		m[key] = state
	}
	cutoff := now.Add(-abuseWindow)
	kept := state.hits[:0]
	for _, t := range state.hits {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	state.hits = append(kept, now)
	return len(state.hits)
}

// touchIPWindow updates the per-key distinct-IP set and returns the
// post-prune count.
func (d *AbuseDetector) touchIPWindow(keyPrefix, ip string, now time.Time) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	state, ok := d.keyLeakKeys[keyPrefix]
	if !ok {
		state = &ipWindowState{ips: make(map[string]time.Time)}
		d.keyLeakKeys[keyPrefix] = state
	}
	cutoff := now.Add(-abuseWindow)
	for k, t := range state.ips {
		if !t.After(cutoff) {
			delete(state.ips, k)
		}
	}
	state.ips[ip] = now
	return len(state.ips)
}

// checkAndSetCooldown returns true if the cooldown for this key has
// expired (or never existed) and stamps now into the map. Returns
// false if the key is still in cooldown.
func (d *AbuseDetector) checkAndSetCooldown(key string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if last, ok := d.signalCooldown[key]; ok {
		if now.Sub(last) < abuseSignalCooldown {
			return false
		}
	}
	d.signalCooldown[key] = now
	return true
}

// fire is the shared sink for all detector hits. Persists the signal,
// logs at warn, and dispatches the Discord/Slack alert.
func (d *AbuseDetector) fire(ctx context.Context, sig store.AbuseSignal) {
	sig.SignalID = fmt.Sprintf("abuse_%d_%s", sig.DetectedAt.UnixNano(), sig.Kind)

	if d.store != nil {
		if err := d.store.CreateAbuseSignal(ctx, &sig); err != nil && d.logger != nil {
			d.logger.Warn("abuse: persist failed",
				"error", err.Error(),
				"kind", sig.Kind,
				"project_id", sig.ProjectID,
			)
		}
	}

	if d.logger != nil {
		d.logger.Warn("abuse signal",
			"signal_id", sig.SignalID,
			"kind", sig.Kind,
			"severity", sig.Severity,
			"project_id", sig.ProjectID,
		)
	}

	dispatchAbuseWebhook(sig, d.logger)
}

// dispatchAbuseWebhook posts the alert to MESEDI_ALERT_WEBHOOK_URL on
// a goroutine. Same wire shape as the 5xx alert but in amber instead
// of red, with the abuse-detector footer.
func dispatchAbuseWebhook(sig store.AbuseSignal, logger *slog.Logger) {
	urlVal := AlertWebhookURL.Load()
	if urlVal == nil {
		return
	}
	url, _ := urlVal.(string)
	if url == "" {
		return
	}

	go func() {
		body, contentType := abuseWebhookPayload(url, sig)
		req, err := http.NewRequestWithContext(
			context.Background(), "POST", url, bytes.NewReader(body),
		)
		if err != nil {
			if logger != nil {
				logger.Warn("abuse webhook: build request failed", "error", err.Error())
			}
			return
		}
		req.Header.Set("Content-Type", contentType)

		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			if logger != nil {
				logger.Warn("abuse webhook: post failed", "error", err.Error())
			}
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 && logger != nil {
			logger.Warn("abuse webhook: non-2xx response",
				"status", resp.StatusCode, "webhook_host", req.URL.Host)
		}
	}()
}

func abuseWebhookPayload(url string, sig store.AbuseSignal) ([]byte, string) {
	title := fmt.Sprintf("Mesedi abuse signal: %s", sig.Kind)
	detail := fmt.Sprintf(
		"Project `%s` triggered `%s` (severity %s). Detail: %s",
		sig.ProjectID, sig.Kind, sig.Severity, sig.Detail,
	)

	if isSlackWebhook(url) {
		payload := map[string]any{
			"attachments": []map[string]any{{
				"color":  "#f59e0b",
				"title":  title,
				"text":   detail,
				"footer": "mesedi-api on Fly.io · abuse detector",
				"ts":     sig.DetectedAt.Unix(),
			}},
		}
		body, _ := json.Marshal(payload)
		return body, "application/json"
	}
	if isDiscordWebhook(url) {
		payload := map[string]any{
			"embeds": []map[string]any{{
				"title":       title,
				"description": detail,
				"color":       0xf59e0b,
				"footer":      map[string]string{"text": "mesedi-api on Fly.io · abuse detector"},
				"timestamp":   sig.DetectedAt.UTC().Format(time.RFC3339),
			}},
		}
		body, _ := json.Marshal(payload)
		return body, "application/json"
	}
	body, _ := json.Marshal(sig)
	return body, "application/json"
}

// ---------------------------------------------------------------------
// Background worker: progresses signals through notification +
// auto-suspension on the 24h schedule the ToS commits to.
// ---------------------------------------------------------------------

// StartAbuseWorker launches the background loop on its own goroutine.
// Stops cleanly when ctx is cancelled (caller passes a cancellable
// context from main's signal handler so SIGTERM drains the worker).
func StartAbuseWorker(ctx context.Context, s store.Store, mailer mail.Mailer, logger *slog.Logger, dashboardURL string) {
	go func() {
		ticker := time.NewTicker(workerScanCadence)
		defer ticker.Stop()

		// Run one scan immediately on startup so a backend restart
		// doesn't delay overdue notifications.
		abuseWorkerTick(ctx, s, mailer, logger, dashboardURL)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				abuseWorkerTick(ctx, s, mailer, logger, dashboardURL)
			}
		}
	}()
}

func abuseWorkerTick(ctx context.Context, s store.Store, mailer mail.Mailer, logger *slog.Logger, dashboardURL string) {
	if s == nil {
		return
	}
	signals, err := s.ListAbuseSignals(ctx, true, 500)
	if err != nil {
		if logger != nil {
			logger.Warn("abuse worker: list failed", "error", err.Error())
		}
		return
	}
	now := time.Now()
	for _, sig := range signals {
		// Already suspended? Nothing more for the worker to do until
		// a human resolves.
		if sig.SuspendedAt != nil {
			continue
		}
		// Not yet notified and 24h has passed? Send notification.
		if sig.NotifiedAt == nil {
			if now.Sub(sig.DetectedAt) < worker24h {
				continue
			}
			project, perr := s.GetProject(ctx, sig.ProjectID)
			if perr != nil {
				if logger != nil {
					logger.Warn("abuse worker: project lookup failed",
						"error", perr.Error(),
						"project_id", sig.ProjectID,
						"signal_id", sig.SignalID,
					)
				}
				continue
			}
			if project.OwnerEmail == "" {
				// No way to notify; mark notified anyway so we don't
				// loop, and let suspension proceed on the next pass.
				_ = s.MarkAbuseSignalNotified(ctx, sig.SignalID, now)
				continue
			}
			if err := mailer.SendSuspensionWarning(ctx, mail.SuspensionWarningInput{
				ToEmail:      project.OwnerEmail,
				ProjectName:  project.Name,
				SignalKind:   sig.Kind,
				DetectedAt:   sig.DetectedAt,
				DashboardURL: dashboardURL,
			}); err != nil && logger != nil {
				logger.Warn("abuse worker: notification email failed",
					"error", err.Error(),
					"signal_id", sig.SignalID,
				)
				// Don't mark notified; we'll retry next tick.
				continue
			}
			if err := s.MarkAbuseSignalNotified(ctx, sig.SignalID, now); err != nil && logger != nil {
				logger.Warn("abuse worker: mark notified failed",
					"error", err.Error(),
					"signal_id", sig.SignalID,
				)
			}
			continue
		}
		// Notified more than 24h ago and still not resolved? Suspend.
		if now.Sub(*sig.NotifiedAt) >= worker24h {
			reason := "abuse:" + sig.Kind
			if err := s.MarkAbuseSignalSuspended(ctx, sig.SignalID, sig.ProjectID, reason, now); err != nil && logger != nil {
				logger.Warn("abuse worker: suspend failed",
					"error", err.Error(),
					"signal_id", sig.SignalID,
				)
				continue
			}
			if logger != nil {
				logger.Warn("abuse worker: project auto-suspended",
					"project_id", sig.ProjectID,
					"signal_id", sig.SignalID,
					"reason", reason,
				)
			}
		}
	}
}
