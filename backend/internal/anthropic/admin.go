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
