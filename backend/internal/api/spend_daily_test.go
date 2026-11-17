package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
	"mesedi/backend/internal/pricing"
	"mesedi/backend/internal/store"
)

func TestSpendDailyPricesCallsAsRunsArePriced(t *testing.T) {
	st, err := store.OpenSQLite(":memory:", quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.CreateProject(ctx, &store.Project{ProjectID: "p", Name: "p", Tier: "hobby", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	day1 := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	if err := st.CreateExecution(ctx, &events.Execution{ExecutionID: "x", ProjectID: "p", Status: "completed", StartedAt: day1}); err != nil {
		t.Fatal(err)
	}
	call := func(i int, at time.Time, payload string) events.Event {
		return events.Event{EventID: fmt.Sprintf("e%d", i), ExecutionID: "x", EventType: events.EventTypeLLMCall,
			Sequence: i, Timestamp: at, Payload: json.RawMessage(payload)}
	}
	model := "claude-sonnet-4-5"
	if !pricing.IsKnownModel(model) {
		t.Skipf("%s is not in the price table", model)
	}
	if err := st.SaveEvents(ctx, []events.Event{
		call(1, day1, fmt.Sprintf(`{"model":%q,"provider":"anthropic","input_tokens":1000000,"output_tokens":0}`, model)),
		call(2, day1.Add(time.Hour), `{"model":"gpt-unknown-x","input_tokens":5,"output_tokens":5,"estimated_cost_usd":0.25}`),
		call(3, day1.AddDate(0, 0, 1), `{"model":"my-model","provider":"ollama","estimated_cost_usd":0.5}`),
		call(4, day1.AddDate(0, 0, 5), `{"model":"claude-x","estimated_cost_usd":9}`), // outside the range
	}); err != nil {
		t.Fatal(err)
	}
	h := &Handlers{Store: st, Logger: quietLogger()}
	req := httptest.NewRequest("GET", "/me/spend/daily?from=2026-11-10&to=2026-11-12", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyProjectID, "p"))
	rec := httptest.NewRecorder()
	h.HandleSpendDaily(rec, req)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Days []spendDay `json:"days"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Days) != 3 || got.Days[0].Date != "2026-11-10" || got.Days[2].Date != "2026-11-12" {
		t.Fatalf("days %+v", got.Days)
	}
	want := pricing.ComputeLLMCost(model, 1000000, 0)
	if d := got.Days[0]; d.Providers["anthropic"] != want || d.Providers["openai"] != 0.25 || d.Calls != 2 {
		t.Errorf("day 1 = %+v; want anthropic %v at list price and openai 0.25 from the SDK estimate", d, want)
	}
	if d := got.Days[1]; d.Providers["ollama"] != 0.5 || d.TotalUSD != 0.5 {
		t.Errorf("day 2 = %+v", d)
	}
	if d := got.Days[2]; d.Calls != 0 || d.TotalUSD != 0 {
		t.Errorf("a quiet day must be listed as zero, not left out: %+v", d)
	}

	for _, q := range []string{"from=2026-11-10", "from=bad&to=2026-11-10", "from=2026-11-10&to=2026-11-01", "from=2026-01-01&to=2026-12-31"} {
		req := httptest.NewRequest("GET", "/me/spend/daily?"+q, nil)
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyProjectID, "p"))
		rec := httptest.NewRecorder()
		h.HandleSpendDaily(rec, req)
		if rec.Code != 400 {
			t.Errorf("%s gave %d", q, rec.Code)
		}
	}
}

func TestCallProvider(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"Anthropic", ""}: "anthropic", {"", "claude-opus-5-5"}: "anthropic", {"", "gpt-5"}: "openai",
		{"", "o3-mini"}: "openai", {"", "gemini-3-pro"}: "gemini", {"", "command-r"}: "cohere", {"", "llama3"}: "unknown",
	} {
		if got := callProvider(in[0], in[1]); got != want {
			t.Errorf("%v: %s, want %s", in, got, want)
		}
	}
}
