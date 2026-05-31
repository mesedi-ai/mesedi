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
	SendBudgetCeilingBreach(ctx context.Context, in BudgetCeilingBreachInput) error
	SendOrgInvite(ctx context.Context, in OrgInviteInput) error
}

// OrgInviteInput is everything the team-invite email template needs
// (#263). Sent when an admin clicks 'Invite' on /app/team. AcceptURL
// carries the invite token and lands the invitee on the public
// accept page.
type OrgInviteInput struct {
	ToEmail      string
	OrgName      string
	InviterEmail string
	Role         string
	AcceptURL    string
	ExpiresAt    time.Time
}

// BudgetCeilingBreachInput is everything the tenant-budget-ceiling
// breach email needs (#252). Sent the first time a tenant's
// month-to-date burn crosses its configured ceiling. One send per
// breach per calendar month.
type BudgetCeilingBreachInput struct {
	ToEmail        string
	BurnUSD        float64
	CeilingUSD     float64
	BreachAction   string // "warn" | "halt"
	ProjectCount   int
	DashboardURL   string
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

func (m NoopMailer) SendBudgetCeilingBreach(ctx context.Context, in BudgetCeilingBreachInput) error {
	if m.Logger != nil {
		m.Logger.Debug("mail: budget ceiling breach (noop, no RESEND_API_KEY)",
			"to", in.ToEmail,
			"burn_usd", in.BurnUSD,
			"ceiling_usd", in.CeilingUSD,
		)
	}
	return nil
}

func (m NoopMailer) SendOrgInvite(ctx context.Context, in OrgInviteInput) error {
	if m.Logger != nil {
		m.Logger.Debug("mail: org invite (noop, no RESEND_API_KEY)",
			"to", in.ToEmail,
			"org", in.OrgName,
			"role", in.Role,
			"inviter", in.InviterEmail,
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

// SendBudgetCeilingBreach renders and ships the tenant budget-ceiling
// breach email (#252). Sent the first time a tenant's MTD burn
// crosses the configured ceiling.
func (m *ResendMailer) SendBudgetCeilingBreach(ctx context.Context, in BudgetCeilingBreachInput) error {
	subject := fmt.Sprintf("Mesedi budget ceiling breached: $%.2f / $%.2f",
		in.BurnUSD, in.CeilingUSD)

	textBody := fmt.Sprintf(
		"Your Mesedi tenant just crossed its monthly budget ceiling.\n\n"+
			"Month-to-date burn: $%.2f\n"+
			"Configured ceiling: $%.2f\n"+
			"Action taken:       %s\n"+
			"Projects in tenant: %d\n\n"+
			"View the rollup: %s/app/org\n",
		in.BurnUSD, in.CeilingUSD, in.BreachAction, in.ProjectCount,
		in.DashboardURL,
	)
	htmlBody := fmt.Sprintf(
		"<p>Your Mesedi tenant just crossed its monthly budget ceiling.</p>"+
			"<ul>"+
			"<li><strong>Month-to-date burn:</strong> $%.2f</li>"+
			"<li><strong>Configured ceiling:</strong> $%.2f</li>"+
			"<li><strong>Action taken:</strong> %s</li>"+
			"<li><strong>Projects in tenant:</strong> %d</li>"+
			"</ul>"+
			"<p><a href=\"%s/app/org\">View the rollup</a></p>",
		in.BurnUSD, in.CeilingUSD, in.BreachAction, in.ProjectCount,
		in.DashboardURL,
	)

	body, err := json.Marshal(resendRequest{
		From:    m.From,
		To:      []string{in.ToEmail},
		Subject: subject,
		HTML:    htmlBody,
		Text:    textBody,
	})
	if err != nil {
		return fmt.Errorf("mail: marshal budget ceiling breach: %w", err)
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
		m.Logger.Info("mail: budget ceiling breach sent",
			"to", in.ToEmail,
			"burn_usd", in.BurnUSD,
			"ceiling_usd", in.CeilingUSD,
		)
	}
	return nil
}

// SendOrgInvite renders and ships the team-invite email (#263). The
// invitee receives a link to the accept page carrying the token.
func (m *ResendMailer) SendOrgInvite(ctx context.Context, in OrgInviteInput) error {
	subject := fmt.Sprintf("%s invited you to %s on Mesedi",
		in.InviterEmail, in.OrgName)

	textBody := fmt.Sprintf(
		"%s has invited you to join the %s organization on Mesedi as a %s.\n\n"+
			"Accept the invite: %s\n\n"+
			"This invite expires on %s. If you weren't expecting this, you can ignore the message.\n",
		in.InviterEmail, in.OrgName, in.Role,
		in.AcceptURL,
		in.ExpiresAt.Format("Jan 2, 2006"),
	)
	htmlBody := fmt.Sprintf(
		"<p><strong>%s</strong> has invited you to join the <strong>%s</strong> "+
			"organization on Mesedi as a <strong>%s</strong>.</p>"+
			"<p><a href=\"%s\">Accept the invite</a></p>"+
			"<p>This invite expires on %s. If you weren't expecting this, you can ignore the message.</p>",
		in.InviterEmail, in.OrgName, in.Role,
		in.AcceptURL,
		in.ExpiresAt.Format("Jan 2, 2006"),
	)

	body, err := json.Marshal(resendRequest{
		From:    m.From,
		To:      []string{in.ToEmail},
		Subject: subject,
		HTML:    htmlBody,
		Text:    textBody,
	})
	if err != nil {
		return fmt.Errorf("mail: marshal org invite: %w", err)
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
		m.Logger.Info("mail: org invite sent",
			"to", in.ToEmail,
			"org", in.OrgName,
			"role", in.Role,
		)
	}
	return nil
}
