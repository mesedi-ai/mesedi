package api

// Operator notices — non-error events worth interrupting the founder
// for, sent to the same MESEDI_ALERT_WEBHOOK_URL that carries 5xx
// alerts.
//
// WHY THIS IS SEPARATE FROM fireAlertWebhook:
// That function is shaped around an HTTP failure — it takes a status
// code, a method, a path and a duration, and renders everything in
// alarm red. A signup is not a failure and should not look like one;
// an operator who learns to dismiss red boxes will eventually dismiss
// a real incident. Same transport, same fail-open discipline,
// different colour and different vocabulary.
//
// Deliberately reuses the existing alert webhook rather than adding a
// second secret. One channel the founder actually watches beats two
// they configure once and forget — and the volume here is signups,
// not requests.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// operatorNoticeColor is a calm blue, deliberately not the 0xdc2626
// red that fireAlertWebhook uses for 5xx. The colour is the whole
// point: an operator should be able to tell "someone signed up" from
// "production is broken" without reading a word.
const operatorNoticeColor = 0x3b82f6

// fireOperatorNotice posts an informational message to the operator
// webhook. Runs on a goroutine and swallows every error: a webhook
// outage must never affect the request that triggered it, and the
// caller has already succeeded by the time this runs.
//
// fields render as label/value rows on Slack and Discord and as a
// flat object on any other receiver. Order is not guaranteed — Go map
// iteration is random — so do not encode meaning in position.
func fireOperatorNotice(
	title, detail string,
	fields map[string]string,
	logger *slog.Logger,
) {
	urlVal := AlertWebhookURL.Load()
	if urlVal == nil {
		return
	}
	url, _ := urlVal.(string)
	if url == "" {
		return
	}

	go func() {
		body, contentType := operatorNoticePayload(url, title, detail, fields)
		// context.Background(), NOT the request context — deliberate.
		// The signup response has already been written by the time
		// this runs, so the request context is either cancelled or
		// about to be; inheriting it would cancel the notice before
		// it left the machine. The 10s client timeout below is what
		// actually bounds this call.
		//
		// This also means the send does not survive a restart: if the
		// machine dies mid-flight the notice is lost. That is the
		// right trade — the signup itself is already committed to
		// Postgres, and a missed "someone signed up" ping is an
		// inconvenience, not data loss. Persisting and retrying
		// notices would mean a queue, a table and a worker for
		// something whose entire value is being immediate.
		req, err := http.NewRequestWithContext(
			context.Background(), "POST", url, bytes.NewReader(body),
		)
		if err != nil {
			logger.Warn("operator notice: build request failed", "error", err.Error())
			return
		}
		req.Header.Set("Content-Type", contentType)

		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			logger.Warn("operator notice: post failed", "error", err.Error())
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			logger.Warn("operator notice: non-2xx response",
				"status", resp.StatusCode, "webhook_host", req.URL.Host)
		}
	}()
}

// operatorNoticePayload mirrors alertWebhookPayload's shape detection
// so both notice types render natively on whichever webhook the
// operator configured.
//
// map[string]any is used throughout rather than typed structs, and
// that is deliberate: Slack attachments and Discord embeds are
// unrelated third-party wire formats with heterogeneous, partly
// optional fields, and we build each one exactly once. Defining Go
// structs for both would add two type hierarchies that exist only to
// be marshalled immediately and would drift the moment either vendor
// changes a field. Same call made in alertWebhookPayload above; the
// shapes are asserted by tests rather than by the type system.
func operatorNoticePayload(
	url, title, detail string, fields map[string]string,
) ([]byte, string) {
	if isSlackWebhook(url) {
		sf := make([]map[string]any, 0, len(fields))
		for k, v := range fields {
			sf = append(sf, map[string]any{
				"title": k,
				"value": v,
				"short": true,
			})
		}
		payload := map[string]any{
			"attachments": []map[string]any{{
				"color":  "#3b82f6",
				"title":  title,
				"text":   detail,
				"fields": sf,
				"footer": "mesedi-api on Fly.io",
				"ts":     time.Now().Unix(),
			}},
		}
		body, _ := json.Marshal(payload)
		return body, "application/json"
	}

	if isDiscordWebhook(url) {
		df := make([]map[string]any, 0, len(fields))
		for k, v := range fields {
			df = append(df, map[string]any{
				"name":   k,
				"value":  v,
				"inline": true,
			})
		}
		embed := map[string]any{
			"title":       title,
			"description": detail,
			"color":       operatorNoticeColor,
			"footer":      map[string]string{"text": "mesedi-api on Fly.io"},
			"timestamp":   time.Now().UTC().Format(time.RFC3339),
		}
		if len(df) > 0 {
			embed["fields"] = df
		}
		payload := map[string]any{"embeds": []map[string]any{embed}}
		body, _ := json.Marshal(payload)
		return body, "application/json"
	}

	payload := map[string]any{
		"service":   "mesedi-api",
		"kind":      "operator_notice",
		"title":     title,
		"detail":    detail,
		"fields":    fields,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}
	body, _ := json.Marshal(payload)
	return body, "application/json"
}

// maskEmailForNotice reduces an address to a recognisable but
// non-complete form: rob@example.com -> r••@example.com.
//
// The operator channel is a Discord server, and a webhook URL is a
// bearer credential that has already been pasted into a chat
// transcript once today. Putting customers' full email addresses on
// the other end of that is a needless standing exposure — the domain
// is what tells you whether a signup is a real company or a throwaway,
// and the full address is one click away in the admin dashboard when
// it actually matters.
func maskEmailForNotice(email string) string {
	at := -1
	for i, r := range email {
		if r == '@' {
			at = i
			break
		}
	}
	if at <= 0 {
		return "(hidden)"
	}
	local, domain := email[:at], email[at:]
	if len(local) <= 1 {
		return local + "••" + domain
	}
	return fmt.Sprintf("%c••%s", local[0], domain)
}
