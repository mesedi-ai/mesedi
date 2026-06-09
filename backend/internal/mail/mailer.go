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
	// #188 customer-initiated lifecycle confirmations. Both fire from
	// the danger-zone flow on /app/settings. SendDowngradeScheduled
	// confirms that Cloud Team will cancel at the current period end;
	// SendAccountClosed confirms the project has been hard-deleted.
	SendDowngradeScheduled(ctx context.Context, in DowngradeScheduledInput) error
	SendAccountClosed(ctx context.Context, in AccountClosedInput) error
	Enabled() bool
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
// confirmation email template renders (#188). Sent immediately when
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
// confirmation email template renders (#188). Sent after
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
