// Package mail sends transactional email via Resend.
//
// The Mailer interface decouples the signup handler from the
// concrete provider; tests and local-dev runs without RESEND_API_KEY
// configured use NoopMailer, which silently swallows every send.
//
// Welcome email is the only template that ships in this
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

// formatThousands renders an integer with comma separators
// (10000 -> "10,000"). Used in email templates that need to render
// numbers in customer-readable form without depending on a heavy
// i18n library.
func formatThousands(n int) string {
	if n < 0 {
		return "-" + formatThousands(-n)
	}
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return formatThousands(n/1000) + "," + fmt.Sprintf("%03d", n%1000)
}

// Mailer is the narrow interface signup and other handlers depend on.
// Implementations: ResendMailer (production) and NoopMailer (dev/test).
type Mailer interface {
	SendWelcome(ctx context.Context, in WelcomeInput) error
	SendSuspensionWarning(ctx context.Context, in SuspensionWarningInput) error
	SendBudgetCeilingBreach(ctx context.Context, in BudgetCeilingBreachInput) error
	SendOrgInvite(ctx context.Context, in OrgInviteInput) error
	SendHobbyBillingNotification(ctx context.Context, in HobbyBillingNotificationInput) error
	// customer-initiated lifecycle confirmations. Both fire from
	// the danger-zone flow on /app/settings. SendDowngradeScheduled
	// confirms that Cloud Team will cancel at the current period end;
	// SendAccountClosed confirms the project has been hard-deleted.
	SendDowngradeScheduled(ctx context.Context, in DowngradeScheduledInput) error
	SendAccountClosed(ctx context.Context, in AccountClosedInput) error
	// SendMagicLink ships the one-time sign-in link (commit 2).
	// The body contains a single tappable URL and a short explanation
	// of the 15-minute expiry window; no marketing, no images.
	SendMagicLink(ctx context.Context, in MagicLinkInput) error
	Enabled() bool
}

// MagicLinkInput carries everything the magic-link template renders.
// SignInURL is the full https URL the recipient clicks; the dashboard
// server's /api/auth/magic-link/verify route handles the token. We
// pass the full URL rather than the raw token so the template stays
// composable -- the email layer never assembles the URL itself.
type MagicLinkInput struct {
	ToEmail   string
	SignInURL string
	ExpiresAt time.Time
}

// HobbyBillingNotificationKind enumerates the three email kinds the
// HobbyBillingScheduler emits.
type HobbyBillingNotificationKind string

const (
	HobbyBillingNotificationChargeFailed HobbyBillingNotificationKind = "charge_failed"
	HobbyBillingNotificationCardDetached HobbyBillingNotificationKind = "card_detached"
	HobbyBillingNotificationReceipt      HobbyBillingNotificationKind = "receipt"
)

// HobbyBillingNotificationInput is the typed payload the scheduler
// passes to SendHobbyBillingNotification. Kind discriminates which
// template the mailer renders.
type HobbyBillingNotificationInput struct {
	Kind            HobbyBillingNotificationKind
	ToEmail         string
	ProjectName     string
	DashboardURL    string
	AmountUSD       float64 // populated for charge_failed and receipt
	FailureCount    int     // populated for charge_failed
	FailureCeiling  int     // populated for charge_failed and card_detached
	PaymentIntentID string  // populated for receipt
	// IncludedExecutions is the Hobby free-quota size injected by the
	// scheduler from the api.HobbyExecutionLimit constant. Passed
	// via the input rather than imported here so the mail package
	// does not create a circular dependency with the api package.
	// Used in the card_detached template body. Defaults to 10000 if
	// the caller passes zero (defensive).
	IncludedExecutions int
}

// OrgInviteInput is everything the team-invite email template needs
//. Sent when an admin clicks 'Invite' on /app/team. AcceptURL
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
// breach email needs. Sent the first time a tenant's
// month-to-date burn crosses its configured ceiling. One send per
// breach per calendar month.
type BudgetCeilingBreachInput struct {
	ToEmail      string
	BurnUSD      float64
	CeilingUSD   float64
	BreachAction string // "warn" | "halt"
	ProjectCount int
	DashboardURL string
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

// DowngradeScheduledInput carries everything the downgrade-scheduled
// confirmation email template renders. Sent immediately when
// the customer clicks Downgrade in the danger zone; the cancellation
// itself fires at PeriodEnd.
type DowngradeScheduledInput struct {
	ToEmail      string
	ProjectName  string
	PeriodEnd    time.Time // when Cloud Team coverage ends
	DashboardURL string
	// ImmediateFlip is true on the corrupted-state code path where the
	// DB tier flipped to Hobby instantly because no Stripe
	// subscription existed. The template tells the customer the
	// downgrade is already effective in that case.
	ImmediateFlip bool
}

// AccountClosedInput carries everything the account-closed
// confirmation email template renders. Sent after
// DeleteProjectCascade succeeds; this is the last touch with the
// recipient because their dashboard credentials are gone.
type AccountClosedInput struct {
	ToEmail     string
	ProjectName string
	ClosedAt    time.Time
	// SupportEmail is the address customers can reply to if they
	// closed by mistake (we cannot undo the cascade but we can help
	// them re-create the project with their data).
	SupportEmail string
}

// WelcomeInput is everything the welcome template needs.
type WelcomeInput struct {
	ToEmail      string // recipient
	ProjectName  string // human-readable project name from signup
	APIKeyPrefix string // e.g. "mesedi_sk_abc123..." prefix only
	DashboardURL string // e.g. https://app.mesedi.ai
	DocsURL      string // e.g. https://app.mesedi.ai/docs/quickstart
	// VerifyURL is populated for raw-email signups — the
	// one-click link the recipient must click before the dashboard
	// unlocks. Empty string means "skip the verify block" (e.g. SSO
	// signups inherit a verified email from the IdP and don't need it).
	VerifyURL string
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

func (m NoopMailer) SendHobbyBillingNotification(ctx context.Context, in HobbyBillingNotificationInput) error {
	if m.Logger != nil {
		m.Logger.Debug("mail: hobby billing notification (noop, no RESEND_API_KEY)",
			"to", in.ToEmail,
			"kind", string(in.Kind),
			"project", in.ProjectName,
			"amount_usd", in.AmountUSD,
		)
	}
	return nil
}

func (m NoopMailer) SendDowngradeScheduled(ctx context.Context, in DowngradeScheduledInput) error {
	if m.Logger != nil {
		m.Logger.Debug("mail: downgrade scheduled (noop, no RESEND_API_KEY)",
			"to", in.ToEmail,
			"project", in.ProjectName,
			"period_end", in.PeriodEnd,
			"immediate_flip", in.ImmediateFlip,
		)
	}
	return nil
}

func (m NoopMailer) SendAccountClosed(ctx context.Context, in AccountClosedInput) error {
	if m.Logger != nil {
		m.Logger.Debug("mail: account closed (noop, no RESEND_API_KEY)",
			"to", in.ToEmail,
			"project", in.ProjectName,
			"closed_at", in.ClosedAt,
		)
	}
	return nil
}

func (m NoopMailer) SendMagicLink(ctx context.Context, in MagicLinkInput) error {
	if m.Logger != nil {
		// Log the URL at debug so dev runs can copy/paste it from the
		// log instead of needing a real Resend account.
		m.Logger.Debug("mail: magic-link (noop, no RESEND_API_KEY)",
			"to", in.ToEmail,
			"sign_in_url", in.SignInURL,
			"expires_at", in.ExpiresAt,
		)
	}
	return nil
}

// Enabled reports whether this Noop mailer actually sends anything.
// Always false; callers can use this to skip pre-render work when no
// mailer is wired.
func (m NoopMailer) Enabled() bool { return false }

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
// breach email. Sent the first time a tenant's MTD burn
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

// SendOrgInvite renders and ships the team-invite email. The
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

// Enabled reports whether this ResendMailer will actually send.
// Always true on a non-nil ResendMailer; the NoopMailer counterpart
// returns false.
func (m *ResendMailer) Enabled() bool { return m != nil && m.APIKey != "" }

// SendHobbyBillingNotification renders one of three templates
// (charge_failed, card_detached, receipt) and sends via Resend.
func (m *ResendMailer) SendHobbyBillingNotification(
	ctx context.Context, in HobbyBillingNotificationInput,
) error {
	var subject, htmlBody, textBody string
	dashboardURL := in.DashboardURL
	if dashboardURL == "" {
		dashboardURL = "https://app.mesedi.ai"
	}

	switch in.Kind {
	case HobbyBillingNotificationChargeFailed:
		subject = fmt.Sprintf("Mesedi: your Hobby card was declined (attempt %d of %d)",
			in.FailureCount, in.FailureCeiling)
		textBody = fmt.Sprintf(
			"Your Mesedi Hobby overage charge of $%.2f did not go through.\n\n"+
				"We will try again in about 48 hours. After %d consecutive failed attempts, "+
				"we will remove the saved card from your project and you will need to attach a new one from %s/app/billing to keep using Mesedi above the free quota.\n\n"+
				"If your card has changed, please update it at: %s/app/billing\n\n"+
				"Project: %s\nAttempt: %d of %d\n",
			in.AmountUSD, in.FailureCeiling, dashboardURL, dashboardURL,
			in.ProjectName, in.FailureCount, in.FailureCeiling,
		)
		htmlBody = fmt.Sprintf(
			"<p>Your Mesedi Hobby overage charge of <strong>$%.2f</strong> did not go through.</p>"+
				"<p>We will try again in about 48 hours. After %d consecutive failed attempts, "+
				"we will remove the saved card from your project and you will need to attach a new one from "+
				"<a href=\"%s/app/billing\">%s/app/billing</a> to keep using Mesedi above the free quota.</p>"+
				"<p>Project: <strong>%s</strong><br/>Attempt: %d of %d</p>",
			in.AmountUSD, in.FailureCeiling, dashboardURL, dashboardURL,
			in.ProjectName, in.FailureCount, in.FailureCeiling,
		)
	case HobbyBillingNotificationCardDetached:
		subject = "Mesedi: your saved card has been removed"
		// Defensive default: a caller that forgot to populate
		// IncludedExecutions still gets a sensible value rather than
		// "free Hobby quota (0 executions per month)".
		includedExecs := in.IncludedExecutions
		if includedExecs <= 0 {
			includedExecs = 10000
		}
		textBody = fmt.Sprintf(
			"After %d consecutive failed charge attempts, we have removed the saved card from your Mesedi project.\n\n"+
				"Your project will continue to work at the free Hobby quota (%s executions per month). "+
				"To use Mesedi above the free quota again, please attach a new card from %s/app/billing.\n\n"+
				"Project: %s\nURL: %s/app/billing\n",
			in.FailureCeiling, formatThousands(includedExecs), dashboardURL, in.ProjectName, dashboardURL,
		)
		htmlBody = fmt.Sprintf(
			"<p>After %d consecutive failed charge attempts, we have removed the saved card from your Mesedi project.</p>"+
				"<p>Your project will continue to work at the free Hobby quota (%s executions per month). "+
				"To use Mesedi above the free quota again, please attach a new card from "+
				"<a href=\"%s/app/billing\">%s/app/billing</a>.</p>"+
				"<p>Project: <strong>%s</strong></p>",
			in.FailureCeiling, formatThousands(includedExecs), dashboardURL, dashboardURL, in.ProjectName,
		)
	case HobbyBillingNotificationReceipt:
		subject = "Mesedi: Hobby overage charged"
		textBody = fmt.Sprintf(
			"Your Mesedi Hobby overage of $%.2f has been charged successfully.\n\n"+
				"A new billing period has started. The next charge (if any) will fire after the new period closes.\n\n"+
				"Project: %s\nPayment reference: %s\n",
			in.AmountUSD, in.ProjectName, in.PaymentIntentID,
		)
		htmlBody = fmt.Sprintf(
			"<p>Your Mesedi Hobby overage of <strong>$%.2f</strong> has been charged successfully.</p>"+
				"<p>A new billing period has started. The next charge (if any) will fire after the new period closes.</p>"+
				"<p>Project: <strong>%s</strong><br/>Payment reference: <code>%s</code></p>",
			in.AmountUSD, in.ProjectName, in.PaymentIntentID,
		)
	default:
		return fmt.Errorf("mail: unknown HobbyBillingNotificationKind %q", in.Kind)
	}

	body, err := json.Marshal(resendRequest{
		From:    m.From,
		To:      []string{in.ToEmail},
		Subject: subject,
		HTML:    htmlBody,
		Text:    textBody,
	})
	if err != nil {
		return fmt.Errorf("mail: marshal hobby billing notification: %w", err)
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
		m.Logger.Info("mail: hobby billing notification sent",
			"to", in.ToEmail,
			"kind", string(in.Kind),
			"project", in.ProjectName,
		)
	}
	return nil
}

// SendDowngradeScheduled emails the customer that their Cloud Team
// downgrade has been recorded (danger-zone flow on /app/settings).
// Two phrasings depending on ImmediateFlip: in the normal Path-A
// flow the cancellation lands at PeriodEnd; in the corrupted-state
// Path B (DB tier=Team but no Stripe subscription) the flip happens
// instantly because there's nothing to cancel.
func (m *ResendMailer) SendDowngradeScheduled(ctx context.Context, in DowngradeScheduledInput) error {
	subject := "Mesedi: Cloud Team downgrade scheduled"
	if in.ImmediateFlip {
		subject = "Mesedi: reverted to Cloud Hobby"
	}

	periodLine := ""
	if !in.ImmediateFlip {
		periodLine = fmt.Sprintf(
			"Cloud Team coverage runs until %s. After that the project reverts to Cloud Hobby (10,000 executions / 15-day retention).\n\n",
			in.PeriodEnd.Format("January 2, 2006"),
		)
	} else {
		periodLine = "Cloud Hobby is effective immediately.\n\n"
	}

	textBody := fmt.Sprintf(
		"Hi,\n\n"+
			"This is a confirmation that you scheduled a downgrade for the Mesedi project %q.\n\n"+
			"%s"+
			"What stays:\n"+
			"  - All recent executions inside the 15-day Hobby retention window\n"+
			"  - Your API keys (they continue to authenticate against the Hobby tier)\n"+
			"  - Project settings, severity routing, webhooks\n\n"+
			"What changes:\n"+
			"  - Included executions drop from 100,000 / month to 10,000 / month\n"+
			"  - Hobby is 1 project, 1 person. Team invites are no longer accepted; if your org had other members, remove them before downgrading or the downgrade will be blocked.\n"+
			"  - Executions older than 15 days are pruned at the next period start\n\n"+
			"If this was a mistake, you can cancel the downgrade from /app/billing in the dashboard before the period ends.\n\n"+
			"View billing: %s/app/billing\n",
		in.ProjectName, periodLine, in.DashboardURL,
	)
	htmlBody := fmt.Sprintf(
		"<p>This is a confirmation that you scheduled a downgrade for the Mesedi project <strong>%s</strong>.</p>"+
			"<p>%s</p>"+
			"<p><strong>What stays:</strong></p>"+
			"<ul>"+
			"<li>All recent executions inside the 15-day Hobby retention window</li>"+
			"<li>Your API keys (they continue to authenticate against the Hobby tier)</li>"+
			"<li>Project settings, severity routing, webhooks</li>"+
			"</ul>"+
			"<p><strong>What changes:</strong></p>"+
			"<ul>"+
			"<li>Included executions drop from 100,000 / month to 10,000 / month</li>"+
			"<li>Hobby is 1 project, 1 person. Team invites are no longer accepted; if your org had other members, remove them before downgrading or the downgrade will be blocked.</li>"+
			"<li>Executions older than 15 days are pruned at the next period start</li>"+
			"</ul>"+
			"<p>If this was a mistake, you can cancel the downgrade from <a href=\"%s/app/billing\">your billing page</a> before the period ends.</p>",
		in.ProjectName, periodLine, in.DashboardURL,
	)

	body, err := json.Marshal(resendRequest{
		From:    m.From,
		To:      []string{in.ToEmail},
		Subject: subject,
		HTML:    htmlBody,
		Text:    textBody,
	})
	if err != nil {
		return fmt.Errorf("mail: marshal downgrade scheduled: %w", err)
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
		m.Logger.Info("mail: downgrade scheduled sent",
			"to", in.ToEmail,
			"project", in.ProjectName,
			"immediate_flip", in.ImmediateFlip,
		)
	}
	return nil
}

// SendAccountClosed confirms the close-account cascade succeeded
//. Sent immediately AFTER DeleteProjectCascade, before the
// dashboard's force-logout fires, so the customer has a paper trail.
//
// History:
//
// PL8 (Robert): an older draft promised "we can help you re-create
// the project with your information" which was false, since the
// cascade wiped everything including the audit trail, leaving us
// with nothing. We also considered pointing at security@ for
// takeover cases but dropped it because the audit row capturing
// the close was also gone, so there was nothing for security to
// investigate.
//
// Pre-launch (, migration 031): we decoupled audit_events
// from projects so audit rows now survive DeleteProjectCascade
// (project_name_snapshot + project_deleted_at columns). That
// makes the security pointer honest again -- if a victim of an
// account takeover or a member-without-permission contacts us,
// we CAN tell them who pressed Close from what IP at what time
// against what API key, because that audit row is now preserved
// for forensic queries against the admin-only search endpoint.
// So the security pointer is back in the email body, with a
// 30-day window that matches our "fast triage" support promise
// rather than the full 7-year retention window. Deleted DATA
// (executions, events, webhooks) is still unrecoverable; the
// pointer is about establishing accountability, not restoration.
//
// The SupportEmail field on AccountClosedInput is used here as
// the contact address customers reply to.
func (m *ResendMailer) SendAccountClosed(ctx context.Context, in AccountClosedInput) error {
	subject := "Mesedi: account closed"

	// Default support address if caller did not set one. Falling
	// back to a hard-coded address avoids an awkward "contact
	// {{empty}}" line if a future caller forgets to populate the
	// field.
	supportAddr := in.SupportEmail
	if supportAddr == "" {
		supportAddr = "security@mesedi.ai"
	}

	textBody := fmt.Sprintf(
		"Hi,\n\n"+
			"This confirms that the Mesedi project %q was permanently closed on %s.\n\n"+
			"What was deleted:\n"+
			"  - All executions, events, and failure groups\n"+
			"  - Every API key on the project\n"+
			"  - Webhooks and their delivery history\n"+
			"  - The project, organization, members, and pending invites\n"+
			"  - Any Stripe subscription tied to the project (canceled immediately)\n\n"+
			"If you did NOT authorize this closure, contact %s within 30 days.\n"+
			"We retain the audit trail of who pressed Close and from where, and can\n"+
			"use it to investigate. Deleted data itself cannot be recovered, but we\n"+
			"can help you establish accountability.\n\n"+
			"Thank you for trying Mesedi. We'd love to have you back anytime.\n",
		in.ProjectName,
		in.ClosedAt.Format("January 2, 2006 at 3:04 PM MST"),
		supportAddr,
	)
	htmlBody := fmt.Sprintf(
		"<p>This confirms that the Mesedi project <strong>%s</strong> was permanently closed on %s.</p>"+
			"<p><strong>What was deleted:</strong></p>"+
			"<ul>"+
			"<li>All executions, events, and failure groups</li>"+
			"<li>Every API key on the project</li>"+
			"<li>Webhooks and their delivery history</li>"+
			"<li>The project, organization, members, and pending invites</li>"+
			"<li>Any Stripe subscription tied to the project (canceled immediately)</li>"+
			"</ul>"+
			"<p>If you did <strong>NOT</strong> authorize this closure, contact "+
			"<a href=\"mailto:%s\">%s</a> within 30 days. We retain the audit "+
			"trail of who pressed Close and from where, and can use it to "+
			"investigate. Deleted data itself cannot be recovered, but we can "+
			"help you establish accountability.</p>"+
			"<p>Thank you for trying Mesedi. We'd love to have you back anytime.</p>",
		in.ProjectName,
		in.ClosedAt.Format("January 2, 2006 at 3:04 PM MST"),
		supportAddr, supportAddr,
	)

	body, err := json.Marshal(resendRequest{
		From:    m.From,
		To:      []string{in.ToEmail},
		Subject: subject,
		HTML:    htmlBody,
		Text:    textBody,
	})
	if err != nil {
		return fmt.Errorf("mail: marshal account closed: %w", err)
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
		m.Logger.Info("mail: account closed sent",
			"to", in.ToEmail,
			"project", in.ProjectName,
		)
	}
	return nil
}

// SendMagicLink ships the one-time sign-in link email. Body
// is intentionally austere: one URL, a short expiry note, no
// branding or marketing. Customers expect transactional auth mail to
// look transactional; HTML-heavy templates raise phishing concerns.
func (m *ResendMailer) SendMagicLink(ctx context.Context, in MagicLinkInput) error {
	subject := "Your Mesedi sign-in link"
	expiresIn := time.Until(in.ExpiresAt).Round(time.Minute)
	textBody := fmt.Sprintf(
		"Click the link below to sign in to Mesedi.\n\n"+
			"%s\n\n"+
			"This link is valid for %s and can be used once.\n"+
			"If you did not request this, you can ignore the email.\n",
		in.SignInURL, expiresIn,
	)
	htmlBody := fmt.Sprintf(
		"<p>Click the link below to sign in to Mesedi.</p>"+
			"<p><a href=\"%s\">%s</a></p>"+
			"<p>This link is valid for %s and can be used once. "+
			"If you did not request this, you can ignore the email.</p>",
		in.SignInURL, in.SignInURL, expiresIn,
	)

	body, err := json.Marshal(resendRequest{
		From:    m.From,
		To:      []string{in.ToEmail},
		Subject: subject,
		HTML:    htmlBody,
		Text:    textBody,
	})
	if err != nil {
		return fmt.Errorf("mail: marshal magic link: %w", err)
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
		m.Logger.Info("mail: magic link sent",
			"to", in.ToEmail,
		)
	}
	return nil
}
