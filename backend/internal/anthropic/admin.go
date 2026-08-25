package anthropic

// Admin API client for the Cost Report endpoint. Separate from
// client.go because the Admin API uses a different key prefix
// (sk-ant-admin-...) and lives on different URLs (organizations/*).
//
// Scope (v1):
//   - Cost Report: GET /v1/organizations/cost_report?starting_at=...&ending_at=...
//   - Sums total cost USD across the returned buckets so the caller
//     gets one number ("you spent $X.XX between $start and $end")
//   - Walks pagination via next_page until has_more is false
//
// NOT in scope:
//   - Usage Report (tokens, not dollars) — Mesedi's burn-rate widget
//     uses dollars only
//   - Workspace/API-key grouping — flat total is what the admin
//     dashboard renders
//   - The "remaining credit balance" — Anthropic does not expose this
//     via the documented Admin API. Balance is tracked separately on
//     a manual-entry surface; see store.AnthropicCreditSnapshot.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

const (
	// adminEndpointBase is the Admin API root. The Cost Report is
	// reachable at /cost_report under this base.
	adminEndpointBase = "https://api.anthropic.com/v1/organizations"
	// adminTimeout caps a single Cost Report call. The report is
	// fast (DB-backed analytics on Anthropic's side) but pagination
	// can mean multiple round trips; 30s headroom is plenty.
	adminTimeout = 30 * time.Second
)

// ErrAdminDisabled is returned by the admin client when no admin key
// is configured. Callers should surface a "not configured" UI rather
// than treating it as a hard error so the dashboard degrades
// gracefully on deployments without admin instrumentation.
var ErrAdminDisabled = errors.New("anthropic: admin client disabled (no admin key)")

// AdminClient wraps the Cost Report endpoint. Pass in an
// sk-ant-admin-... API key on construction; an empty key disables
// every call (returns ErrAdminDisabled).
type AdminClient struct {
	apiKey  string
	baseURL string
	httpc   *http.Client
}

// NewAdminClient constructs a client around the given admin API key.
// httpc is optional; nil uses a default with a 30 second timeout
// and the standard transport.
func NewAdminClient(apiKey string, httpc *http.Client) *AdminClient {
	if httpc == nil {
		httpc = &http.Client{Timeout: adminTimeout}
	}
	return &AdminClient{
		apiKey:  apiKey,
		baseURL: adminEndpointBase,
		httpc:   httpc,
	}
}

// Configured reports whether the admin client has a key. Callers
// gate their UX on this so empty-key deployments render "not
// configured" instead of fetching and failing.
func (c *AdminClient) Configured() bool {
	return c != nil && c.apiKey != ""
}

// costReportBucket is one daily bucket from the Cost Report response.
// Anthropic returns costs as strings in cents (lowest currency unit),
// per the documented "decimal strings in lowest units" contract; we
// parse to float64 USD here so the rest of the codebase deals with
// dollars.
type costReportBucket struct {
	StartingAt string `json:"starting_at"`
	EndingAt   string `json:"ending_at"`
	Results    []struct {
		// Amount is the cost in cents as a decimal string, e.g.
		// "150" = $1.50. Some Anthropic responses also use
		// "amount_str" naming under the hood; we accept both via
		// the alternate Amount field if present.
		Amount    string `json:"amount,omitempty"`
		AmountStr string `json:"amount_str,omitempty"`
		// Currency is "USD" in practice; preserved for future
		// expansion.
		Currency string `json:"currency,omitempty"`
		// Cost is an older alternative shape that returns the
		// USD float directly (some pre-release docs reference
		// this); accepted as a fallback.
		Cost float64 `json:"cost,omitempty"`
	} `json:"results"`
}

// costReportResponse is the shape of the Cost Report endpoint.
type costReportResponse struct {
	Data     []costReportBucket `json:"data"`
	HasMore  bool               `json:"has_more"`
	NextPage string             `json:"next_page,omitempty"`
}

// CostReport is the trimmed-down summary the dashboard needs:
// total USD spent across the requested window plus a per-day
// breakdown for charting. The burn-rate widget divides TotalUSD by
// the window's day count; the chart renders one bar per
// DailyBuckets entry.
type CostReport struct {
	StartingAt   time.Time
	EndingAt     time.Time
	TotalUSD     float64
	DailyBuckets []DailyCostBucket
}

// DailyCostBucket is one day of total spend. Date is the start of
// the bucket in UTC (matches Anthropic's "1d" granularity).
type DailyCostBucket struct {
	Date time.Time
	USD  float64
}

// GetCostReport returns the total USD spent between startingAt and
// endingAt inclusive. Walks pagination if Anthropic returns has_more.
// Returns ErrAdminDisabled when no key is configured so the caller
// can branch on "feature not wired" without inspecting error
// strings.
func (c *AdminClient) GetCostReport(
	ctx context.Context, startingAt, endingAt time.Time,
) (*CostReport, error) {
	if !c.Configured() {
		return nil, ErrAdminDisabled
	}
	if endingAt.Before(startingAt) {
		return nil, errors.New("anthropic: cost report ending_at before starting_at")
	}

	out := &CostReport{
		StartingAt: startingAt,
		EndingAt:   endingAt,
	}

	page := ""
	for {
		q := url.Values{}
		q.Set("starting_at", startingAt.UTC().Format(time.RFC3339))
		q.Set("ending_at", endingAt.UTC().Format(time.RFC3339))
		if page != "" {
			q.Set("page", page)
		}
		reqURL := c.baseURL + "/cost_report?" + q.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, fmt.Errorf("anthropic admin: build request: %w", err)
		}
		req.Header.Set("x-api-key", c.apiKey)
		req.Header.Set("anthropic-version", defaultAPIVersion)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "Mesedi/1.0 (https://mesedi.ai)")

		resp, err := c.httpc.Do(req)
		if err != nil {
			return nil, fmt.Errorf("anthropic admin: do request: %w", err)
		}
		body, readErr := readAllAndClose(resp)
		if readErr != nil {
			return nil, fmt.Errorf("anthropic admin: read response: %w", readErr)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("anthropic admin: cost report HTTP %d: %s",
				resp.StatusCode, truncateForError(string(body)))
		}

		var parsed costReportResponse
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("anthropic admin: decode response: %w", err)
		}
		for _, bucket := range parsed.Data {
			var bucketUSD float64
			for _, r := range bucket.Results {
				usd, err := parseCostUSD(r.Amount, r.AmountStr, r.Cost)
				if err != nil {
					return nil, fmt.Errorf("anthropic admin: parse cost: %w", err)
				}
				bucketUSD += usd
			}
			out.TotalUSD += bucketUSD
			// Parse the bucket's start timestamp so the chart can
			// render with real UTC dates. Bad timestamps degrade
			// silently to zero-time so the spend total is still
			// accurate even if Anthropic ships a malformed value.
			start, _ := time.Parse(time.RFC3339, bucket.StartingAt)
			out.DailyBuckets = append(out.DailyBuckets, DailyCostBucket{
				Date: start.UTC(),
				USD:  bucketUSD,
			})
		}
		if !parsed.HasMore || parsed.NextPage == "" {
			break
		}
		page = parsed.NextPage
	}
	return out, nil
}

// parseCostUSD normalizes the multiple cost-amount shapes Anthropic
// has used. The current docs say "decimal strings in lowest units
// (cents)" — i.e., "150" means $1.50. Pre-release shapes used
// "cost": 1.50 as a float in dollars. We accept either and fail loud
// if neither field carries a value.
func parseCostUSD(amount, amountStr string, costDollars float64) (float64, error) {
	raw := amount
	if raw == "" {
		raw = amountStr
	}
	if raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid amount string %q: %w", raw, err)
		}
		// Cents -> dollars.
		return v / 100.0, nil
	}
	if costDollars != 0 {
		return costDollars, nil
	}
	// Zero is a valid bucket (e.g., free workspace with no usage).
	return 0, nil
}

// readAllAndClose drains the response body and closes it. A 4 MiB
// cap is enforced because cost reports are tiny in practice (a few
// KB even for a 31-day report); anything larger is a bug or a
// malicious response.
func readAllAndClose(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	const maxBody = 4 * 1024 * 1024
	return io.ReadAll(io.LimitReader(resp.Body, maxBody))
}

// truncateForError snips a long body for an error message so we
// don't dump megabytes into logs on a 4xx.
func truncateForError(s string) string {
	const max = 512
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// ─────────────────────────────────────────────────────────────────
// Usage Report — the only way to see TODAY's spend
// ─────────────────────────────────────────────────────────────────
//
// WHY THIS EXISTS (verified against the live API on 2026-08-25, so
// nobody has to re-derive it):
//
//   - /cost_report returns dollars but OMITS the current in-progress
//     UTC day entirely. Setting ending_at to a future timestamp is
//     accepted without error and still returns nothing for today.
//   - /usage_report/messages with bucket_width=1d omits today too.
//   - /usage_report/messages with bucket_width=1h DOES include today,
//     hour by hour, and group_by[]=model populates the model field.
//     It returns token counts, not dollars.
//
// So: settled days come from the cost report (authoritative dollars,
// Anthropic's own arithmetic), and today is reconstructed from hourly
// token counts priced with our own rate table. The two are
// deliberately different sources, which is why the API and the UI
// both label today as an estimate rather than blending it silently
// into a figure that looks equally settled.
//
// The practical consequence of not having this: on launch morning
// the admin panel read "$0.00 / day" while real spend was happening,
// because every dollar of it landed on the in-progress day. The
// number was true and useless.

// usageReportResult is one grouped row inside an hourly bucket.
// Fields mirror the Admin API exactly; the ones we do not price
// (server_tool_use, service_tier, context_window) are omitted rather
// than parsed and ignored.
type usageReportResult struct {
	UncachedInputTokens int `json:"uncached_input_tokens"`
	CacheCreation       struct {
		Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens"`
		Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens"`
	} `json:"cache_creation"`
	CacheReadInputTokens int `json:"cache_read_input_tokens"`
	OutputTokens         int `json:"output_tokens"`
	// Model is null unless group_by[]=model is requested. We always
	// request it — without a model we cannot price the tokens, since
	// Sonnet and Haiku differ by 5x on input.
	Model string `json:"model"`
	// WorkspaceID is null unless group_by[]=workspace_id is
	// requested. Not used yet; it is the hook for per-product COGS
	// once Mesedi has its own workspace inside the org.
	WorkspaceID string `json:"workspace_id"`
}

type usageReportBucket struct {
	StartingAt string              `json:"starting_at"`
	EndingAt   string              `json:"ending_at"`
	Results    []usageReportResult `json:"results"`
}

type usageReportResponse struct {
	Data     []usageReportBucket `json:"data"`
	HasMore  bool                `json:"has_more"`
	NextPage string              `json:"next_page,omitempty"`
}

// UsageCost is priced token usage over a window, broken down by
// model so the caller can show which model is driving spend.
type UsageCost struct {
	StartingAt time.Time
	EndingAt   time.Time
	// TotalUSD is our own computation from token counts, NOT a
	// figure Anthropic returned. Treat it as an estimate.
	TotalUSD float64
	// ByModel maps model id to USD. Empty when there was no usage.
	ByModel map[string]float64
	// Usage aggregates tokens across every model, for display.
	Usage TokenUsage
	// UnpricedModels lists model ids seen in the response that have
	// no explicit entry in the rate table, so the caller can warn
	// rather than quietly bill them at the fallback rate. A new
	// model shipping is the normal cause.
	UnpricedModels []string
}

// GetUsageCost returns priced token usage between startingAt and
// endingAt using hourly buckets grouped by model. Use this for the
// current in-progress day; use GetCostReport for settled days.
//
// Returns ErrAdminDisabled when no key is configured, matching
// GetCostReport so callers branch identically.
func (c *AdminClient) GetUsageCost(
	ctx context.Context, startingAt, endingAt time.Time,
) (*UsageCost, error) {
	if !c.Configured() {
		return nil, ErrAdminDisabled
	}
	if endingAt.Before(startingAt) {
		return nil, errors.New("anthropic: usage report ending_at before starting_at")
	}

	out := &UsageCost{
		StartingAt: startingAt,
		EndingAt:   endingAt,
		ByModel:    map[string]float64{},
	}
	unpriced := map[string]bool{}

	page := ""
	for {
		q := url.Values{}
		q.Set("starting_at", startingAt.UTC().Format(time.RFC3339))
		q.Set("ending_at", endingAt.UTC().Format(time.RFC3339))
		// Hourly is mandatory, not a preference: the daily bucket
		// width silently excludes the in-progress day, which is the
		// only day this method exists to see.
		q.Set("bucket_width", "1h")
		// Without this the model field comes back null and the
		// tokens are unpriceable.
		q.Set("group_by[]", "model")
		if page != "" {
			q.Set("page", page)
		}
		reqURL := c.baseURL + "/usage_report/messages?" + q.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, fmt.Errorf("anthropic admin: build usage request: %w", err)
		}
		req.Header.Set("x-api-key", c.apiKey)
		req.Header.Set("anthropic-version", defaultAPIVersion)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "Mesedi/1.0 (https://mesedi.ai)")

		resp, err := c.httpc.Do(req)
		if err != nil {
			return nil, fmt.Errorf("anthropic admin: do usage request: %w", err)
		}
		body, readErr := readAllAndClose(resp)
		if readErr != nil {
			return nil, fmt.Errorf("anthropic admin: read usage response: %w", readErr)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("anthropic admin: usage report HTTP %d: %s",
				resp.StatusCode, truncateForError(string(body)))
		}

		var parsed usageReportResponse
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("anthropic admin: decode usage response: %w", err)
		}

		for _, bucket := range parsed.Data {
			for _, r := range bucket.Results {
				u := TokenUsage{
					UncachedInputTokens:  r.UncachedInputTokens,
					CacheWrite5mTokens:   r.CacheCreation.Ephemeral5mInputTokens,
					CacheWrite1hTokens:   r.CacheCreation.Ephemeral1hInputTokens,
					CacheReadInputTokens: r.CacheReadInputTokens,
					OutputTokens:         r.OutputTokens,
				}
				// A row with no tokens is not worth attributing;
				// Anthropic emits these for quiet hours.
				if u.TotalInputTokens() == 0 && u.OutputTokens == 0 {
					continue
				}
				model := r.Model
				if model == "" {
					// Should not happen given group_by[]=model, but
					// an unattributed row is still real spend. Bill
					// it at the fallback rate under a visible name
					// rather than dropping it and under-reporting.
					model = "unknown"
				}
				if !HasExplicitRate(model) {
					unpriced[model] = true
				}
				cost := ComputeUsageCostUSD(model, u)
				out.TotalUSD += cost
				out.ByModel[model] += cost

				out.Usage.UncachedInputTokens += u.UncachedInputTokens
				out.Usage.CacheWrite5mTokens += u.CacheWrite5mTokens
				out.Usage.CacheWrite1hTokens += u.CacheWrite1hTokens
				out.Usage.CacheReadInputTokens += u.CacheReadInputTokens
				out.Usage.OutputTokens += u.OutputTokens
			}
		}

		if !parsed.HasMore || parsed.NextPage == "" {
			break
		}
		page = parsed.NextPage
	}

	for m := range unpriced {
		out.UnpricedModels = append(out.UnpricedModels, m)
	}
	sort.Strings(out.UnpricedModels)
	return out, nil
}
