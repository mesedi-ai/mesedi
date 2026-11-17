package api

// GET /me/spend/daily?from=YYYY-MM-DD&to=YYYY-MM-DD
//
// The spend Mesedi saw in monitored runs, per UTC day and provider,
// priced exactly as runs are priced (llmCallCost). Read by the self-run
// spend check (sdk-python, mesedi.spend_check), which compares it with
// the provider's own bill on the customer's computer, so the provider's
// admin key never reaches Mesedi (owner decision, 17 November 2026).
//
// Spend outside monitored runs, a stolen key used elsewhere included,
// is invisible to Mesedi by design; the difference between the bill and
// this report is the only way to see it, and it is the customer's to
// compute.

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

// spendDailyMaxDays bounds one request; a longer period is several.
const spendDailyMaxDays = 93

type spendDay struct {
	Date      string             `json:"date"`
	TotalUSD  float64            `json:"total_usd"`
	Providers map[string]float64 `json:"providers"`
	Calls     int                `json:"calls"`
}

func (h *Handlers) HandleSpendDaily(w http.ResponseWriter, r *http.Request) {
	projectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	from, err := time.Parse("2006-01-02", r.URL.Query().Get("from"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "from must be a date, YYYY-MM-DD")
		return
	}
	to, err := time.Parse("2006-01-02", r.URL.Query().Get("to"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "to must be a date, YYYY-MM-DD")
		return
	}
	if to.Before(from) {
		writeError(w, http.StatusBadRequest, "to is before from")
		return
	}
	days := int(to.Sub(from).Hours()/24) + 1
	if days > spendDailyMaxDays {
		writeError(w, http.StatusBadRequest, "at most 93 days per request")
		return
	}

	tier := ""
	if p, err := h.Store.GetProject(r.Context(), projectID); err == nil && p != nil {
		tier = p.Tier
	}
	overrides := LoadProjectDetectorThresholds(r.Context(), h.Store, h.Logger, projectID, tier, nil).Pricing.CustomModelPricing

	calls, err := h.Store.ListLLMCallCostsInRange(r.Context(), projectID, from, to.AddDate(0, 0, 1))
	if err != nil {
		h.Logger.Error("spend daily: list calls failed", "project_id", projectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not read spend")
		return
	}
	// Request-scoped: max spendDailyMaxDays entries, one per day asked for.
	byDay := make(map[string]*spendDay, days)
	for i := 0; i < days; i++ {
		d := from.AddDate(0, 0, i).Format("2006-01-02")
		byDay[d] = &spendDay{Date: d, Providers: map[string]float64{}}
	}
	for _, c := range calls {
		day, ok := byDay[c.At.UTC().Format("2006-01-02")]
		if !ok {
			continue
		}
		usd, _ := llmCallCost(llmCostFields{Model: c.Model, InputTokens: c.InputTokens,
			OutputTokens: c.OutputTokens, EstimatedCostUSD: c.EstimatedCostUSD}, overrides)
		prov := callProvider(c.Provider, c.Model)
		day.Providers[prov] += usd
		day.TotalUSD += usd
		day.Calls++
	}
	out := make([]*spendDay, 0, days)
	for _, d := range byDay {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	writeJSON(w, http.StatusOK, map[string]any{
		"from":     from.Format("2006-01-02"),
		"to":       to.Format("2006-01-02"),
		"currency": "USD",
		"days":     out,
		"note": "Spend Mesedi saw in monitored runs, estimated from token counts at list " +
			"prices. Spend outside monitored runs is not here: compare with your provider's " +
			"bill to see it.",
	})
}

// callProvider names the provider a model call went to: the SDK's own
// provider field, or the model name's family, or "unknown".
func callProvider(provider, model string) string {
	if p := strings.ToLower(strings.TrimSpace(provider)); p != "" {
		return p
	}
	m := strings.ToLower(model)
	switch {
	case strings.HasPrefix(m, "claude"):
		return "anthropic"
	case strings.HasPrefix(m, "gpt-"), strings.HasPrefix(m, "chatgpt"), strings.HasPrefix(m, "o1"),
		strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"), strings.HasPrefix(m, "text-embedding"):
		return "openai"
	case strings.HasPrefix(m, "gemini"):
		return "gemini"
	case strings.HasPrefix(m, "command"):
		return "cohere"
	}
	return "unknown"
}
