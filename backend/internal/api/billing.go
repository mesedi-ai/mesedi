// Stripe billing endpoints. This file implements the four backend
// surfaces #120 needs:
//
//	POST /billing/checkout: auth-required. Creates a Stripe
//	                           Checkout session for the calling
//	                           project to upgrade Hobby → Pro;
//	                           returns the hosted-Checkout URL.
//
//	POST /billing/portal: auth-required. Creates a Stripe
//	                           Customer Portal session so an
//	                           already-paying project can update
//	                           card, see invoices, or cancel.
//
//	GET  /billing: auth-required. Returns the calling
//	                           project's tier, current-period
//	                           executions used, period bounds, and
//	                           tier-defined limits. Drives the
//	                           dashboard /app/billing page.
//
//	GET  /billing/usage: auth-required. Returns daily
//	                           execution counts for the last 30
//	                           days. Drives the usage chart on the
//	                           billing page.
//
//	POST /billing/webhook: PUBLIC (no bearer). Receives Stripe
//	                           events. Authenticity is verified via
//	                           the Stripe-Signature header and the
//	                           shared webhook secret. Dispatches
//	                           checkout.session.completed,
//	                           customer.subscription.updated,
//	                           customer.subscription.deleted, and
//	                           invoice.paid.
//
// Enforcement (Hobby silent-drop, Pro overage usage records) is
// deliberately not wired in this slice, the counter increments on
// every POST /executions but nothing gates on it yet. The follow-up
// enforcement slice adds those gates without changing the schema.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v82"
	portalsession "github.com/stripe/stripe-go/v82/billingportal/session"
	checkoutsession "github.com/stripe/stripe-go/v82/checkout/session"
	"github.com/stripe/stripe-go/v82/customer"
	"github.com/stripe/stripe-go/v82/invoiceitem"
	"github.com/stripe/stripe-go/v82/subscription"
	"github.com/stripe/stripe-go/v82/webhook"

	"mesedi/backend/internal/mail"
	"mesedi/backend/internal/store"
)

// ── tier constants (mirror /pricing page) ───────────────────────
//
// The 4-tier pricing model (post-Tier-1-4 rewrite):
//
//   Self-Hosted (Community Edition) : Free forever, MIT (no DB row)
//   Cloud Hobby                     : 10K execs free / mo, $0.002 overage, $200 cap
//   Cloud Team                      : $99/mo, 100K execs included, $0.001 overage
//   Cloud Enterprise                : custom contract, unlimited
//
// Migration 019 flipped any pre-existing tier='pro' rows to 'team'
// so the database speaks the new vocabulary too. The legacy "pro"
// string is no longer written, but it's accepted at read time via
// normalizeTier() so any in-flight Stripe webhook that still carries
// the old label doesn't crash the dispatcher.

const (
	TierHobby      = "hobby"
	TierTeam       = "team"
	TierEnterprise = "enterprise"

	// TierProLegacy is the pre-rewrite name for what is now "team".
	// Accepted at read time only; never written. Lets old data
	// (and pre-deploy Stripe webhook retries) flow without surprise.
	TierProLegacy = "pro"

	// HobbyExecutionLimit is the included monthly quota on Hobby.
	// Executions in [0, HobbyExecutionLimit] are free; executions
	// above this number bill at HobbyOveragePriceUSD each.
	HobbyExecutionLimit = 10000
	// HobbyDefaultRetentionDays is the retention window applied at
	// signup AND enforced as the tier cap in tierRetentionCap. Drift
	// guard tools/check-tier-constants.sh asserts this matches the
	// TS lib/tier-constants.ts retentionDays for the hobby tier.
	HobbyDefaultRetentionDays = 15
	// TeamDefaultRetentionDays is the retention window applied at
	// upgrade AND enforced as the tier cap in tierRetentionCap.
	TeamDefaultRetentionDays = 90
	// HobbyOveragePriceUSD is the per-execution cost above the
	// Hobby free quota. The cap-enforcement path computes
	// (executions_above_quota * HobbyOveragePriceUSD) against the
	// project's billing_cap_usd; when the cost crosses the cap, the
	// ingest path returns 402 and stops accepting new executions.
	HobbyOveragePriceUSD = 0.002

	// TeamExecutionIncluded is the included monthly quota on Team.
	// Executions past this number bill at TeamOveragePriceUSD each.
	TeamExecutionIncluded = 100000
	// TeamOveragePriceUSD is the per-execution overage cost on Team.
	// Half the Hobby rate (volume customers pay less per unit).
	TeamOveragePriceUSD = 0.001

	// TeamAIAnalysisLimit caps the number of LLM-assisted root-cause
	// analyses a Team-tier ORGANIZATION can request per billing
	// period (Mesedi #27). The cap is per-organization (tenant_id),
	// NOT per-project, because Team allows unlimited projects under
	// one org and a per-project cap would be trivially bypassed by
	// creating more projects under the same subscription.
	//
	// Each analysis triggers an Anthropic API call billed to
	// Verdifax LLC's account; without a cap, an org generating
	// thousands of distinct failure groups could push the monthly
	// Anthropic bill into hundreds of dollars.
	//
	// 200 / period is calibrated to cover realistic diagnostic use:
	// a typical Team org with a healthy 1-5% failure rate across
	// 100K monthly executions (summed across all their projects)
	// produces 1K-5K flagged executions, which deduplicate into
	// 50-300 distinct failure groups (most failures recur). 200
	// analyses covers the long-tail comfortably.
	//
	// Cap math at $0.03 per analysis (Haiku 4.5 input + output):
	// 200 * $0.03 = $6 worst case per Team org per month. At
	// $99/mo revenue, that is ~6% of revenue to Anthropic, a
	// healthy COGS ratio for an AI-powered feature.
	//
	// Past the cap, HandleAnalyzeFailureGroup returns the raw
	// failure-group row (failure class, severity, sample executions)
	// without invoking the LLM; the dashboard surfaces an "AI
	// explanation rate limit reached for this organization this
	// period" note instead of the Markdown analysis card.
	TeamAIAnalysisLimit = 200

	// HobbyAIAnalysisLimit is 0: Hobby projects do not get
	// LLM-assisted root-cause analysis at all. This matches the
	// industry pattern (Arize Alyx, LangSmith Engine on Plus+) of
	// gating AI-assisted analysis to paid tiers, and protects
	// Verdifax LLC's Anthropic bill from being subsidized by free
	// users. Hobby customers still see the failure detection chip
	// and event detail, just not the LLM-written paragraph.
	HobbyAIAnalysisLimit = 0

	// HobbyBillingFailureCeiling is the number of consecutive
	// failed charge attempts the HobbyBillingScheduler tolerates
	// before auto-detaching the saved payment method. Once a
	// customer's card has declined this many times in a row, the
	// scheduler clears stripe_customer_id (reverting the project to
	// hard-capped "no card on file" state) and emails the customer
	// asking them to attach a new card via the dashboard.
	HobbyBillingFailureCeiling = 5

	// HobbyBillingRetryCadence is the minimum interval between
	// charge attempts on the same project. With this set to 48h,
	// the scheduler tries on day 1, then day 3, day 5, etc., until
	// either the charge succeeds or HobbyBillingFailureCeiling is
	// hit. The daily scheduler tick still fires every 24h, but
	// projects that were attempted within the last 48h are
	// skipped to enforce the every-other-day cadence.
	HobbyBillingRetryCadence = 48 * time.Hour
)

// normalizeTier maps any legacy tier string to its canonical form.
// Called on every project read so the rest of the codebase only
// ever sees "hobby" | "team" | "enterprise". Defensive: even after
// migration 019 we keep this around so an out-of-band SQL fix that
// re-inserts "pro" doesn't break the dashboard.
func normalizeTier(tier string) string {
	if tier == TierProLegacy {
		return TierTeam
	}
	return tier
}

// ── config ─────────────────────────────────────────────────────

// StripeConfig is the minimum set of Stripe identifiers and shared
// secrets the billing handlers need at runtime. Constructed from
// environment variables in main; passed into api.New so handler
// methods can access it via h.Stripe.
//
// SecretKey: the test/live secret API key. Begins with "sk_test_" or
// "sk_live_". Set via MESEDI_STRIPE_SECRET_KEY.
//
// WebhookSecret: the signing secret for the configured webhook
// endpoint. Begins with "whsec_". Set via MESEDI_STRIPE_WEBHOOK_SECRET.
//
// TeamPriceID: the Stripe Price ID for the $99/mo Team plan. Begins
// with "price_". Set via MESEDI_STRIPE_TEAM_PRICE_ID; the legacy
// MESEDI_STRIPE_PRO_PRICE_ID is still honored at startup as a
// fallback so an in-flight deploy doesn't lose billing on the env
// rename.
//
// If any of the three is empty the billing endpoints respond with
// 503; this lets the backend run in local-dev without Stripe configured.
type StripeConfig struct {
	SecretKey     string
	WebhookSecret string
	TeamPriceID   string
}

// Configured returns true iff all three required Stripe values are
// present. When false, billing endpoints return 503 with a clear
// message instead of crashing on missing config.
func (c StripeConfig) Configured() bool {
	return c.SecretKey != "" && c.WebhookSecret != "" && c.TeamPriceID != ""
}

// applyKey sets the package-global stripe.Key once per request from
// the configured SecretKey. The Stripe Go SDK uses a global for the
// default backend; setting it on each call is cheap and safe (no
// hidden mutation across goroutines beyond the assignment itself,
// which is a same-value write after the first call).
func (c StripeConfig) applyKey() {
	stripe.Key = c.SecretKey
}

// ── helpers ────────────────────────────────────────────────────

// tierExecutionLimit returns the included monthly execution quota
// for a tier. Enterprise returns 0 meaning "no fixed limit"; the
// dashboard renders that as the unicode infinity sign.
func tierExecutionLimit(tier string) int64 {
	switch normalizeTier(tier) {
	case TierHobby:
		return HobbyExecutionLimit
	case TierTeam:
		return TeamExecutionIncluded
	case TierEnterprise:
		return 0
	default:
		return HobbyExecutionLimit
	}
}

// tierOveragePriceUSD returns the per-execution overage price for a
// tier. Enterprise returns 0 (no per-execution overage; contract-
// driven). Hobby is the most expensive per unit, Team is half that
// rate.
func tierOveragePriceUSD(tier string) float64 {
	switch normalizeTier(tier) {
	case TierHobby:
		return HobbyOveragePriceUSD
	case TierTeam:
		return TeamOveragePriceUSD
	default:
		return 0
	}
}

// billingNotConfigured writes the standard 503 used when an
// endpoint is called before STRIPE_* env vars are set.
func billingNotConfigured(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable,
		"Stripe is not configured on this backend; billing endpoints are disabled")
}

// computeOverageCostUSD returns the paid-overage cost for the
// project's current period. Computed live from the execution count
// rather than persisted so there's exactly one source of truth.
//
// Formula: max(0, executions_this_period - included_quota) *
//
//	per-execution rate for the project's tier.
//
// Included quota = tier base + non-expired admin-granted bonus.
// Enterprise returns 0 (no per-execution overage; contract-driven).
func computeOverageCostUSD(p *store.Project) float64 {
	if p == nil {
		return 0
	}
	tier := normalizeTier(p.Tier)
	rate := tierOveragePriceUSD(tier)
	if rate == 0 {
		return 0
	}
	now := time.Now().UTC()
	grant := p.GrantedExecutions
	if p.GrantedExecutionsExpiresAt != nil && now.After(*p.GrantedExecutionsExpiresAt) {
		grant = 0
	}
	included := tierExecutionLimit(tier) + grant
	if included < 0 {
		included = 0
	}
	overUnits := p.ExecutionsThisPeriod - included
	if overUnits <= 0 {
		return 0
	}
	return float64(overUnits) * rate
}

// capExceeded reports whether the project's current per-period
// overage cost has crossed its billing_cap_usd. Returns the
// computed cost and the cap so callers can include both in the
// error message. A zero or negative cap is treated as "no cap"
// (the customer explicitly opted into uncapped billing); ingest
// proceeds normally. Enterprise projects also bypass since their
// per-execution rate is 0.
//
// Hobby special case: a Hobby project with no Stripe customer on
// file (i.e., no payment method ever attached) is hard-capped at
// the included free quota. The first execution past the included
// quota returns true here, blocking ingest until the customer
// attaches a card via POST /billing/payment-method/setup. This is
// the right behavior because we have no way to charge them for any
// accrued overage; without this guard, every cardless Hobby
// project would silently accumulate up to $200 of uncollectable
// overage each month before getting cut off.
func capExceeded(p *store.Project) (bool, float64, float64) {
	if p == nil {
		return false, 0, 0
	}
	cost := computeOverageCostUSD(p)

	// Hobby-no-card: hard cap at the included quota. cost > 0 means
	// they've already exceeded it; block immediately. The reported
	// "cap" is 0 so the 402 message can say "free quota exceeded; add
	// a card to continue."
	if normalizeTier(p.Tier) == TierHobby && p.StripeCustomerID == "" {
		return cost > 0, 0, cost
	}

	capUSD := p.BillingCapUSD
	if capUSD <= 0 {
		return false, capUSD, cost
	}
	return cost >= capUSD, capUSD, cost
}

// effectiveCapUSD returns the dollar overage ceiling the cap-check
// actually enforces for this project right now. Mirrors capExceeded's
// branching so the dashboard can render the same number the ingest
// path applies.
//
//   - Hobby with no Stripe customer  → 0 (hard cap at free quota)
//   - Anyone else with BillingCapUSD > 0 → BillingCapUSD
//   - Anyone else with BillingCapUSD <= 0 → math.Inf(+1) is too noisy
//     for the JSON wire; we return 0 here too and the dashboard treats
//     0 + has_payment_method=true as "uncapped" (renders an infinity
//     sign instead of "$0.00").
func effectiveCapUSD(p *store.Project) float64 {
	if p == nil {
		return 0
	}
	if normalizeTier(p.Tier) == TierHobby && p.StripeCustomerID == "" {
		return 0
	}
	if p.BillingCapUSD > 0 {
		return p.BillingCapUSD
	}
	return 0
}

// ── response payloads ──────────────────────────────────────────

type BillingStatusResponse struct {
	OK                   bool   `json:"ok"`
	ProjectID            string `json:"project_id"`
	Tier                 string `json:"tier"`
	ExecutionsThisPeriod int64  `json:"executions_this_period"`
	IncludedExecutions   int64  `json:"included_executions"`
	// GrantedExecutions surfaces the raw admin-granted bonus so the
	// dashboard can render it as marketing social proof on the tier
	// card ("5,000 included · +100,000 admin granted"). Zero means
	// no grant; negative means a prior grant was revoked. This value
	// is zeroed out in the response when the grant has expired.
	GrantedExecutions int64 `json:"granted_executions"`
	// GrantedExecutionsExpiresAt is the moment the bonus stops
	// counting. Nil means no expiration set. When the dashboard
	// renders the grant on the tier card, it should also show this
	// date + a days-remaining countdown.
	GrantedExecutionsExpiresAt *time.Time `json:"granted_executions_expires_at,omitempty"`
	// TierExpiresAt is the moment an admin-flipped tier reverts to
	// Hobby. Nil means the tier is permanent. The dashboard shows
	// this on the current-tier row when set.
	TierExpiresAt            *time.Time `json:"tier_expires_at,omitempty"`
	OveragePricePerExecution float64    `json:"overage_price_per_execution_usd"`
	// BillingCapUSD is the monthly hard cap on overage spend. When
	// OverageCostThisPeriodUSD crosses this number the ingest path
	// silent-drops new executions with 402. Default $200; future
	// slice lets customers configure it.
	BillingCapUSD float64 `json:"billing_cap_usd"`
	// OverageCostThisPeriodUSD is the running paid-overage cost for
	// the active period. Resets to 0 on invoice.paid + on the
	// per-tier rollover for Hobby (which has no Stripe invoice).
	OverageCostThisPeriodUSD float64    `json:"overage_cost_this_period_usd"`
	CurrentPeriodStart       *time.Time `json:"current_period_start,omitempty"`
	CurrentPeriodEnd         *time.Time `json:"current_period_end,omitempty"`
	StripeCustomerID         string     `json:"stripe_customer_id,omitempty"`
	StripeSubscriptionID     string     `json:"stripe_subscription_id,omitempty"`
	// CanUpgrade is true when this project can go from Hobby to Team
	// via POST /billing/checkout. False if already on Team or
	// Enterprise.
	CanUpgrade bool `json:"can_upgrade"`
	// CanManage is true when this project has an existing Stripe
	// customer / subscription it can manage via POST /billing/portal.
	CanManage bool `json:"can_manage"`
	// HasPaymentMethod is true when the project has a Stripe customer
	// id on file (indicating a card has been attached via either the
	// Team Checkout flow or the Hobby Setup Intent flow). For Hobby
	// specifically, false here means the project is hard-capped at the
	// included free quota and ingest returns 402 the moment a
	// customer exceeds it. The dashboard uses this to surface an
	// "Add a card" CTA on /app/billing.
	HasPaymentMethod bool `json:"has_payment_method"`
	// EffectiveCapUSD is the cap that ingest actually enforces right
	// now. For Hobby-no-card projects it's 0 (hard-cap at included
	// quota); for everyone else it's BillingCapUSD. The dashboard
	// renders this in the "Cap" row so the customer sees the same
	// number ingest applies.
	EffectiveCapUSD float64 `json:"effective_cap_usd"`
}

type CheckoutResponse struct {
	OK        bool   `json:"ok"`
	URL       string `json:"url"`
	SessionID string `json:"session_id"`
}

type PortalResponse struct {
	OK  bool   `json:"ok"`
	URL string `json:"url"`
}

type UsageResponse struct {
	OK    bool                        `json:"ok"`
	Days  []store.DailyExecutionCount `json:"days"`
	Since time.Time                   `json:"since"`
	Until time.Time                   `json:"until"`
}

// ── handlers ───────────────────────────────────────────────────

// HandleGetBilling returns the calling project's billing snapshot.
// Auth-required. Always returns 200 with the current tier even if
// Stripe is not configured (Hobby projects don't need Stripe).
func (h *Handlers) HandleGetBilling(w http.ResponseWriter, r *http.Request) {
	projectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	p, err := h.Store.GetProject(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "load project: "+err.Error())
		return
	}

	// Lazy expiration enforcement (#168).
	// 1) If tier_expires_at has passed, the project effectively
	//    reverts to Hobby for response purposes. The DB column still
	//    holds the original tier value for auditing.
	// 2) If granted_executions_expires_at has passed, the bonus
	//    counts as zero for the included-executions math, and the
	//    granted_executions field in the response is zeroed too so
	//    the customer dashboard doesn't show a stale "+N admin
	//    granted" badge.
	now := time.Now().UTC()
	effectiveTier := normalizeTier(p.Tier)
	if p.TierExpiresAt != nil && now.After(*p.TierExpiresAt) {
		effectiveTier = TierHobby
	}
	effectiveGrant := p.GrantedExecutions
	if p.GrantedExecutionsExpiresAt != nil && now.After(*p.GrantedExecutionsExpiresAt) {
		effectiveGrant = 0
	}

	// Effective included executions = base tier allowance + any admin-
	// granted credits (see #150 promo lever). Enterprise has 0 base
	// (treated as unlimited via the bool below) and the grant adds
	// on top of that conceptually; in practice enterprise customers
	// don't go through the grant flow.
	included := tierExecutionLimit(effectiveTier) + effectiveGrant
	if included < 0 {
		// Negative grants larger than the tier base would produce a
		// nonsense effective limit; floor at zero so the dashboard
		// renders "0 of 0" instead of a confusing negative.
		included = 0
	}
	resp := BillingStatusResponse{
		OK:                         true,
		ProjectID:                  p.ProjectID,
		Tier:                       effectiveTier,
		ExecutionsThisPeriod:       p.ExecutionsThisPeriod,
		IncludedExecutions:         included,
		GrantedExecutions:          effectiveGrant,
		GrantedExecutionsExpiresAt: p.GrantedExecutionsExpiresAt,
		TierExpiresAt:              p.TierExpiresAt,
		OveragePricePerExecution:   tierOveragePriceUSD(effectiveTier),
		BillingCapUSD:              p.BillingCapUSD,
		// OverageCostThisPeriodUSD is computed live from the
		// execution count + tier rate; not persisted. computeOverageCostUSD
		// returns 0 when the project is still inside the free quota
		// or when the tier has no overage (Enterprise).
		OverageCostThisPeriodUSD: computeOverageCostUSD(p),
		CurrentPeriodStart:       p.CurrentPeriodStart,
		CurrentPeriodEnd:         p.CurrentPeriodEnd,
		StripeCustomerID:         p.StripeCustomerID,
		StripeSubscriptionID:     p.StripeSubscriptionID,
		CanUpgrade:               effectiveTier == TierHobby && h.Stripe.Configured(),
		CanManage:                p.StripeCustomerID != "" && h.Stripe.Configured(),
		// HasPaymentMethod was previously derived from p.StripeCustomerID
		// != "", which is wrong: the Hobby Setup Intent flow creates the
		// Stripe customer record BEFORE the customer attaches a card. If
		// they bounce out of the hosted Checkout page without confirming,
		// the customer record exists with no payment method, and the
		// dashboard wrongly displayed "Card on file" (#187 Robert
		// flagged this concretely). Source of truth: Stripe's
		// invoice_settings.default_payment_method on the customer; the
		// setup_intent.succeeded webhook sets that, so checking it here
		// agrees with whatever Stripe says.
		HasPaymentMethod: stripeCustomerHasPaymentMethod(h, p.StripeCustomerID),
		EffectiveCapUSD:  effectiveCapUSD(p),
	}
	writeJSON(w, http.StatusOK, resp)
}

// stripeCustomerHasPaymentMethod live-queries Stripe for the
// customer's invoice_settings.default_payment_method. Returns false
// for empty customer id, unconfigured Stripe, or any API error (we
// fail closed so a Stripe outage doesn't accidentally show "Card on
// file" when the truth is unknown). The Stripe call adds ~50-150ms
// to GET /billing in the worst case; the page is not on a hot path.
func stripeCustomerHasPaymentMethod(h *Handlers, customerID string) bool {
	if customerID == "" || !h.Stripe.Configured() {
		return false
	}
	h.Stripe.applyKey()
	cust, err := customer.Get(customerID, nil)
	if err != nil || cust == nil {
		h.Logger.Warn("stripe customer fetch for has_payment_method failed",
			"customer_id", customerID,
			"error", func() string {
				if err != nil {
					return err.Error()
				}
				return "nil customer"
			}())
		return false
	}
	if cust.InvoiceSettings != nil &&
		cust.InvoiceSettings.DefaultPaymentMethod != nil &&
		cust.InvoiceSettings.DefaultPaymentMethod.ID != "" {
		return true
	}
	return false
}

// HandleGetBillingUsage returns daily execution counts for the last
// 30 days, used by the usage chart on /app/billing. Days with zero
// executions are omitted server-side; the dashboard fills gaps
// client-side.
func (h *Handlers) HandleGetBillingUsage(w http.ResponseWriter, r *http.Request) {
	projectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	until := time.Now().UTC()
	since := until.Add(-30 * 24 * time.Hour)
	days, err := h.Store.GetDailyExecutionCounts(r.Context(), projectID, since, until)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"load daily execution counts: "+err.Error())
		return
	}
	if days == nil {
		days = []store.DailyExecutionCount{}
	}
	writeJSON(w, http.StatusOK, UsageResponse{
		OK:    true,
		Days:  days,
		Since: since,
		Until: until,
	})
}

// HandleCreateCheckout creates a Stripe Checkout session for the
// calling project to upgrade Hobby → Pro. Returns the hosted-page
// URL the dashboard will window.location.assign to.
//
// Idempotency: clicking the upgrade button twice creates two
// sessions; that's fine, Stripe expires unused sessions after 24h.
// On the success URL Stripe redirects with ?session_id={CHECKOUT_SESSION_ID}
// so the dashboard can read the current /billing state immediately.
func (h *Handlers) HandleCreateCheckout(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "admin") {
		return
	}
	if !h.Stripe.Configured() {
		billingNotConfigured(w)
		return
	}
	projectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	p, err := h.Store.GetProject(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "load project: "+err.Error())
		return
	}
	if normalizeTier(p.Tier) != TierHobby {
		writeError(w, http.StatusBadRequest,
			"project is already on a paid tier; use /billing/portal to manage")
		return
	}

	h.Stripe.applyKey()
	dashboardBase := h.resolveDashboardBase(r)
	successURL := dashboardBase + "/app/billing?status=success&session_id={CHECKOUT_SESSION_ID}"
	cancelURL := dashboardBase + "/app/billing?status=canceled"

	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				Price:    stripe.String(h.Stripe.TeamPriceID),
				Quantity: stripe.Int64(1),
			},
		},
		SuccessURL: stripe.String(successURL),
		CancelURL:  stripe.String(cancelURL),
		// ClientReferenceID lets the webhook handler find our
		// project from the Checkout completion event without needing
		// a Stripe Customer lookup first.
		ClientReferenceID: stripe.String(projectID),
		Metadata: map[string]string{
			"mesedi_project_id": projectID,
		},
	}
	// Prefill email when available so the customer doesn't retype.
	if p.OwnerEmail != "" {
		params.CustomerEmail = stripe.String(p.OwnerEmail)
	}

	session, err := checkoutsession.New(params)
	if err != nil {
		h.Logger.Error("stripe checkout create failed",
			"project_id", projectID, "error", err.Error())
		writeError(w, http.StatusBadGateway,
			"create Stripe Checkout session: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, CheckoutResponse{
		OK:        true,
		URL:       session.URL,
		SessionID: session.ID,
	})
}

// HandleCreatePortal creates a Stripe Customer Portal session for
// the calling project, redirecting them to update payment method,
// view invoices, or cancel. Requires that the project has a Stripe
// customer id already (set after the first successful Checkout).
func (h *Handlers) HandleCreatePortal(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "admin") {
		return
	}
	if !h.Stripe.Configured() {
		billingNotConfigured(w)
		return
	}
	projectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	p, err := h.Store.GetProject(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "load project: "+err.Error())
		return
	}
	if p.StripeCustomerID == "" {
		writeError(w, http.StatusBadRequest,
			"project has no Stripe customer yet; upgrade first via /billing/checkout")
		return
	}

	h.Stripe.applyKey()
	returnURL := h.resolveDashboardBase(r) + "/app/billing"
	params := &stripe.BillingPortalSessionParams{
		Customer:  stripe.String(p.StripeCustomerID),
		ReturnURL: stripe.String(returnURL),
	}
	session, err := portalsession.New(params)
	if err != nil {
		h.Logger.Error("stripe portal create failed",
			"project_id", projectID, "error", err.Error())
		writeError(w, http.StatusBadGateway,
			"create Stripe Portal session: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, PortalResponse{
		OK:  true,
		URL: session.URL,
	})
}

// HandleStripeWebhook receives Stripe events. Public (no bearer);
// authenticity is verified via the Stripe-Signature header against
// the configured webhook secret. Returns 200 on every recognized
// event (even when we choose not to act) so Stripe stops retrying.
//
// The handler must read the raw request body for signature
// verification, make sure no upstream middleware consumes it
// before this runs. Today the chain is recover → log → router, and
// none of those touch the body.
func (h *Handlers) HandleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	if !h.Stripe.Configured() {
		// Best to fail quietly with a 503 here rather than 200; we
		// don't want Stripe to record success for events we won't
		// actually process. Stripe will retry while config is missing.
		billingNotConfigured(w)
		return
	}

	// Cap body size to prevent abuse, Stripe events are kilobytes,
	// not megabytes; 1 MB is comfortable headroom.
	const maxBody = 1 << 20
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}

	sig := r.Header.Get("Stripe-Signature")
	if sig == "" {
		writeError(w, http.StatusBadRequest, "missing Stripe-Signature header")
		return
	}

	// Use ConstructEventWithOptions with IgnoreAPIVersionMismatch so a
	// newer Stripe API version on the webhook endpoint (e.g. dahlia)
	// doesn't reject events when stripe-go is pinned to an older one
	// (e.g. basil). The fields we read (id, type, data.object) are
	// stable across these versions.
	event, err := webhook.ConstructEventWithOptions(body, sig, h.Stripe.WebhookSecret, webhook.ConstructEventOptions{
		IgnoreAPIVersionMismatch: true,
	})
	if err != nil {
		// Invalid signature is the normal case for misconfigured
		// secrets or replay attempts; return 400 (not 401, which
		// Stripe treats as "endpoint dead").
		h.Logger.Warn("stripe webhook signature verify failed", "error", err.Error())
		writeError(w, http.StatusBadRequest, "signature verification failed")
		return
	}

	logger := h.Logger.With("stripe_event_id", event.ID, "stripe_event_type", string(event.Type))
	logger.Info("stripe webhook received")

	if err := h.dispatchStripeEvent(r.Context(), event, logger); err != nil { //nolint:contextcheck
		// Logged at the dispatch site; respond 200 anyway so Stripe
		// doesn't endlessly retry events we can't currently process
		// (e.g., transient DB outage will be re-fired by a future
		// related event; long retry storms aren't useful).
		// Stripe's "Failed delivery" UI surfaces non-2xx, so for now
		// log + 200 is the right trade-off. Re-evaluate if we
		// implement idempotency keys later.
		logger.Error("stripe webhook dispatch failed", "error", err.Error())
	}
	w.WriteHeader(http.StatusOK)
}

// dispatchStripeEvent routes one verified Stripe event to its
// per-type handler. Unknown event types are logged at info and
// silently acknowledged. The context passed in is the request
// context; per-handler database calls use context.Background()
// because Stripe should not see request cancellation as "event
// failed", the response status is what controls Stripe's retry
// behavior, not whether the side effects finished.
func (h *Handlers) dispatchStripeEvent(
	ctx context.Context,
	event stripe.Event,
	logger *slog.Logger,
) error {
	_ = ctx // reserved for future per-event tracing; see comment above
	switch event.Type {
	case "checkout.session.completed":
		return h.handleCheckoutCompleted(event)
	case "customer.subscription.created", "customer.subscription.updated":
		// Stripe fires `created` when the Checkout flow finalizes a new
		// subscription and `updated` on subsequent changes (renewal,
		// cancel-at-period-end, plan switch). Both events carry the
		// same stripe.Subscription payload shape, so the same handler
		// works for both, and listening on `created` is critical
		// because the Checkout session itself doesn't carry the period
		// bounds, so without this case the dashboard would show
		// "current period syncing…" until the next renewal.
		return h.handleSubscriptionUpdated(event)
	case "customer.subscription.deleted":
		return h.handleSubscriptionDeleted(event)
	case "invoice.upcoming":
		// invoice.upcoming fires ~1 hour before Stripe finalizes the
		// next subscription invoice (configurable in Dashboard, default
		// 1 hour). This is our window to push overage as an InvoiceItem
		// attached to that upcoming invoice so the customer gets billed
		// for executions above the included quota at period close.
		return h.handleInvoiceUpcoming(event, logger)
	case "invoice.paid":
		return h.handleInvoicePaid(event)
	case "setup_intent.succeeded":
		// Hobby card-attach flow: customer finished Stripe Elements
		// confirmCardSetup; save the resulting payment method as the
		// customer's default so the hobby billing scheduler can charge
		// it off-session at period close.
		return h.handleSetupIntentSucceeded(event, logger)
	default:
		logger.Info("stripe event ignored (not handled)")
		return nil
	}
}

// handleCheckoutCompleted upgrades the project to Pro and records
// the Stripe customer + subscription identifiers. The
// ClientReferenceID we set on session creation gives us the project_id
// without an extra Stripe round-trip.
func (h *Handlers) handleCheckoutCompleted(event stripe.Event) error {
	var session stripe.CheckoutSession
	if err := json.Unmarshal(event.Data.Raw, &session); err != nil {
		return fmt.Errorf("unmarshal checkout.session: %w", err)
	}
	projectID := strings.TrimSpace(session.ClientReferenceID)
	if projectID == "" {
		// Fall back to metadata if ClientReferenceID is empty.
		if session.Metadata != nil {
			projectID = strings.TrimSpace(session.Metadata["mesedi_project_id"])
		}
	}
	if projectID == "" {
		return fmt.Errorf("checkout.session.completed missing project_id")
	}
	customerID := ""
	if session.Customer != nil {
		customerID = session.Customer.ID
	}
	// CRITICAL (#188 Robert): the SAME checkout.session.completed event
	// fires for both Team-tier subscription Checkout AND Hobby-tier
	// setup-mode Checkout (the card-attach flow). The previous code
	// unconditionally set TierTeam, which silently upgraded any Hobby
	// customer who attached a card to Team without ever opening a
	// subscription. Distinguishing on session.Mode is the only reliable
	// signal: "subscription" is a real upgrade, "setup" is just
	// collecting a card.
	if session.Mode == stripe.CheckoutSessionModeSetup {
		// Hobby card-attach. The Stripe customer id is already
		// persisted by HandleCreateSetupCheckout; the
		// setup_intent.succeeded webhook will save the resulting
		// payment method and bootstrap period bounds. Nothing to do
		// here.
		return nil
	}
	subscriptionID := ""
	if session.Subscription != nil {
		subscriptionID = session.Subscription.ID
	}
	// Period bounds may not be on the Checkout session itself; the
	// subsequent customer.subscription.updated event will fill them
	// in. For now, set to nil, the dashboard handles missing bounds
	// gracefully.
	return h.Store.UpdateProjectBilling(
		context.Background(),
		projectID, TierTeam, customerID, subscriptionID, nil, nil,
	)
}

// handleSubscriptionUpdated refreshes period bounds and (if the
// subscription was canceled at period end) records the downgrade
// signal. Actual downgrade happens on customer.subscription.deleted.
func (h *Handlers) handleSubscriptionUpdated(event stripe.Event) error {
	var sub stripe.Subscription
	if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
		return fmt.Errorf("unmarshal subscription: %w", err)
	}
	if sub.Customer == nil {
		return fmt.Errorf("subscription.updated missing customer")
	}
	p, err := h.Store.GetProjectByStripeCustomerID(context.Background(), sub.Customer.ID)
	if err != nil {
		return fmt.Errorf("lookup project by customer %s: %w", sub.Customer.ID, err)
	}
	periodStart, periodEnd := subscriptionPeriodBounds(&sub)
	tier := normalizeTier(p.Tier)
	if sub.Status == stripe.SubscriptionStatusActive ||
		sub.Status == stripe.SubscriptionStatusTrialing {
		tier = TierTeam
	}
	return h.Store.UpdateProjectBilling(
		context.Background(),
		p.ProjectID, tier, sub.Customer.ID, sub.ID, periodStart, periodEnd,
	)
}

// handleSubscriptionDeleted downgrades the project back to Hobby
// when the Stripe subscription is canceled (either at period end or
// immediately).
func (h *Handlers) handleSubscriptionDeleted(event stripe.Event) error {
	var sub stripe.Subscription
	if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
		return fmt.Errorf("unmarshal subscription: %w", err)
	}
	if sub.Customer == nil {
		return fmt.Errorf("subscription.deleted missing customer")
	}
	p, err := h.Store.GetProjectByStripeCustomerID(context.Background(), sub.Customer.ID)
	if err != nil {
		return fmt.Errorf("lookup project by customer %s: %w", sub.Customer.ID, err)
	}
	// Keep the customer id so the user can re-subscribe later
	// without re-collecting card data; clear the subscription id and
	// period bounds. Tier returns to Hobby.
	return h.Store.UpdateProjectBilling(
		context.Background(),
		p.ProjectID, TierHobby, sub.Customer.ID, "", nil, nil,
	)
}

// handleInvoicePaid resets the per-period execution counter when a
// new invoice is paid (i.e., a new billing period begins). Idempotent:
// if Stripe re-delivers the same event the counter resets to zero
// again on the (already-active) new period.
func (h *Handlers) handleInvoicePaid(event stripe.Event) error {
	var invoice stripe.Invoice
	if err := json.Unmarshal(event.Data.Raw, &invoice); err != nil {
		return fmt.Errorf("unmarshal invoice: %w", err)
	}
	if invoice.Customer == nil {
		// Not all invoices have a customer (one-off payments etc.);
		// for a subscription product we expect one. Log and move on.
		return nil
	}
	p, err := h.Store.GetProjectByStripeCustomerID(context.Background(), invoice.Customer.ID)
	if err != nil {
		return fmt.Errorf("lookup project by customer %s: %w", invoice.Customer.ID, err)
	}
	periodStart, periodEnd := invoicePeriodBounds(&invoice)
	if periodStart == nil || periodEnd == nil {
		// Invoice without period bounds, nothing to roll over.
		return nil
	}
	return h.Store.ResetExecutionsThisPeriod(
		context.Background(),
		p.ProjectID, *periodStart, *periodEnd,
	)
}

// handleInvoiceUpcoming pushes the project's accumulated overage onto
// the upcoming invoice as a Stripe InvoiceItem. Called when Stripe
// emits invoice.upcoming, which by default fires ~1 hour before the
// next subscription invoice finalizes (configurable per webhook
// endpoint in the Stripe Dashboard).
//
// Only Team-tier projects act here. Hobby has no Stripe subscription
// so it never emits invoice.upcoming; Hobby overage is collected by a
// separate scheduler that charges the saved payment method directly.
// Enterprise has a contract-driven rate ($0/exec at this layer) so
// computeOverageCostUSD returns 0 and we short-circuit.
//
// Idempotency: the Stripe event ID is used as the request idempotency
// key on invoiceitem.New, so a Stripe-side re-delivery of the same
// event produces the same InvoiceItem (Stripe dedupes server-side)
// rather than double-charging the customer.
//
// Cap respect: if the project has a billing_cap_usd set, the pushed
// amount is min(computed_cost, cap). This mirrors the ingest-path cap
// behavior so the customer never sees a charge larger than they
// agreed to.
//
// Rounding: per-execution rate is $0.001 (sub-cent). We compute the
// total in dollars then round to the nearest cent. Sub-cent residue
// is discarded; over a full period this is at most $0.005 of
// rounding loss in the customer's favor, which is the right
// direction.
func (h *Handlers) handleInvoiceUpcoming(event stripe.Event, logger *slog.Logger) error {
	var invoice stripe.Invoice
	if err := json.Unmarshal(event.Data.Raw, &invoice); err != nil {
		return fmt.Errorf("unmarshal invoice.upcoming: %w", err)
	}
	if invoice.Customer == nil {
		// Not a customer-attached invoice; nothing to bill.
		return nil
	}
	customerID := invoice.Customer.ID
	p, err := h.Store.GetProjectByStripeCustomerID(context.Background(), customerID)
	if err != nil {
		return fmt.Errorf("lookup project by customer %s: %w", customerID, err)
	}
	// Only Team-tier customers go through this path. Hobby has no
	// Stripe subscription (so no invoice.upcoming ever fires for them).
	// Enterprise is contract-driven; computeOverageCostUSD returns 0
	// for them which would already short-circuit below, but bail
	// explicitly so the log is clearer.
	tier := normalizeTier(p.Tier)
	if tier != TierTeam {
		logger.Info("invoice.upcoming ignored: non-Team tier",
			"project_id", p.ProjectID, "tier", tier)
		return nil
	}

	cost := computeOverageCostUSD(p)
	if cost <= 0 {
		// Inside the included quota; nothing to bill.
		logger.Info("invoice.upcoming: no overage to bill",
			"project_id", p.ProjectID,
			"executions_this_period", p.ExecutionsThisPeriod)
		return nil
	}

	// Respect the billing cap. A cap of zero means uncapped (no
	// adjustment). Most Team customers run uncapped; this branch
	// matters only for customers who opted into a self-imposed
	// ceiling via the dashboard (#168 follow-up).
	if p.BillingCapUSD > 0 && cost > p.BillingCapUSD {
		cost = p.BillingCapUSD
	}

	// Convert to integer cents. Stripe wants minor currency units as
	// an int64. math.Round to handle the standard float-to-int boundary;
	// at our sub-cent rate the residue is negligible and the rounding
	// is in the customer's favor most of the time.
	cents := int64(math.Round(cost * 100))
	if cents <= 0 {
		// Overage existed but rounded to zero cents (less than half a
		// cent of actual overage). Not worth a Stripe API call.
		return nil
	}

	included := tierExecutionLimit(tier) + p.GrantedExecutions
	if included < 0 {
		included = 0
	}
	overUnits := p.ExecutionsThisPeriod - included

	desc := fmt.Sprintf(
		"Mesedi Team overage: %d executions x $%.3f",
		overUnits, TeamOveragePriceUSD,
	)

	h.Stripe.applyKey()
	params := &stripe.InvoiceItemParams{
		Customer:    stripe.String(customerID),
		Amount:      stripe.Int64(cents),
		Currency:    stripe.String(string(stripe.CurrencyUSD)),
		Description: stripe.String(desc),
		Metadata: map[string]string{
			"mesedi_project_id":      p.ProjectID,
			"mesedi_stripe_event_id": event.ID,
			"mesedi_overage_units":   fmt.Sprintf("%d", overUnits),
			"mesedi_tier":            tier,
		},
	}
	// InvoiceItem.Customer alone is sufficient: Stripe automatically
	// attaches "pending" (un-invoiced) items to that customer's next
	// finalized invoice. We have one subscription per Mesedi project,
	// so disambiguation isn't needed. (Stripe API v2024+ removed the
	// Invoice.Subscription field; the relationship lives on a
	// per-line-item Parent struct now, which the upcoming invoice
	// preview doesn't always populate.)
	//
	// Idempotency key = Stripe event ID. If Stripe re-delivers this
	// exact event, the duplicate InvoiceItem.New call returns the
	// already-created item rather than creating a second one.
	params.IdempotencyKey = stripe.String(event.ID)

	ii, err := invoiceitem.New(params)
	if err != nil {
		return fmt.Errorf("create overage invoice item: %w", err)
	}
	logger.Info("invoice.upcoming: overage pushed",
		"project_id", p.ProjectID,
		"overage_units", overUnits,
		"amount_cents", cents,
		"invoice_item_id", ii.ID,
	)
	return nil
}

// ── Hobby Setup Intent flow ──────────────────────────────────────
//
// Hobby projects don't go through Stripe Checkout (no subscription)
// but they DO need a payment method on file to use any execution
// past the free quota. The Setup Intent flow is how they attach one:
//
//   1. Dashboard POSTs /billing/payment-method/setup
//   2. Backend creates a Stripe customer for the project (if not yet
//      created) and a SetupIntent against that customer.
//   3. Backend returns {client_secret, customer_id} to the dashboard.
//   4. Dashboard renders Stripe Elements with the client_secret and
//      calls stripe.confirmCardSetup(...) on form submit.
//   5. Stripe processes the card. On success it fires the
//      setup_intent.succeeded webhook to our /billing/webhook endpoint.
//   6. Webhook handler sets the resulting payment method as the
//      customer's invoice_settings.default_payment_method so future
//      off-session PaymentIntents charge it automatically. It also
//      bootstraps the project's billing period bounds if NULL
//      (period_start = now, period_end = now + 1 month).
//
// At this point the project has stripe_customer_id != "", capExceeded
// reverts to the normal BillingCapUSD ceiling ($200 default), and the
// hobby billing scheduler will charge any accrued overage at period
// rollover.

type SetupCheckoutResponse struct {
	OK        bool   `json:"ok"`
	URL       string `json:"url"`
	SessionID string `json:"session_id"`
}

// HandleCreateSetupCheckout creates (or reuses) the Stripe customer
// for the calling project and returns a Stripe Checkout session URL
// in setup mode. The dashboard redirects the customer to that URL;
// Stripe hosts the card-entry form on its own domain and redirects
// back to the dashboard's success URL on completion.
//
// Setup-mode Checkout generates a SetupIntent under the hood, so
// the existing handleSetupIntentSucceeded webhook handler picks up
// the resulting payment method and sets it as the customer's
// default invoice payment method (so the hobby billing scheduler
// can charge it off-session at period close).
//
// Why Stripe Checkout instead of Stripe Elements in-app: smaller
// dashboard surface (no @stripe/stripe-js dependency, no CSP
// adjustments for the Stripe iframe), and Stripe owns all the PCI
// scope at the cost of a ~10 second redirect round-trip. For a
// solo-dev ship that trade-off is the right one.
func (h *Handlers) HandleCreateSetupCheckout(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "admin") {
		return
	}
	if !h.Stripe.Configured() {
		billingNotConfigured(w)
		return
	}
	projectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	p, err := h.Store.GetProject(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "load project: "+err.Error())
		return
	}

	h.Stripe.applyKey()

	customerID := p.StripeCustomerID
	if customerID == "" {
		// First card-attach for this project. Create a Stripe customer
		// with the project's owner email so invoices land in the right
		// inbox. The Mesedi project_id goes into metadata so webhook
		// re-resolution back to a project works.
		params := &stripe.CustomerParams{
			Metadata: map[string]string{
				"mesedi_project_id": p.ProjectID,
			},
		}
		if p.OwnerEmail != "" {
			params.Email = stripe.String(p.OwnerEmail)
		}
		if p.Name != "" {
			params.Description = stripe.String("Mesedi project: " + p.Name)
		}
		cust, cErr := customer.New(params)
		if cErr != nil {
			h.Logger.Error("stripe customer create failed",
				"project_id", projectID, "error", cErr.Error())
			writeError(w, http.StatusBadGateway,
				"create Stripe customer: "+cErr.Error())
			return
		}
		customerID = cust.ID
		// Persist the customer id immediately. Even if the Checkout
		// session creation below fails, the next attempt will reuse
		// this customer rather than create a duplicate.
		if upErr := h.Store.UpdateProjectBilling(
			r.Context(),
			p.ProjectID,
			normalizeTier(p.Tier),
			customerID,
			p.StripeSubscriptionID,
			p.CurrentPeriodStart,
			p.CurrentPeriodEnd,
		); upErr != nil {
			h.Logger.Error("persist stripe customer id failed",
				"project_id", projectID, "error", upErr.Error())
			writeError(w, http.StatusInternalServerError,
				"persist Stripe customer id: "+upErr.Error())
			return
		}
	}

	dashboardBase := h.resolveDashboardBase(r)
	successURL := dashboardBase + "/app/billing?status=card-attached&session_id={CHECKOUT_SESSION_ID}"
	cancelURL := dashboardBase + "/app/billing?status=card-attach-canceled"

	// Mode=setup means Stripe does not collect a payment now;
	// instead it collects the card and generates a SetupIntent that
	// fires setup_intent.succeeded on completion. usage=off_session
	// inside SetupIntentData is the magic flag that lets us charge
	// the saved card later without the customer being present (the
	// hobby billing scheduler runs at period close off-session).
	params := &stripe.CheckoutSessionParams{
		Mode:               stripe.String(string(stripe.CheckoutSessionModeSetup)),
		Customer:           stripe.String(customerID),
		PaymentMethodTypes: stripe.StringSlice([]string{"card"}),
		SuccessURL:         stripe.String(successURL),
		CancelURL:          stripe.String(cancelURL),
		ClientReferenceID:  stripe.String(p.ProjectID),
		Metadata: map[string]string{
			"mesedi_project_id": p.ProjectID,
			"mesedi_flow":       "hobby_card_attach",
		},
		SetupIntentData: &stripe.CheckoutSessionSetupIntentDataParams{
			Metadata: map[string]string{
				"mesedi_project_id": p.ProjectID,
			},
		},
	}

	session, err := checkoutsession.New(params)
	if err != nil {
		h.Logger.Error("stripe setup checkout create failed",
			"project_id", projectID, "error", err.Error())
		writeError(w, http.StatusBadGateway,
			"create Stripe Setup Checkout session: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, SetupCheckoutResponse{
		OK:        true,
		URL:       session.URL,
		SessionID: session.ID,
	})
}

// handleSetupIntentSucceeded fires when the dashboard's Stripe
// Elements confirmCardSetup call succeeds. The webhook payload
// carries the attached payment_method ID; we save it as the
// customer's default invoice payment method so the hobby billing
// scheduler can charge it off-session at period close.
//
// Also bootstraps the project's billing period bounds for Hobby
// projects whose period_start/end are NULL. We do this at first
// card-attach (not at signup) because before a card is on file
// there's no need to track a period boundary (overage is hard-
// capped at zero so there's nothing to bill).
func (h *Handlers) handleSetupIntentSucceeded(event stripe.Event, logger *slog.Logger) error {
	var si stripe.SetupIntent
	if err := json.Unmarshal(event.Data.Raw, &si); err != nil {
		return fmt.Errorf("unmarshal setup_intent: %w", err)
	}
	if si.Customer == nil || si.Customer.ID == "" {
		// No customer attached to the SetupIntent. Shouldn't happen
		// (we always create them against a customer) but bail
		// cleanly rather than panic.
		return nil
	}
	if si.PaymentMethod == nil || si.PaymentMethod.ID == "" {
		// No payment method on the succeeded intent. Also shouldn't
		// happen for our flow.
		return nil
	}

	customerID := si.Customer.ID
	pmID := si.PaymentMethod.ID

	// Set the new payment method as the customer's default for
	// future invoices.
	h.Stripe.applyKey()
	custParams := &stripe.CustomerParams{
		InvoiceSettings: &stripe.CustomerInvoiceSettingsParams{
			DefaultPaymentMethod: stripe.String(pmID),
		},
	}
	if _, err := customer.Update(customerID, custParams); err != nil {
		return fmt.Errorf("set default payment method on customer %s: %w", customerID, err)
	}

	// Bootstrap period bounds for Hobby on first card-attach.
	p, err := h.Store.GetProjectByStripeCustomerID(context.Background(), customerID)
	if err != nil {
		return fmt.Errorf("lookup project by customer %s: %w", customerID, err)
	}
	if normalizeTier(p.Tier) == TierHobby &&
		(p.CurrentPeriodStart == nil || p.CurrentPeriodEnd == nil) {
		now := time.Now().UTC()
		end := now.AddDate(0, 1, 0)
		if err := h.Store.ResetExecutionsThisPeriod(
			context.Background(),
			p.ProjectID, now, end,
		); err != nil {
			return fmt.Errorf("bootstrap period bounds for %s: %w", p.ProjectID, err)
		}
		logger.Info("hobby period bounds bootstrapped on card-attach",
			"project_id", p.ProjectID,
			"period_start", now,
			"period_end", end,
		)
	}

	logger.Info("setup_intent.succeeded: default payment method set",
		"project_id", p.ProjectID,
		"customer_id", customerID,
		"payment_method_id", pmID,
	)
	return nil
}

// ── small helpers ───────────────────────────────────────────────

// subscriptionPeriodBounds extracts current_period_start and
// current_period_end from a Stripe subscription as time.Time
// pointers. Returns (nil, nil) if either is zero (Stripe represents
// "unset" as 0 unix timestamp).
func subscriptionPeriodBounds(sub *stripe.Subscription) (*time.Time, *time.Time) {
	if sub == nil {
		return nil, nil
	}
	var startPtr, endPtr *time.Time
	// In v82 the period fields live on each subscription item.
	if len(sub.Items.Data) > 0 {
		item := sub.Items.Data[0]
		if item.CurrentPeriodStart > 0 {
			t := time.Unix(item.CurrentPeriodStart, 0).UTC()
			startPtr = &t
		}
		if item.CurrentPeriodEnd > 0 {
			t := time.Unix(item.CurrentPeriodEnd, 0).UTC()
			endPtr = &t
		}
	}
	return startPtr, endPtr
}

// HandleDowngradeToHobby cancels the project's Cloud Team Stripe
// subscription at the current period end (no proration), flips tier
// back to Hobby in the database, and clears the Stripe subscription
// id pointer. Customer keeps all their data, just loses Team
// features and 100K execs/period when the period rolls over. Wired
// for the customer-facing "Downgrade to Hobby" branch of the close-
// account flow on /app/settings (#188). Admin only.
func (h *Handlers) HandleDowngradeToHobby(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "admin") {
		return
	}
	projectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	p, err := h.Store.GetProject(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "load project: "+err.Error())
		return
	}
	if normalizeTier(p.Tier) != TierTeam {
		writeError(w, http.StatusConflict,
			"project is not on Cloud Team; downgrade only makes sense from Team")
		return
	}

	// Two paths depending on whether there's a real Stripe subscription
	// on file:
	//
	// (A) Live subscription: cancel at current_period_end so the
	//     customer keeps Team for paid-for time. The
	//     subscription.updated/deleted webhook flips DB tier to Hobby
	//     when the period rolls.
	//
	// (B) Tier=Team in DB but no Stripe subscription id (the corrupted
	//     state that resulted from the prior handleCheckoutCompleted
	//     bug auto-upgrading every checkout.session.completed): there's
	//     nothing to cancel on Stripe's side, so the subscription
	//     webhook never fires. Flip the DB directly to Hobby so the
	//     customer can actually escape the wrong tier (#188 Robert
	//     flagged this).
	if p.StripeSubscriptionID != "" && h.Stripe.Configured() {
		h.Stripe.applyKey()
		cancelAtPeriodEnd := true
		sub, sErr := subscription.Update(p.StripeSubscriptionID, &stripe.SubscriptionParams{
			CancelAtPeriodEnd: &cancelAtPeriodEnd,
		})
		if sErr != nil {
			h.Logger.Error("downgrade: subscription.update cancel_at_period_end failed",
				"project_id", projectID, "sub_id", p.StripeSubscriptionID, "error", sErr.Error())
			writeError(w, http.StatusBadGateway,
				"could not schedule Stripe cancellation: "+sErr.Error())
			return
		}
		// Fire the downgrade-scheduled confirmation email. Best-effort:
		// log on error but never block the response. PeriodEnd comes
		// from Stripe's response so the customer's email reflects what
		// Stripe actually agreed to, not what we predicted (#188 email
		// notifications).
		h.sendDowngradeEmailBestEffort(r, p, periodEndFromSub(sub), false)

		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"message": "Cloud Team will cancel at the end of the current period and the project will revert to Cloud Hobby.",
		})
		return
	}

	// Path (B): no live subscription. Flip DB tier immediately.
	if err := h.Store.UpdateProjectBilling(
		r.Context(),
		p.ProjectID,
		TierHobby,
		p.StripeCustomerID,
		"",
		nil,
		nil,
	); err != nil {
		writeError(w, http.StatusInternalServerError,
			"could not update tier: "+err.Error())
		return
	}
	h.Logger.Info("downgrade: direct tier flip (no active Stripe subscription)",
		"project_id", projectID,
		"prior_tier", p.Tier,
		"stripe_customer_id", p.StripeCustomerID)
	// Path-B email: ImmediateFlip=true so the template phrases it as
	// "reverted to Cloud Hobby" rather than "scheduled cancel".
	h.sendDowngradeEmailBestEffort(r, p, time.Now().UTC(), true)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "Reverted to Cloud Hobby. No Stripe subscription was active to cancel.",
	})
}

// sendDowngradeEmailBestEffort renders + sends the
// downgrade-scheduled confirmation. Failure is logged but never
// surfaced to the caller; the downgrade itself already succeeded
// and the email is a side notification. Skipped when the project
// has no owner email on file.
func (h *Handlers) sendDowngradeEmailBestEffort(
	r *http.Request,
	p *store.Project,
	periodEnd time.Time,
	immediateFlip bool,
) {
	if h.Mailer == nil || !h.Mailer.Enabled() || p.OwnerEmail == "" {
		return
	}
	dashboardBase := h.resolveDashboardBase(r)
	if mailErr := h.Mailer.SendDowngradeScheduled(
		r.Context(),
		mail.DowngradeScheduledInput{
			ToEmail:       p.OwnerEmail,
			ProjectName:   p.Name,
			PeriodEnd:     periodEnd,
			DashboardURL:  dashboardBase,
			ImmediateFlip: immediateFlip,
		},
	); mailErr != nil {
		h.Logger.Warn("downgrade scheduled email send failed",
			"project_id", p.ProjectID,
			"to_email", p.OwnerEmail,
			"error", mailErr.Error())
	}
}

// periodEndFromSub pulls current_period_end off a Stripe Subscription
// response. Falls back to one month from now if Stripe's response is
// missing the bound (shouldn't happen for an active sub but be
// defensive so the email always has a sensible date).
func periodEndFromSub(sub *stripe.Subscription) time.Time {
	if sub != nil && len(sub.Items.Data) > 0 &&
		sub.Items.Data[0].CurrentPeriodEnd > 0 {
		return time.Unix(sub.Items.Data[0].CurrentPeriodEnd, 0).UTC()
	}
	return time.Now().UTC().AddDate(0, 1, 0)
}

// HandleCloseAccount is the danger-zone delete. Cancels any Stripe
// subscription immediately, then hard-deletes the project and every
// dependent row via Store.DeleteProjectCascade. After this returns
// the dashboard's 401-logout handler (#187) will catch the next
// request and bounce the user to /login. Admin only. Idempotent at
// the Stripe level (canceling an already-canceled sub is a no-op).
func (h *Handlers) HandleCloseAccount(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "admin") {
		return
	}
	projectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	p, err := h.Store.GetProject(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "load project: "+err.Error())
		return
	}

	// Cancel Stripe subscription immediately if there is one. Failures
	// here are logged but do not block the cascade-delete; the customer
	// can re-cancel from the Stripe dashboard after the fact if needed,
	// and they're already losing access to dashboard either way.
	if p.StripeSubscriptionID != "" && h.Stripe.Configured() {
		h.Stripe.applyKey()
		_, sErr := subscription.Cancel(p.StripeSubscriptionID, nil)
		if sErr != nil {
			h.Logger.Warn("close account: subscription.Cancel failed",
				"project_id", projectID, "sub_id", p.StripeSubscriptionID, "error", sErr.Error())
		}
	}

	// Send the close-account confirmation BEFORE the cascade-delete
	// fires. Once DeleteProjectCascade succeeds, we lose p.OwnerEmail
	// (the project row is gone), and the customer also loses their
	// dashboard auth - so this is the last possible window to email
	// them. Best-effort: log on error but never block the close
	// (#188 email notifications).
	if h.Mailer != nil && h.Mailer.Enabled() && p.OwnerEmail != "" {
		if mailErr := h.Mailer.SendAccountClosed(
			r.Context(),
			mail.AccountClosedInput{
				ToEmail:      p.OwnerEmail,
				ProjectName:  p.Name,
				ClosedAt:     time.Now().UTC(),
				SupportEmail: "support@mesedi.ai",
			},
		); mailErr != nil {
			h.Logger.Warn("account closed email send failed",
				"project_id", projectID,
				"to_email", p.OwnerEmail,
				"error", mailErr.Error())
		}
	}

	if err := h.Store.DeleteProjectCascade(r.Context(), projectID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError,
			"delete project: "+err.Error())
		return
	}

	h.Logger.Warn("project hard-deleted via close-account flow",
		"project_id", projectID,
		"owner_email", p.OwnerEmail,
		"tier", p.Tier)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "Account closed. All project data has been deleted.",
	})
}

// HandleUpdateBillingCap lets the calling project's admin set their
// monthly overage spend cap (projects.billing_cap_usd). PUT body:
// {"cap_usd": <float>}. Validated server-side as a positive number
// less than $10,000 to avoid accidental nukes; 0 is allowed and means
// "use the constants default" (the hobby billing scheduler falls back
// to TIER_CONSTANTS.hobby.defaultCapUSD when the project field is
// zero). Returns the new value so the dashboard can update its state
// without a follow-up GET (#187).
func (h *Handlers) HandleUpdateBillingCap(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "admin") {
		return
	}
	projectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	var body struct {
		CapUSD float64 `json:"cap_usd"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.CapUSD < 0 {
		writeError(w, http.StatusBadRequest, "cap_usd must be >= 0")
		return
	}
	const maxCap = 10000.0
	if body.CapUSD > maxCap {
		writeError(w, http.StatusBadRequest,
			"cap_usd above $10,000 must be configured by support; this is a sanity guard, not a hard limit")
		return
	}
	if err := h.Store.UpdateProjectBillingCap(r.Context(), projectID, body.CapUSD); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError,
			"update billing cap: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"cap_usd": body.CapUSD,
	})
}

// invoicePeriodBounds extracts the most-likely line-item period
// from an Invoice. Stripe invoices carry per-line period bounds;
// for a simple one-product subscription the first line item is the
// subscription line and its period is the billing period we want.
func invoicePeriodBounds(inv *stripe.Invoice) (*time.Time, *time.Time) {
	if inv == nil || inv.Lines == nil || len(inv.Lines.Data) == 0 {
		return nil, nil
	}
	line := inv.Lines.Data[0]
	if line.Period == nil {
		return nil, nil
	}
	var startPtr, endPtr *time.Time
	if line.Period.Start > 0 {
		t := time.Unix(line.Period.Start, 0).UTC()
		startPtr = &t
	}
	if line.Period.End > 0 {
		t := time.Unix(line.Period.End, 0).UTC()
		endPtr = &t
	}
	return startPtr, endPtr
}
