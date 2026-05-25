// Automated abuse and issue detection (#172).
//
// The ToS commits to "Suspension or termination for cause" with six
// explicit triggers and a 24h notification window. This file is the
// first slice of the detection plumbing that backs that commitment.
//
// v0 scope (this file):
//   - One detector: sustained rate-limit abuse. A project that hits
//     429 thirty times within a rolling sixty-minute window is flagged
//     as showing abuse signal. The signal posts to the Discord alert
//     webhook configured via MESEDI_ALERT_WEBHOOK_URL, same channel
//     as 5xx alerts so the operator has one place to look.
//   - Per-project cooldown so a sustained outage from one client does
//     not spam the alert channel; once flagged, the same project
//     cannot re-fire for another sixty minutes.
//   - In-memory state. Consistent with the rate-limit middleware
//     posture: state resets on backend restart, which is acceptable
//     for v0 abuse signals (the worst case is a slightly delayed
//     re-fire after a restart).
//
// Future slices (NOT in this file yet):
//   - Persistent audit table (abuse_signals) for forensics + ToS
//     compliance evidence.
//   - Admin dashboard page that lists open signals and resolved ones.
//   - Additional detectors: key-leak heuristic (same key from >N
//     distinct IPs in a short window), oversized-payload spikes,
//     suspicious signup patterns (disposable domains, identical
//     project names from the same IP).
//   - Auto-suspension when an open signal sits unresolved past 24h,
//     with the 24h notification the ToS promises.

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
)

// Abuse-detection thresholds. Tuned generously: a real
// runaway-loop client comfortably blows past these in a few
// minutes; a well-behaved client never approaches them.
const (
	// rateLimitAbuseWindow is the rolling window we count 429s over.
	rateLimitAbuseWindow = 60 * time.Minute
	// rateLimitAbuseThreshold is the 429 count within the window that
	// triggers an abuse signal. A well-behaved SDK never hits ANY
	// 429 in normal operation, so 30 is a soft signal that something
	// is wrong on the client side and worth a human glance.
	rateLimitAbuseThreshold = 30
	// abuseSignalCooldown is the minimum gap between consecutive
	// signals for the same project. Prevents one stuck client from
	// generating a continuous stream of alerts.
	abuseSignalCooldown = 60 * time.Minute
)

// AbuseDetector tracks per-project signal state in memory. Concurrent
// access is safe via the internal mutex; the inner per-project entry
// is mutated only while holding the outer lock so no entry-level
// locking is needed.
type AbuseDetector struct {
	mu       sync.Mutex
	projects map[string]*abuseProjectState
	logger   *slog.Logger
}

type abuseProjectState struct {
	rateLimitHits []time.Time // timestamps of 429s within the rolling window
	lastSignalAt  time.Time   // most recent signal fire (for cooldown)
}

// NewAbuseDetector is the constructor.
func NewAbuseDetector(logger *slog.Logger) *AbuseDetector {
	return &AbuseDetector{
		projects: make(map[string]*abuseProjectState),
		logger:   logger,
	}
}

// RecordRateLimit is called once per 429 response. Updates the rolling
// window, evaluates the threshold, and fires an abuse signal if the
// project has crossed the line and is not in cooldown.
func (d *AbuseDetector) RecordRateLimit(projectID, method, path string) {
	if projectID == "" {
		return
	}
	now := time.Now()

	d.mu.Lock()
	state, ok := d.projects[projectID]
	if !ok {
		state = &abuseProjectState{}
		d.projects[projectID] = state
	}
	// Prune entries older than the rolling window.
	cutoff := now.Add(-rateLimitAbuseWindow)
	kept := state.rateLimitHits[:0]
	for _, t := range state.rateLimitHits {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	state.rateLimitHits = append(kept, now)
	count := len(state.rateLimitHits)
	inCooldown := now.Sub(state.lastSignalAt) < abuseSignalCooldown
	shouldFire := count >= rateLimitAbuseThreshold && !inCooldown
	if shouldFire {
		state.lastSignalAt = now
	}
	d.mu.Unlock()

	if !shouldFire {
		return
	}

	d.fireSignal(AbuseSignal{
		Kind:       "rate_limit_sustained",
		Severity:   "warning",
		ProjectID:  projectID,
		Method:     method,
		Path:       path,
		Count:      count,
		WindowMins: int(rateLimitAbuseWindow.Minutes()),
		Detected:   now,
	})
}

// AbuseSignal is the payload for a single abuse detection event. The
// shape doubles as the alert-webhook payload and the row schema for
// the future abuse_signals audit table.
type AbuseSignal struct {
	Kind       string    `json:"kind"`
	Severity   string    `json:"severity"`
	ProjectID  string    `json:"project_id"`
	Method     string    `json:"method,omitempty"`
	Path       string    `json:"path,omitempty"`
	Count      int       `json:"count,omitempty"`
	WindowMins int       `json:"window_mins,omitempty"`
	Detected   time.Time `json:"detected_at"`
}

// fireSignal logs the abuse signal at warn-level and posts a Discord/
// Slack/generic webhook alert if MESEDI_ALERT_WEBHOOK_URL is configured.
// Runs the webhook POST on a goroutine so a slow webhook never blocks
// the request hot path that triggered detection.
func (d *AbuseDetector) fireSignal(sig AbuseSignal) {
	if d.logger != nil {
		d.logger.Warn("abuse signal",
			"kind", sig.Kind,
			"severity", sig.Severity,
			"project_id", sig.ProjectID,
			"count", sig.Count,
			"window_mins", sig.WindowMins,
			"method", sig.Method,
			"path", sig.Path,
		)
	}

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
			if d.logger != nil {
				d.logger.Warn("abuse webhook: build request failed", "error", err.Error())
			}
			return
		}
		req.Header.Set("Content-Type", contentType)

		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			if d.logger != nil {
				d.logger.Warn("abuse webhook: post failed", "error", err.Error())
			}
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 && d.logger != nil {
			d.logger.Warn("abuse webhook: non-2xx response",
				"status", resp.StatusCode, "webhook_host", req.URL.Host)
		}
	}()
}

// abuseWebhookPayload mirrors alertWebhookPayload (5xx alerts) but
// describes an abuse-signal event. Auto-detects Slack and Discord
// URL shapes; falls back to generic JSON.
func abuseWebhookPayload(url string, sig AbuseSignal) ([]byte, string) {
	title := fmt.Sprintf("Mesedi abuse signal: %s", sig.Kind)
	detail := fmt.Sprintf(
		"Project `%s` triggered `%s`: %d events in %dm window. Last: `%s %s`",
		sig.ProjectID, sig.Kind, sig.Count, sig.WindowMins, sig.Method, sig.Path,
	)

	if isSlackWebhook(url) {
		payload := map[string]any{
			"attachments": []map[string]any{{
				"color":  "#f59e0b",
				"title":  title,
				"text":   detail,
				"footer": "mesedi-api on Fly.io · abuse detector",
				"ts":     sig.Detected.Unix(),
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
				"timestamp":   sig.Detected.UTC().Format(time.RFC3339),
			}},
		}
		body, _ := json.Marshal(payload)
		return body, "application/json"
	}

	body, _ := json.Marshal(sig)
	return body, "application/json"
}
