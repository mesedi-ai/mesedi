// Package webhooks implements the failure-class escalation dispatcher
// for Mesedi's task #83. The flow is:
//
//  1. A trigger (slice 2: manual test endpoint; slice 3: failure_group
//     creation) produces a Payload describing what happened.
//  2. The dispatcher serializes the payload to JSON, computes an
//     HMAC-SHA256 signature of the body using the webhook's secret,
//     and POSTs the body to the configured URL with the signature in
//     an X-Mesedi-Signature header.
//  3. Each attempt is recorded as a webhook_deliveries row via the
//     Store. Transient failures retry with exponential backoff up to
//     MaxAttempts; permanent failures (non-2xx after retries, or a
//     URL parse error) record a final "failed" row and stop.
//
// The dispatcher is intentionally synchronous from the caller's
// perspective for slice 2, the test endpoint blocks while the
// delivery is attempted so the operator sees the result. Slice 3
// will add a non-blocking `Dispatch` variant that starts a goroutine
// for the auto-fire path.
package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"mesedi/backend/internal/store"
)

// Default retry/backoff policy. Three attempts total: initial + 2
// retries. Backoff is 1s and then 4s, modest enough to absorb a
// receiver that's briefly overloaded, short enough that the operator
// sees a result quickly during local testing.
const (
	MaxAttempts          = 3
	InitialBackoff       = 1 * time.Second
	BackoffMultiplier    = 4
	PerAttemptTimeout    = 10 * time.Second
	MaxResponseBodyBytes = 2048
	SignatureHeader      = "X-Mesedi-Signature"
	EventIDHeader        = "X-Mesedi-Event-Id"
	UserAgent            = "Mesedi-Webhook/1"
)

// Payload is the JSON structure POSTed to the customer's receiver.
// Versioned via the Version field so future schema changes don't
// silently break customer-side parsers, they can switch on it.
//
// Test deliveries (slice 2) set Test=true and use synthetic values
// for FailureClass/Signature/SampleExecutionID; real deliveries
// (slice 3) set Test=false and populate from the live failure_group.
type Payload struct {
	Version           string    `json:"version"`
	Event             string    `json:"event"`
	Test              bool      `json:"test,omitempty"`
	ProjectID         string    `json:"project_id"`
	WebhookID         string    `json:"webhook_id"`
	GroupID           string    `json:"group_id,omitempty"`
	FailureClass      string    `json:"failure_class"`
	Signature         string    `json:"signature"`
	SampleExecutionID string    `json:"sample_execution_id,omitempty"`
	DashboardURL      string    `json:"dashboard_url,omitempty"`
	PlaybookURL       string    `json:"playbook_url,omitempty"`
	DeliveryID        string    `json:"delivery_id"`
	Timestamp         time.Time `json:"timestamp"`
}

// BuildTestPayload returns a synthetic Payload an operator can use to
// verify their receiver is reachable and signature-verifying
// correctly. The signature is a fixed string ("test_signature") and
// the dashboard URL points at the React dashboard root; the test
// payload omits the SampleExecutionID so adapters don't render dead
// links into the receiving channel (exec-test isn't a real row).
//
// dashboardBaseURL should be the dashboard origin without a path
// (e.g. https://mesedi.vercel.app), no trailing slash. Adapters
// append their own routes.
func BuildTestPayload(webhook *store.ProjectWebhook, dashboardBaseURL, deliveryID string) Payload {
	return Payload{
		Version:      "1",
		Event:        "failure_group.test",
		Test:         true,
		ProjectID:    webhook.ProjectID,
		WebhookID:    webhook.WebhookID,
		FailureClass: "crashes",
		Signature:    "test_signature",
		// SampleExecutionID intentionally empty for test deliveries , 
		// avoids putting "exec-test" (a 404 link) into Discord/Slack.
		DashboardURL: dashboardBaseURL,
		PlaybookURL:  "",
		DeliveryID:   deliveryID,
		Timestamp:    time.Now().UTC(),
	}
}

// Sign returns the hex-encoded HMAC-SHA256 of body using secret.
// The receiver MUST recompute this with the same key and compare in
// constant time before trusting the payload. Mesedi will never deliver
// a payload without this header.
func Sign(body, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// DeliveryResult captures the outcome of one delivery (across all
// retry attempts). Callers can use this to log to the
// webhook_deliveries table.
type DeliveryResult struct {
	Status       string // "delivered" | "failed"
	Attempts     int
	HTTPStatus   *int   // last attempt's HTTP status, if any
	Error        string // last attempt's transport-layer error, if any
	ResponseBody string // last attempt's response body (truncated)
	DurationMs   int64  // total duration across all attempts
}

// Deliver POSTs a signed JSON payload to the webhook URL with retry
// and backoff. Returns a DeliveryResult describing the outcome of the
// final attempt, plus the per-attempt records the caller can persist
// to the webhook_deliveries log.
//
// This is synchronous. Slice 2's test endpoint blocks on it; slice 3's
// auto-fire path will wrap it in a goroutine.
func Deliver(
	ctx context.Context,
	logger *slog.Logger,
	httpClient *http.Client,
	webhook *store.ProjectWebhook,
	payload Payload,
) (DeliveryResult, []store.WebhookDelivery) {
	body, err := json.Marshal(payload)
	if err != nil {
		// Marshal failure is unrecoverable and should never happen for
		// our well-typed Payload. Bail with one failed-attempt record
		// so the operator sees the bug.
		return DeliveryResult{
				Status:   "failed",
				Attempts: 1,
				Error:    "marshal payload: " + err.Error(),
			}, []store.WebhookDelivery{{
				WebhookID: webhook.WebhookID,
				ProjectID: webhook.ProjectID,
				Attempt:   1,
				Status:    "failed",
				Error:     "marshal payload: " + err.Error(),
			}}
	}

	// Receiver-specific payload reshape. Discord (and future chat
	// targets) need a body shape Mesedi's generic Payload doesn't
	// match. When an adapter applies, the HMAC signature is recomputed
	// over the adapted body so the on-wire signature header is correct
	// for whatever leaves the dispatcher.
	if adapted, ok, adaptErr := adaptedBody(webhook.URL, payload); ok {
		if adaptErr != nil {
			return DeliveryResult{
					Status:   "failed",
					Attempts: 1,
					Error:    "marshal adapted payload: " + adaptErr.Error(),
				}, []store.WebhookDelivery{{
					WebhookID: webhook.WebhookID,
					ProjectID: webhook.ProjectID,
					Attempt:   1,
					Status:    "failed",
					Error:     "marshal adapted payload: " + adaptErr.Error(),
				}}
		}
		body = adapted
	}

	signature := Sign(body, []byte(webhook.Secret))

	attempts := make([]store.WebhookDelivery, 0, MaxAttempts)
	backoff := InitialBackoff
	start := time.Now()

	var lastResult DeliveryResult
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		attemptStart := time.Now()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook.URL, bytes.NewReader(body))
		if err != nil {
			rec := store.WebhookDelivery{
				WebhookID:    webhook.WebhookID,
				ProjectID:    webhook.ProjectID,
				FailureClass: payload.FailureClass,
				Signature:    payload.Signature,
				GroupID:      payload.GroupID,
				Attempt:      attempt,
				Status:       "failed",
				Error:        "build request: " + err.Error(),
				DurationMs:   time.Since(attemptStart).Milliseconds(),
			}
			attempts = append(attempts, rec)
			lastResult = DeliveryResult{
				Status:   "failed",
				Attempts: attempt,
				Error:    rec.Error,
			}
			break // build error won't get fixed by retry
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", UserAgent)
		req.Header.Set(SignatureHeader, signature)
		req.Header.Set(EventIDHeader, payload.DeliveryID)

		resp, err := httpClient.Do(req)
		duration := time.Since(attemptStart).Milliseconds()

		if err != nil {
			// Transport error (DNS, connection refused, timeout). Retry.
			rec := store.WebhookDelivery{
				WebhookID:    webhook.WebhookID,
				ProjectID:    webhook.ProjectID,
				FailureClass: payload.FailureClass,
				Signature:    payload.Signature,
				GroupID:      payload.GroupID,
				Attempt:      attempt,
				Status:       "failed",
				Error:        err.Error(),
				DurationMs:   duration,
			}
			attempts = append(attempts, rec)
			lastResult = DeliveryResult{
				Status:     "failed",
				Attempts:   attempt,
				Error:      err.Error(),
				DurationMs: duration,
			}
			logger.Warn("webhook delivery transport error",
				"webhook_id", webhook.WebhookID,
				"attempt", attempt,
				"error", err.Error(),
			)
			if attempt < MaxAttempts {
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					break
				}
				backoff *= BackoffMultiplier
			}
			continue
		}

		// We have a response. Read body (truncated) and close.
		respBody := readLimited(resp.Body, MaxResponseBodyBytes)
		_ = resp.Body.Close()
		httpStatus := resp.StatusCode
		statusOK := httpStatus >= 200 && httpStatus < 300

		rec := store.WebhookDelivery{
			WebhookID:    webhook.WebhookID,
			ProjectID:    webhook.ProjectID,
			FailureClass: payload.FailureClass,
			Signature:    payload.Signature,
			GroupID:      payload.GroupID,
			Attempt:      attempt,
			HTTPStatus:   &httpStatus,
			ResponseBody: respBody,
			DurationMs:   duration,
		}
		if statusOK {
			rec.Status = "delivered"
		} else {
			rec.Status = "failed"
			rec.Error = fmt.Sprintf("non-2xx status: %d", httpStatus)
		}
		attempts = append(attempts, rec)
		lastResult = DeliveryResult{
			Status:       rec.Status,
			Attempts:     attempt,
			HTTPStatus:   &httpStatus,
			Error:        rec.Error,
			ResponseBody: respBody,
			DurationMs:   duration,
		}

		if statusOK {
			logger.Info("webhook delivered",
				"webhook_id", webhook.WebhookID,
				"attempt", attempt,
				"http_status", httpStatus,
				"duration_ms", duration,
			)
			break
		}

		logger.Warn("webhook delivery non-2xx",
			"webhook_id", webhook.WebhookID,
			"attempt", attempt,
			"http_status", httpStatus,
		)
		// 4xx (except 429) is a permanent error, retry won't fix
		// "Forbidden" or "Bad Request." Only retry on 5xx and 429.
		if httpStatus < 500 && httpStatus != http.StatusTooManyRequests {
			break
		}
		if attempt < MaxAttempts {
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				break
			}
			backoff *= BackoffMultiplier
		}
	}

	lastResult.DurationMs = time.Since(start).Milliseconds()
	return lastResult, attempts
}

// readLimited reads up to limit bytes from r, returns as a string,
// and silently ignores transport errors past the limit (we don't care
// about the rest of the body if the receiver's chatty).
func readLimited(r io.Reader, limit int) string {
	if limit <= 0 {
		return ""
	}
	buf := make([]byte, limit)
	n, err := io.ReadFull(r, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return string(buf[:n])
	}
	return string(buf[:n])
}

// DefaultHTTPClient returns the HTTP client the dispatcher uses by
// default, strict timeouts, no follow of redirects (a webhook
// receiver should be a single concrete URL, not a redirect chain).
func DefaultHTTPClient() *http.Client {
	return &http.Client{
		Timeout: PerAttemptTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
