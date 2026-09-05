package anthropic

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// The Usage Report path exists because Anthropic's cost report omits
// the current in-progress UTC day. That gap is what made the admin
// panel read "$0.00 / day" on 2026-08-25 while real spend was
// happening. These tests pin the two things that make the
// replacement trustworthy: correct pricing (including cache
// multipliers) and correct request shape (hourly buckets grouped by
// model, daily buckets silently exclude today, which would
// reintroduce the original bug).

func approx(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %.10f, want %.10f", what, got, want)
	}
}

func TestComputeUsageCostUSD_UncachedOnly(t *testing.T) {
	t.Parallel()
	// claude-sonnet-5 is $2.00 in / $10.00 out per MTok.
	got := ComputeUsageCostUSD("claude-sonnet-5", TokenUsage{
		UncachedInputTokens: 1_000_000,
		OutputTokens:        1_000_000,
	})
	approx(t, got, 12.00, "sonnet-5 1M in + 1M out")
}

func TestComputeUsageCostUSD_CacheMultipliers(t *testing.T) {
	t.Parallel()
	// Same token count in each cache bucket, so each line item is
	// directly comparable against the base input rate. Using
	// claude-haiku-4-5 at $1.00/MTok input makes the arithmetic
	// readable: 1M tokens = exactly $1.00 before multipliers.
	got := ComputeUsageCostUSD("claude-haiku-4-5", TokenUsage{
		CacheWrite5mTokens:   1_000_000, // 1.25x -> 1.25
		CacheWrite1hTokens:   1_000_000, // 2.00x -> 2.00
		CacheReadInputTokens: 1_000_000, // 0.10x -> 0.10
	})
	approx(t, got, 3.35, "haiku cache write5m + write1h + read")
}

// A cache read priced as if it were normal input over-reports by
// 10x. This is the specific error the multipliers prevent, and it
// stays invisible until prompt caching is switched on.
func TestComputeUsageCostUSD_CacheReadIsNotFullPrice(t *testing.T) {
	t.Parallel()
	cached := ComputeUsageCostUSD("claude-sonnet-5", TokenUsage{
		CacheReadInputTokens: 1_000_000,
	})
	uncached := ComputeUsageCostUSD("claude-sonnet-5", TokenUsage{
		UncachedInputTokens: 1_000_000,
	})
	if cached >= uncached {
		t.Fatalf("cache read (%.4f) must be cheaper than uncached input (%.4f)",
			cached, uncached)
	}
	approx(t, cached, uncached*0.10, "cache read is 10%% of input")
}

func TestTokenUsage_TotalInputTokens(t *testing.T) {
	t.Parallel()
	u := TokenUsage{
		UncachedInputTokens:  1,
		CacheWrite5mTokens:   2,
		CacheWrite1hTokens:   4,
		CacheReadInputTokens: 8,
		OutputTokens:         16, // must NOT be counted as input
	}
	if got := u.TotalInputTokens(); got != 15 {
		t.Errorf("TotalInputTokens() = %d, want 15 (output must be excluded)", got)
	}
}

// GetUsageCost must request hourly buckets grouped by model. Daily
// buckets omit the in-progress day (verified against the live API
// 2026-08-25), and without group_by the model field comes back null,
// which makes the tokens unpriceable. Both mistakes are silent, so
// pin the query string.
func TestGetUsageCost_RequestShape(t *testing.T) {
	t.Parallel()

	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_ = json.NewEncoder(w).Encode(usageReportResponse{})
	}))
	defer srv.Close()

	c := NewAdminClient("sk-ant-admin-test", srv.Client())
	c.baseURL = srv.URL

	start := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	if _, err := c.GetUsageCost(context.Background(), start, start.Add(6*time.Hour)); err != nil {
		t.Fatalf("GetUsageCost: %v", err)
	}

	if got := gotQuery.Get("bucket_width"); got != "1h" {
		t.Errorf("bucket_width = %q, want 1h. Daily buckets exclude the "+
			"in-progress day, which is the only day this method exists to see.", got)
	}
	if got := gotQuery.Get("group_by[]"); got != "model" {
		t.Errorf("group_by[] = %q, want model. Without it the model field "+
			"is null and tokens cannot be priced.", got)
	}
}

func TestGetUsageCost_PricesAndGroupsByModel(t *testing.T) {
	t.Parallel()

	body := `{
	  "data": [
	    {"starting_at":"2026-08-25T00:00:00Z","ending_at":"2026-08-25T01:00:00Z","results":[
	      {"uncached_input_tokens":1000000,"cache_creation":{"ephemeral_1h_input_tokens":0,"ephemeral_5m_input_tokens":0},"cache_read_input_tokens":0,"output_tokens":0,"model":"claude-haiku-4-5-20251001"},
	      {"uncached_input_tokens":1000000,"cache_creation":{"ephemeral_1h_input_tokens":0,"ephemeral_5m_input_tokens":0},"cache_read_input_tokens":0,"output_tokens":0,"model":"claude-sonnet-5"}
	    ]},
	    {"starting_at":"2026-08-25T01:00:00Z","ending_at":"2026-08-25T02:00:00Z","results":[
	      {"uncached_input_tokens":0,"cache_creation":{"ephemeral_1h_input_tokens":0,"ephemeral_5m_input_tokens":0},"cache_read_input_tokens":0,"output_tokens":0,"model":"claude-haiku-4-5-20251001"}
	    ]}
	  ],
	  "has_more": false
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := NewAdminClient("sk-ant-admin-test", srv.Client())
	c.baseURL = srv.URL

	start := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	got, err := c.GetUsageCost(context.Background(), start, start.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("GetUsageCost: %v", err)
	}

	// Haiku 1M input = $1.00; Sonnet-5 1M input = $2.00.
	approx(t, got.TotalUSD, 3.00, "total")
	approx(t, got.ByModel["claude-haiku-4-5-20251001"], 1.00, "haiku")
	approx(t, got.ByModel["claude-sonnet-5"], 2.00, "sonnet-5")

	// The dated snapshot suffix must still resolve to the haiku rate
	// via LookupRate's prefix matching, NOT the unknown-model
	// fallback ($2/$10), that would silently double the number.
	if len(got.UnpricedModels) != 0 {
		t.Errorf("UnpricedModels = %v, want empty: a dated snapshot id "+
			"should resolve by prefix, not fall back", got.UnpricedModels)
	}

	// The all-zero row in the second bucket must be skipped, not
	// counted as a model with $0 spend.
	if _, ok := got.ByModel["claude-haiku-4-5-20251001"]; !ok {
		t.Error("haiku missing from ByModel")
	}
	if got.Usage.UncachedInputTokens != 2_000_000 {
		t.Errorf("aggregated input tokens = %d, want 2000000",
			got.Usage.UncachedInputTokens)
	}
}

// An unrecognised model still represents real money. It must be
// billed at the fallback rate AND reported, so the UI can flag the
// estimate as approximate instead of presenting a confident number.
func TestGetUsageCost_UnknownModelIsReported(t *testing.T) {
	t.Parallel()

	body := `{"data":[{"starting_at":"2026-08-25T00:00:00Z","ending_at":"2026-08-25T01:00:00Z","results":[
	  {"uncached_input_tokens":1000000,"cache_creation":{"ephemeral_1h_input_tokens":0,"ephemeral_5m_input_tokens":0},"cache_read_input_tokens":0,"output_tokens":0,"model":"claude-something-unreleased"}
	]}],"has_more":false}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := NewAdminClient("sk-ant-admin-test", srv.Client())
	c.baseURL = srv.URL

	start := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	got, err := c.GetUsageCost(context.Background(), start, start.Add(time.Hour))
	if err != nil {
		t.Fatalf("GetUsageCost: %v", err)
	}
	if got.TotalUSD <= 0 {
		t.Error("unknown model must still be billed, not dropped")
	}
	if len(got.UnpricedModels) != 1 || got.UnpricedModels[0] != "claude-something-unreleased" {
		t.Errorf("UnpricedModels = %v, want the unrecognised model reported",
			got.UnpricedModels)
	}
}

func TestGetUsageCost_DisabledWithoutKey(t *testing.T) {
	t.Parallel()
	c := NewAdminClient("", nil)
	_, err := c.GetUsageCost(context.Background(), time.Now(), time.Now())
	if err != ErrAdminDisabled {
		t.Errorf("err = %v, want ErrAdminDisabled so callers branch the "+
			"same way they do for GetCostReport", err)
	}
}
