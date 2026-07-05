package api

// Founder analytics + accounting surface.
//
// Design: live Stripe pass-through. Each /admin/analytics page load
// calls Stripe APIs (charge.List, refund.List, subscription.List)
// and pairs the results with local DB metadata (MRR derived from
// projects.tier='team' count × $99). No local schema, no webhooks
// to maintain — Stripe is the source of truth for money movement.
//
// Trade-off accepted: page load is 1-2 seconds instead of <100ms.
// For a founder-only page checked a few times a week, that's fine.
// If usage patterns ever justify caching we can layer Redis or a
// local table later without breaking the API contract.

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/charge"
	"github.com/stripe/stripe-go/v82/refund"
	"github.com/stripe/stripe-go/v82/subscription"
)

// TeamMonthlyPriceUSD is the monthly Cloud Team subscription price.
// Used to derive MRR from the local team-tier project count. Kept
// here (rather than read from Stripe) because (a) we set the price
// ID in env, (b) a divergence between env and Stripe would be a
// bigger problem than a stale constant, (c) it lets the MRR
// calculation happen without a Stripe round trip.
const TeamMonthlyPriceUSD = 99.0

// adminListLimitMax caps the result-set size for every admin listing
// endpoint (charges, refunds, canceled subscriptions). User-supplied
// `?limit=N` is always clamped to this ceiling before any allocation,
// keeping per-request memory bounded at sizeof(row) * adminListLimitMax.
// Used as the capacity hint in make([]Row, 0, adminListLimitMax) so a
// crafted huge `?limit=` cannot drive an excessive allocation -- and
// so CodeQL's go/uncontrolled-allocation-size query (alerts
// //) sees a constant upper bound at the make() site.
const adminListLimitMax = 200

// AdminAnalyticsSummary is the founder-side accounting tile shown
// at the top of /admin/analytics. All money fields in USD; counts
// are integer.
type AdminAnalyticsSummary struct {
	// StripeConfigured reflects whether the Stripe API is wired.
	// When false, only the MRR fields are populated (which are
	// computed from the local DB).
	StripeConfigured bool `json:"stripe_configured"`

	// Subscription-derived metrics (computed from local DB).
	//
	// ActiveTeamSubscriptions counts projects where tier='team' AND
	// stripe_subscription_id is set. The Stripe subscription id is
	// the canonical "actually paying" signal: admin tier-flips and
	// comped Team projects never get one. Pre-this counter
	// ignored the subscription id and any tier='team' row counted
	// as $99 MRR, which let admin-comped Team projects (and Team
	// projects whose subscription was canceled but tier was not
	// reverted) inflate MRR. The new filter excludes both.
	ActiveTeamSubscriptions int `json:"active_team_subscriptions"`
	// CompedTeamProjects counts projects where tier='team' but
	// stripe_subscription_id is empty. These are admin-granted /
	// comped seats that DO NOT count toward MRR. Surfaced as a
	// separate tile on the analytics page so the gap between
	// total Team seats and paying Team seats is always visible.
	CompedTeamProjects int     `json:"comped_team_projects"`
	MRRUsd             float64 `json:"mrr_usd"`
	ARRUsd             float64 `json:"arr_usd"`

	// Stripe-derived metrics (this month). Nil when Stripe isn't
	// configured or when the live call failed; the dashboard
	// branches on presence to render "—" instead of "0".
	ThisMonthGrossUSD     *float64 `json:"this_month_gross_usd,omitempty"`
	ThisMonthRefundsUSD   *float64 `json:"this_month_refunds_usd,omitempty"`
	ThisMonthNetUSD       *float64 `json:"this_month_net_usd,omitempty"`
	ThisMonthSuccessCount *int     `json:"this_month_success_count,omitempty"`
	ThisMonthFailedCount  *int     `json:"this_month_failed_count,omitempty"`

	// Window the Stripe metrics cover. RFC3339; useful for the UI
	// to render "since Jun 1, 2026" disclosure.
	WindowStart string `json:"window_start,omitempty"`
	WindowEnd   string `json:"window_end,omitempty"`
}

// HandleAdminGetAnalyticsSummary returns the top-of-page metrics
// tile. Stripe failures degrade gracefully: MRR + ARR still
// render from the local DB.
func (h *Handlers) HandleAdminGetAnalyticsSummary(w http.ResponseWriter, r *http.Request) {
	out := AdminAnalyticsSummary{
		StripeConfigured: h.Stripe.Configured(),
	}

	// 1. MRR / ARR from the local projects table. Always available,
	// no API hop. A project counts toward MRR only when it has a
	// Stripe subscription id; admin-comped Team tiers and stale
	// Team rows whose subscription was canceled but tier never
	// reverted are split out into CompedTeamProjects so they stay
	// visible without inflating MRR (see ).
	rows, err := h.Store.ListAllProjects(r.Context())
	if err == nil {
		for _, p := range rows {
			if !strings.EqualFold(p.Tier, TierTeam) {
				continue
			}
			if p.StripeSubscriptionID != "" {
				out.ActiveTeamSubscriptions++
			} else {
				out.CompedTeamProjects++
			}
		}
		out.MRRUsd = float64(out.ActiveTeamSubscriptions) * TeamMonthlyPriceUSD
		out.ARRUsd = out.MRRUsd * 12.0
	} else {
		h.Logger.Warn("analytics: list projects failed; MRR omitted",
			"error", err.Error())
	}

	// 2. Stripe metrics. Best-effort: log warnings + continue when
	// Stripe is down or unconfigured.
	if !out.StripeConfigured {
		writeJSON(w, http.StatusOK, out)
		return
	}
	h.Stripe.applyKey()

	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	out.WindowStart = monthStart.Format(time.RFC3339)
	out.WindowEnd = now.Format(time.RFC3339)

	gross, success, failed, gerr := sumChargesInWindow(monthStart, now)
	if gerr != nil {
		h.Logger.Warn("analytics: charge sum failed",
			"error", gerr.Error())
	} else {
		out.ThisMonthGrossUSD = &gross
		out.ThisMonthSuccessCount = &success
		out.ThisMonthFailedCount = &failed
	}

	refundTotal, rerr := sumRefundsInWindow(monthStart, now)
	if rerr != nil {
		h.Logger.Warn("analytics: refund sum failed",
			"error", rerr.Error())
	} else {
		out.ThisMonthRefundsUSD = &refundTotal
	}

	if out.ThisMonthGrossUSD != nil && out.ThisMonthRefundsUSD != nil {
		net := *out.ThisMonthGrossUSD - *out.ThisMonthRefundsUSD
		out.ThisMonthNetUSD = &net
	}

	writeJSON(w, http.StatusOK, out)
}

// sumChargesInWindow walks Stripe charges between start and end
// (inclusive) and returns USD gross + success count + failed count.
// Uses the standard Stripe iterator pattern; pagination is handled
// by the SDK.
func sumChargesInWindow(start, end time.Time) (float64, int, int, error) {
	params := &stripe.ChargeListParams{}
	params.Filters.AddFilter("created", "gte", strconv.FormatInt(start.Unix(), 10))
	params.Filters.AddFilter("created", "lte", strconv.FormatInt(end.Unix(), 10))
	params.Limit = stripe.Int64(100)

	var gross float64
	var success, failed int
	iter := charge.List(params)
	for iter.Next() {
		c := iter.Charge()
		if c == nil {
			continue
		}
		switch c.Status {
		case stripe.ChargeStatusSucceeded:
			gross += float64(c.Amount) / 100.0
			success++
		case stripe.ChargeStatusFailed:
			failed++
		}
	}
	if err := iter.Err(); err != nil {
		return 0, 0, 0, err
	}
	return gross, success, failed, nil
}

// sumRefundsInWindow walks Stripe refunds between start and end
// inclusive. Returns total USD refunded.
func sumRefundsInWindow(start, end time.Time) (float64, error) {
	params := &stripe.RefundListParams{}
	params.Filters.AddFilter("created", "gte", strconv.FormatInt(start.Unix(), 10))
	params.Filters.AddFilter("created", "lte", strconv.FormatInt(end.Unix(), 10))
	params.Limit = stripe.Int64(100)

	var total float64
	iter := refund.List(params)
	for iter.Next() {
		r := iter.Refund()
		if r == nil {
			continue
		}
		if r.Status == stripe.RefundStatusSucceeded {
			total += float64(r.Amount) / 100.0
		}
	}
	if err := iter.Err(); err != nil {
		return 0, err
	}
	return total, nil
}

// AdminChargeRow is one charge surfaced in the recent-charges table.
// Trimmed down from stripe.Charge so the JSON stays small and only
// surfaces what the dashboard renders.
type AdminChargeRow struct {
	ChargeID       string  `json:"charge_id"`
	AmountUSD      float64 `json:"amount_usd"`
	Currency       string  `json:"currency"`
	Status         string  `json:"status"`
	CustomerID     string  `json:"customer_id,omitempty"`
	CustomerEmail  string  `json:"customer_email,omitempty"`
	Description    string  `json:"description,omitempty"`
	FailureMessage string  `json:"failure_message,omitempty"`
	CreatedAt      string  `json:"created_at"`
	Refunded       bool    `json:"refunded"`
}

// AdminListChargesResponse is the JSON body of GET /admin/charges.
type AdminListChargesResponse struct {
	OK      bool             `json:"ok"`
	Charges []AdminChargeRow `json:"charges"`
	Limit   int              `json:"limit"`
}

// HandleAdminListCharges returns the most recent Stripe charges.
// Default limit 50, hard ceiling 200.
func (h *Handlers) HandleAdminListCharges(w http.ResponseWriter, r *http.Request) {
	if !h.Stripe.Configured() {
		writeError(w, http.StatusServiceUnavailable,
			"Stripe is not configured on this deployment")
		return
	}
	h.Stripe.applyKey()

	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > adminListLimitMax {
		limit = adminListLimitMax
	}

	params := &stripe.ChargeListParams{}
	params.Limit = stripe.Int64(int64(limit))

	out := AdminListChargesResponse{
		OK:      true,
		// Cap allocation at the package-level constant; `limit` is
		// guaranteed <= adminListLimitMax by the clamp above. Using
		// the constant here makes the upper bound visible to static
		// analysis (alert ).
		Charges: make([]AdminChargeRow, 0, adminListLimitMax),
		Limit:   limit,
	}
	iter := charge.List(params)
	for iter.Next() {
		c := iter.Charge()
		if c == nil {
			continue
		}
		row := AdminChargeRow{
			ChargeID:  c.ID,
			AmountUSD: float64(c.Amount) / 100.0,
			Currency:  strings.ToUpper(string(c.Currency)),
			Status:    string(c.Status),
			CreatedAt: time.Unix(c.Created, 0).UTC().Format(time.RFC3339),
			Refunded:  c.Refunded,
		}
		if c.Customer != nil {
			row.CustomerID = c.Customer.ID
		}
		if c.BillingDetails != nil {
			row.CustomerEmail = c.BillingDetails.Email
		}
		row.Description = c.Description
		row.FailureMessage = c.FailureMessage
		out.Charges = append(out.Charges, row)
		if len(out.Charges) >= limit {
			break
		}
	}
	if err := iter.Err(); err != nil {
		writeError(w, http.StatusBadGateway,
			"list charges: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// AdminRefundRow is one refund surfaced in the refunds list.
type AdminRefundRow struct {
	RefundID  string  `json:"refund_id"`
	ChargeID  string  `json:"charge_id,omitempty"`
	AmountUSD float64 `json:"amount_usd"`
	Currency  string  `json:"currency"`
	Status    string  `json:"status"`
	Reason    string  `json:"reason,omitempty"`
	CreatedAt string  `json:"created_at"`
}

type AdminListRefundsResponse struct {
	OK      bool             `json:"ok"`
	Refunds []AdminRefundRow `json:"refunds"`
	Limit   int              `json:"limit"`
}

// HandleAdminListRefunds returns the most recent Stripe refunds.
// Default limit 50, ceiling 200.
func (h *Handlers) HandleAdminListRefunds(w http.ResponseWriter, r *http.Request) {
	if !h.Stripe.Configured() {
		writeError(w, http.StatusServiceUnavailable,
			"Stripe is not configured on this deployment")
		return
	}
	h.Stripe.applyKey()

	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > adminListLimitMax {
		limit = adminListLimitMax
	}

	params := &stripe.RefundListParams{}
	params.Limit = stripe.Int64(int64(limit))

	out := AdminListRefundsResponse{
		OK:      true,
		// See Charges allocation above for the rationale on using
		// the constant rather than `limit` here (alert ).
		Refunds: make([]AdminRefundRow, 0, adminListLimitMax),
		Limit:   limit,
	}
	iter := refund.List(params)
	for iter.Next() {
		ref := iter.Refund()
		if ref == nil {
			continue
		}
		row := AdminRefundRow{
			RefundID:  ref.ID,
			AmountUSD: float64(ref.Amount) / 100.0,
			Currency:  strings.ToUpper(string(ref.Currency)),
			Status:    string(ref.Status),
			Reason:    string(ref.Reason),
			CreatedAt: time.Unix(ref.Created, 0).UTC().Format(time.RFC3339),
		}
		if ref.Charge != nil {
			row.ChargeID = ref.Charge.ID
		}
		out.Refunds = append(out.Refunds, row)
		if len(out.Refunds) >= limit {
			break
		}
	}
	if err := iter.Err(); err != nil {
		writeError(w, http.StatusBadGateway,
			"list refunds: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// AdminCanceledSubRow is one canceled subscription surfaced in the
// recent-churn list.
type AdminCanceledSubRow struct {
	SubscriptionID string  `json:"subscription_id"`
	CustomerID     string  `json:"customer_id,omitempty"`
	CustomerEmail  string  `json:"customer_email,omitempty"`
	Status         string  `json:"status"`
	CanceledAt     string  `json:"canceled_at,omitempty"`
	EndedAt        string  `json:"ended_at,omitempty"`
	CreatedAt      string  `json:"created_at"`
	MRRImpactUSD   float64 `json:"mrr_impact_usd"`
}

type AdminListCanceledSubsResponse struct {
	OK            bool                  `json:"ok"`
	Subscriptions []AdminCanceledSubRow `json:"subscriptions"`
	Limit         int                   `json:"limit"`
}

// HandleAdminListCanceledSubscriptions returns recent churn. Pulls
// subscriptions with status="canceled" from Stripe. Each row carries
// the MRR impact ($99 per Team sub) so the dashboard can show the
// dollar value of churn.
func (h *Handlers) HandleAdminListCanceledSubscriptions(w http.ResponseWriter, r *http.Request) {
	if !h.Stripe.Configured() {
		writeError(w, http.StatusServiceUnavailable,
			"Stripe is not configured on this deployment")
		return
	}
	h.Stripe.applyKey()

	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > adminListLimitMax {
		limit = adminListLimitMax
	}

	params := &stripe.SubscriptionListParams{
		Status: stripe.String("canceled"),
	}
	params.Limit = stripe.Int64(int64(limit))

	out := AdminListCanceledSubsResponse{
		OK:            true,
		// See Charges allocation above for the rationale on using
		// the constant rather than `limit` here (alert ).
		Subscriptions: make([]AdminCanceledSubRow, 0, adminListLimitMax),
		Limit:         limit,
	}
	iter := subscription.List(params)
	for iter.Next() {
		s := iter.Subscription()
		if s == nil {
			continue
		}
		row := AdminCanceledSubRow{
			SubscriptionID: s.ID,
			Status:         string(s.Status),
			CreatedAt:      time.Unix(s.Created, 0).UTC().Format(time.RFC3339),
			MRRImpactUSD:   TeamMonthlyPriceUSD,
		}
		if s.Customer != nil {
			row.CustomerID = s.Customer.ID
			row.CustomerEmail = s.Customer.Email
		}
		if s.CanceledAt > 0 {
			row.CanceledAt = time.Unix(s.CanceledAt, 0).UTC().Format(time.RFC3339)
		}
		if s.EndedAt > 0 {
			row.EndedAt = time.Unix(s.EndedAt, 0).UTC().Format(time.RFC3339)
		}
		out.Subscriptions = append(out.Subscriptions, row)
		if len(out.Subscriptions) >= limit {
			break
		}
	}
	if err := iter.Err(); err != nil {
		writeError(w, http.StatusBadGateway,
			"list canceled subs: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

