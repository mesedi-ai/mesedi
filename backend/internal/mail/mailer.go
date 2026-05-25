// Package mail sends transactional email via Resend.
//
// The Mailer interface decouples the signup handler from the
// concrete provider; tests and local-dev runs without RESEND_API_KEY
// configured use NoopMailer, which silently swallows every send.
//
// Welcome email (#127) is the only template that ships in this
// slice. Day-1 and day-3 nudges land later once a scheduled-job
// mechanism is in place.
package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Mailer is the narrow interface signup and other handlers depend on.
// Implementations: ResendMailer (production) and NoopMailer (dev/test).
type Mailer interface {
	SendWelcome(ctx context.Context, in WelcomeInput) error
	SendSuspensionWarning(ctx context.Context, in SuspensionWarningInput) error
}

// SuspensionWarningInput is everything the suspension-warning
// template needs. Sent 24h after an abuse signal is detected; the
// recipient has 24h more before auto-suspension fires.
type SuspensionWarningInput struct {
	ToEmail      string
	ProjectName  string
	SignalKind   string
	DetectedAt   time.Time
	DashboardURL string
}

// WelcomeInput is everything the welcome template needs.
type WelcomeInput struct {
	ToEmail      string // recipient
	ProjectName  string // human-readable project name from signup
	APIKeyPrefix string // e.g. "mesedi_sk_abc123..." prefix only
	DashboardURL string // e.g. https://mesedi.vercel.app/app
	DocsURL      string // e.g. https://mesedi.vercel.app/docs/quickstart
}

// NoopMailer accepts every send and discards it. Used when no
// RESEND_API_KEY is configured. Logs at debug so dev runs aren't
// silent.
type NoopMailer struct {
	Logger *slog.Logger
}

func (m NoopMailer) SendWelcome(ctx context.Context, in WelcomeInput) error {
	if m.Logger != nil {
		m.Logger.Debug("mail: welcome (noop, no RESEND_API_KEY)",
			"to", in.ToEmail,
			"project", in.ProjectName,
		)
	}
	return nil
}

func (m NoopMailer) SendSuspensionWarning(ctx context.Context, in SuspensionWarningInput) error {
	if m.Logger != nil {
		m.Logger.Debug("mail: suspension warning (noop, no RESEND_API_KEY)",
			"to", in.ToEmail,
			"project", in.ProjectName,
			"signal_kind", in.SignalKind,
		)
	}
	return nil
}

// ResendMailer posts transactional sends to Resend's HTTP API. No
// SDK dependency: Resend's surface is small enough that a single
// JSON POST suffices.
type ResendMailer struct {
	APIKey     string
	From       string // e.g. "Mesedi <hello@mesedi.ai>"
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// NewResendMailer is the constructor. Pass an empty APIKey to get
// an explicit panic at startup instead of mysterious 401s later.
func NewResendMailer(apiKey, from string, logger *slog.Logger) *ResendMailer {
	if apiKey == "" {
		panic("mail: NewResendMailer called with empty apiKey")
	}
	return &ResendMailer{
		APIKey: apiKey,
		From:   from,
		HTTPClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		Logger: logger,
	}
}

type resendRequest struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	HTML    string   `json:"html"`
	Text    string   `json:"text"`
}

type resendResponse struct {
	ID string `json:"id"`
}

// SendWelcome renders and ships the welcome email.
func (m *ResendMailer) SendWelcome(ctx context.Context, in WelcomeInput) error {
	subject := "Welcome to Mesedi"
	html := welcomeHTML(in)
	text := welcomeText(in)

	body, err := json.Marshal(resendRequest{
		From:    m.From,
		To:      []string{in.ToEmail},
		Subject: subject,
		HTML:    html,
		Text:    text,
	})
	if err != nil {
		return fmt.Errorf("mail: marshal welcome: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://api.resend.com/emails", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("mail: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+m.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("mail: post to resend: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("mail: resend returned %d: %s", resp.StatusCode, string(respBody))
	}

	var parsed resendResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		// Send succeeded but we couldn't parse the id, not a fatal
		// error from the caller's perspective.
		if m.Logger != nil {
			m.Logger.Warn("mail: welcome sent but resend response parse failed",
				"error", err.Error())
		}
		return nil
	}

	if m.Logger != nil {
		m.Logger.Info("mail: welcome sent",
			"to", in.ToEmail,
			"resend_id", parsed.ID,
		)
	}
	return nil
}

// SendSuspensionWarning renders and ships the abuse-signal warning
// email. Body explains the signal that fired, the 24h grace window
// before auto-suspension, and how to reach support to dispute.
func (m *ResendMailer) SendSuspensionWarning(ctx context.Context, in SuspensionWarningInput) error {
	subject := "Mesedi: action required on your project"

	body, err := json.Marshal(resendRequest{
		From:    m.From,
		To:      []string{in.ToEmail},
		Subject: subject,
		HTML:    suspensionWarningHTML(in),
		Text:    suspensionWarningText(in),
	})
	if err != nil {
		return fmt.Errorf("mail: marshal suspension warning: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://api.resend.com/emails", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("mail: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+m.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("mail: post to resend: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("mail: resend returned %d: %s", resp.StatusCode, string(respBody))
	}

	if m.Logger != nil {
		m.Logger.Info("mail: suspension warning sent",
			"to", in.ToEmail,
			"signal_kind", in.SignalKind,
		)
	}
	return nil
}
