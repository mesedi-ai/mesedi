package store

// Coverage-debt batch three: the webhook store surface behind alert
// escalation. GetProjectWebhook is what the test-delivery and delete
// paths authorize against, RecordWebhookDelivery is the audit trail
// operators read when an alert did not arrive, and the recurrence
// upsert is what keeps one noisy group from paging forever.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestWebhookConfigDeliveryAndRecurrence(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := OpenSQLite(":memory:", logger)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.CreateProject(ctx, &Project{
		ProjectID: "proj_wh", Name: "webhook lifecycle", Tier: "hobby",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	wh := &ProjectWebhook{
		WebhookID: "wh_1", ProjectID: "proj_wh", Name: "oncall",
		URL: "https://hooks.example-corp.com/x", Secret: "whsec_test",
		EnabledClasses: []string{"crashes", "sandbox_escape"}, Enabled: true,
		CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateProjectWebhook(ctx, wh); err != nil {
		t.Fatalf("create webhook: %v", err)
	}

	// Fetched scoped to its project; a foreign project id must read
	// as not-found, same cross-tenant posture as everything else.
	got, err := st.GetProjectWebhook(ctx, "wh_1", "proj_wh")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.URL != wh.URL || !got.Enabled || len(got.EnabledClasses) != 2 {
		t.Fatalf("round-trip = %+v, want the created webhook", got)
	}
	if _, err := st.GetProjectWebhook(ctx, "wh_1", "proj_other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project get = %v, want ErrNotFound", err)
	}

	// The per-project list carries it.
	list, err := st.ListProjectWebhooksForProject(ctx, "proj_wh")
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v (err %v), want exactly one", list, err)
	}

	// A delivery attempt lands in the audit trail and comes back
	// newest-first from the log the dashboard reads.
	httpStatus := 200
	if err := st.RecordWebhookDelivery(ctx, &WebhookDelivery{
		WebhookID: "wh_1", ProjectID: "proj_wh",
		FailureClass: "crashes", Signature: "crash:sig", GroupID: "grp-1",
		Attempt: 1, Status: "delivered", HTTPStatus: &httpStatus, DurationMs: 42,
	}); err != nil {
		t.Fatalf("record delivery: %v", err)
	}
	deliveries, err := st.ListDeliveriesForWebhook(ctx, "wh_1", 10)
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("deliveries = %v (err %v), want exactly one", deliveries, err)
	}
	if deliveries[0].Status != "delivered" || deliveries[0].HTTPStatus == nil || *deliveries[0].HTTPStatus != 200 {
		t.Fatalf("delivery row = %+v, want delivered with HTTP 200", deliveries[0])
	}

	// Recurrence state: unset reads as zero, the upsert sets it, a
	// second upsert moves it forward rather than erroring, which is
	// the whole point of the ON CONFLICT clause.
	first := time.Now().UTC().Truncate(time.Second)
	if err := st.UpsertWebhookRecurrenceLastFired(ctx, "wh_1", "grp-1", first); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	later := first.Add(time.Hour)
	if err := st.UpsertWebhookRecurrenceLastFired(ctx, "wh_1", "grp-1", later); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	lastFired, err := st.GetWebhookRecurrenceLastFired(ctx, "wh_1", "grp-1")
	if err != nil {
		t.Fatalf("get last fired: %v", err)
	}
	if !lastFired.Equal(later) {
		t.Fatalf("last_fired = %v, want the later upsert %v", lastFired, later)
	}
}
