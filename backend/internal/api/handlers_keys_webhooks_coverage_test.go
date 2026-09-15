package api

// Coverage-debt handler batch five: the customer key list and the
// webhook read/test surface. HandleTestWebhook is driven against a
// real local receiver so the assertion covers the whole loop: the
// synthetic payload is delivered, the outcome is reported, and the
// attempt lands in the delivery log the customer audits later. A
// test webhook that reports "delivered" without writing the log
// would pass any shallower test and quietly break the audit trail.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mesedi/backend/internal/store"
)

func TestAPIKeyAndWebhookEndpoints(t *testing.T) {
	const projectID = "proj_wh"
	h, st := newHandlerCoverageHarness(t, projectID)
	ctx := context.Background()

	t.Run("ListAPIKeys", func(t *testing.T) {
		if err := st.CreateAPIKey(ctx, &store.APIKey{
			KeyID: "key-cov-1", ProjectID: projectID,
			KeyHash: "hash-cov-1", KeyPrefix: "mesedi_sk_cov1",
			Name: "coverage key", CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed key: %v", err)
		}
		rec := httptest.NewRecorder()
		h.HandleListAPIKeys(rec, coverageRequest("GET", "/me/api-keys", "", projectID))
		body := decodeCoverageBody(t, rec)
		if rec.Code != 200 || body["count"].(float64) != 1 {
			t.Fatalf("list = %d %v, want the seeded key", rec.Code, body)
		}
	})

	// One receiver for the whole webhook flow: records that it was
	// hit and answers 200 so delivery succeeds on the first attempt.
	received := 0
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(receiver.Close)

	if err := st.CreateProjectWebhook(ctx, &store.ProjectWebhook{
		WebhookID: "wh-cov-1", ProjectID: projectID,
		Name: "coverage hook", URL: receiver.URL,
		Secret: "cov-secret", Enabled: true,
		RecurrenceMode: store.RecurrenceModeOff,
	}); err != nil {
		t.Fatalf("seed webhook: %v", err)
	}

	t.Run("ListWebhooks", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.HandleListWebhooks(rec, coverageRequest("GET", "/me/webhooks", "", projectID))
		body := decodeCoverageBody(t, rec)
		if rec.Code != 200 || body["count"].(float64) != 1 {
			t.Fatalf("list = %d %v, want the seeded webhook", rec.Code, body)
		}
	})

	t.Run("DeliveriesUnknownWebhookIs404", func(t *testing.T) {
		req := coverageRequest("GET", "/me/webhooks/wh-missing/deliveries", "", projectID)
		req.SetPathValue("id", "wh-missing")
		rec := httptest.NewRecorder()
		h.HandleListWebhookDeliveries(rec, req)
		if rec.Code != 404 {
			t.Fatalf("unknown webhook deliveries = %d, want 404", rec.Code)
		}
	})

	t.Run("TestWebhookDeliversAndLogs", func(t *testing.T) {
		req := coverageRequest("POST", "/me/webhooks/wh-cov-1/test", "", projectID)
		req.SetPathValue("id", "wh-cov-1")
		rec := httptest.NewRecorder()
		h.HandleTestWebhook(rec, req)
		body := decodeCoverageBody(t, rec)
		if rec.Code != 200 || body["ok"] != true || body["status"] != "delivered" {
			t.Fatalf("test webhook = %d %v, want a delivered outcome", rec.Code, body)
		}
		if received != 1 {
			t.Fatalf("receiver hit %d times, want exactly once", received)
		}
		// The attempt must land in the audit-facing delivery log.
		req = coverageRequest("GET", "/me/webhooks/wh-cov-1/deliveries", "", projectID)
		req.SetPathValue("id", "wh-cov-1")
		rec = httptest.NewRecorder()
		h.HandleListWebhookDeliveries(rec, req)
		body = decodeCoverageBody(t, rec)
		if rec.Code != 200 || body["count"].(float64) != 1 {
			t.Fatalf("delivery log = %d %v, want the recorded attempt", rec.Code, body)
		}
	})
}
