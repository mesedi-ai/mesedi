package api

// Coverage-debt handler batch one: the per-detector configuration
// endpoints. Each get/set pair is driven over a real in-memory store
// so the round trip, the validation bounds, and the tier caps are all
// pinned against the actual columns, not stubs. The bounds ARE the
// contract: every one of them exists because the unbounded version
// was a storage-abuse or typo vector, so a test that only checks the
// happy path would miss the half the handler is for.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
	"mesedi/backend/internal/store"
	"mesedi/backend/internal/webhooks"
)

// newHandlerCoverageHarness stands up Handlers over a real in-memory
// SQLite store with one hobby project owned by owner@example.com.
// The project deliberately has no tenant_id so resolveCallerRole
// takes the documented legacy-admin path and role-gated handlers are
// reachable without standing up the whole membership surface.
func newHandlerCoverageHarness(t *testing.T, projectID string) (*Handlers, *store.SQLiteStore) {
	t.Helper()
	st, err := store.OpenSQLite(":memory:", quietLogger())
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.CreateProject(context.Background(), &store.Project{
		ProjectID: projectID, Name: "coverage " + projectID, Tier: "hobby",
		OwnerUserID: "owner@example.com", OwnerEmail: "owner@example.com",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	h := &Handlers{
		Logger:        quietLogger(),
		Store:         st,
		HaltSubs:      NewHaltSubscribers(),
		WebhookClient: webhooks.DefaultHTTPClient(),
	}
	t.Cleanup(h.DrainDispatches)
	return h, st
}

// coverageRequest builds a request already carrying the project auth
// context, mirroring what the auth middleware attaches in production.
func coverageRequest(method, target, body, projectID string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	return r.WithContext(context.WithValue(r.Context(), ctxKeyProjectID, projectID))
}

func decodeCoverageBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return out
}

func seedCoverageExecution(t *testing.T, st *store.SQLiteStore, projectID, execID string, status events.ExecutionStatus) {
	t.Helper()
	if err := st.CreateExecution(context.Background(), &events.Execution{
		ExecutionID: execID, ProjectID: projectID,
		Status: status, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create execution %s: %v", execID, err)
	}
}

func TestDetectorConfigEndpointsRoundTripAndBounds(t *testing.T) {
	const projectID = "proj_cfg"
	h, _ := newHandlerCoverageHarness(t, projectID)

	do := func(fn http.HandlerFunc, method, target, body string) (*httptest.ResponseRecorder, map[string]any) {
		rec := httptest.NewRecorder()
		fn(rec, coverageRequest(method, target, body, projectID))
		return rec, decodeCoverageBody(t, rec)
	}

	t.Run("NoProjectContextIs401", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.HandleGetProviderIncidentConfig(rec, httptest.NewRequest("GET", "/me/provider-incident-config", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("code = %d, want 401 without auth context", rec.Code)
		}
	})

	t.Run("ProviderIncident", func(t *testing.T) {
		rec, body := do(h.HandleGetProviderIncidentConfig, "GET", "/me/provider-incident-config", "")
		if rec.Code != 200 || body["min_tenants"].(float64) != 2 {
			t.Fatalf("default = %d %v, want the documented default of 2", rec.Code, body)
		}
		for _, bad := range []string{`{"min_tenants":0}`, `{"min_tenants":1001}`} {
			if rec, _ := do(h.HandleSetProviderIncidentConfig, "PUT", "/me/provider-incident-config", bad); rec.Code != 400 {
				t.Fatalf("set %s = %d, want 400", bad, rec.Code)
			}
		}
		if rec, _ := do(h.HandleSetProviderIncidentConfig, "PUT", "/me/provider-incident-config", `{"min_tenants":3}`); rec.Code != 200 {
			t.Fatalf("valid set = %d, want 200", rec.Code)
		}
		if _, body := do(h.HandleGetProviderIncidentConfig, "GET", "/me/provider-incident-config", ""); body["min_tenants"].(float64) != 3 {
			t.Fatalf("read back = %v, want the value just set", body["min_tenants"])
		}
	})

	t.Run("TimeBudget", func(t *testing.T) {
		rec, body := do(h.HandleGetTimeBudgetConfig, "GET", "/me/time-budget-config", "")
		if rec.Code != 200 || body["tier"] != "hobby" || body["max_ms_for_tier"].(float64) != 300_000 {
			t.Fatalf("get = %d %v, want the hobby tier cap surfaced", rec.Code, body)
		}
		// Three distinct refusals: zero, over the 24h wire bound, and
		// within the wire bound but over the hobby tier cap.
		for _, bad := range []string{`{"threshold_ms":0}`, `{"threshold_ms":90000000}`, `{"threshold_ms":400000}`} {
			if rec, _ := do(h.HandleSetTimeBudgetConfig, "PUT", "/me/time-budget-config", bad); rec.Code != 400 {
				t.Fatalf("set %s = %d, want 400", bad, rec.Code)
			}
		}
		if rec, _ := do(h.HandleSetTimeBudgetConfig, "PUT", "/me/time-budget-config", `{"threshold_ms":30000}`); rec.Code != 200 {
			t.Fatalf("valid set = %d, want 200", rec.Code)
		}
		if _, body := do(h.HandleGetTimeBudgetConfig, "GET", "/me/time-budget-config", ""); body["threshold_ms"].(float64) != 30_000 {
			t.Fatalf("read back = %v, want 30000", body["threshold_ms"])
		}
	})

	t.Run("CostVelocityAbsolute", func(t *testing.T) {
		for _, bad := range []string{`{"threshold_usd":0.001}`, `{"threshold_usd":20000}`} {
			rec := httptest.NewRecorder()
			h.HandleSetCostVelocityConfig(rec, coverageRequest("PUT", "/me/cost-velocity-config", bad, projectID))
			if rec.Code != 400 {
				t.Fatalf("set %s = %d, want 400: the floor is the storage-abuse guard", bad, rec.Code)
			}
		}
		if rec, body := do(h.HandleSetCostVelocityConfig, "PUT", "/me/cost-velocity-config", `{"threshold_usd":0.5}`); rec.Code != 200 || body["threshold_usd"].(float64) != 0.5 {
			t.Fatalf("valid set = %d %v, want the echo", rec.Code, body)
		}
	})

	t.Run("CostVelocityRate", func(t *testing.T) {
		rec, body := do(h.HandleGetCostVelocityRateConfig, "GET", "/me/cost-velocity-rate-config", "")
		if rec.Code != 200 || body["threshold_usd_per_min"].(float64) != 5 || body["window_minutes"].(float64) != 5 {
			t.Fatalf("defaults = %d %v, want {5.00, 5}", rec.Code, body)
		}
		for _, bad := range []string{
			`{"threshold_usd_per_min":0.05,"window_minutes":5}`,
			`{"threshold_usd_per_min":20000,"window_minutes":5}`,
			`{"threshold_usd_per_min":2.5,"window_minutes":0}`,
			`{"threshold_usd_per_min":2.5,"window_minutes":61}`,
		} {
			if rec, _ := do(h.HandleSetCostVelocityRateConfig, "PUT", "/me/cost-velocity-rate-config", bad); rec.Code != 400 {
				t.Fatalf("set %s = %d, want 400", bad, rec.Code)
			}
		}
		if rec, _ := do(h.HandleSetCostVelocityRateConfig, "PUT", "/me/cost-velocity-rate-config", `{"threshold_usd_per_min":2.5,"window_minutes":10}`); rec.Code != 200 {
			t.Fatalf("valid set = %d, want 200", rec.Code)
		}
		if _, body := do(h.HandleGetCostVelocityRateConfig, "GET", "/me/cost-velocity-rate-config", ""); body["threshold_usd_per_min"].(float64) != 2.5 || body["window_minutes"].(float64) != 10 {
			t.Fatalf("read back = %v, want the pair just set", body)
		}
	})

	t.Run("ToolReturnValue", func(t *testing.T) {
		rec, body := do(h.HandleGetToolReturnValueConfig, "GET", "/me/tool-return-value-config", "")
		if rec.Code != 200 || body["max_bytes_for_tier"].(float64) != 4096 {
			t.Fatalf("get = %d %v, want the hobby 4KB cap surfaced", rec.Code, body)
		}
		// Zero, over the 1MB wire cap, and over the hobby tier cap.
		for _, bad := range []string{`{"max_bytes":0}`, `{"max_bytes":2097152}`, `{"max_bytes":16384}`} {
			if rec, _ := do(h.HandleSetToolReturnValueConfig, "PUT", "/me/tool-return-value-config", bad); rec.Code != 400 {
				t.Fatalf("set %s = %d, want 400", bad, rec.Code)
			}
		}
		if rec, _ := do(h.HandleSetToolReturnValueConfig, "PUT", "/me/tool-return-value-config", `{"max_bytes":2048}`); rec.Code != 200 {
			t.Fatalf("valid set = %d, want 200", rec.Code)
		}
		rec, body = do(h.HandleGetToolReturnValueStats, "GET", "/me/tool-return-value-stats", "")
		if rec.Code != 200 || body["total_calls"].(float64) != 0 || body["max_bytes"].(float64) != 2048 {
			t.Fatalf("stats = %d %v, want empty-window zeros against the configured cap", rec.Code, body)
		}
	})

	t.Run("ConfigFallbackStats", func(t *testing.T) {
		for _, bad := range []string{"0", "169", "abc"} {
			rec := httptest.NewRecorder()
			h.HandleGetConfigFallbackStats(rec, coverageRequest("GET", "/me/config-fallback-stats?window_hours="+bad, "", projectID))
			if rec.Code != 400 {
				t.Fatalf("window_hours=%s = %d, want 400", bad, rec.Code)
			}
		}
		rec, body := do(h.HandleGetConfigFallbackStats, "GET", "/me/config-fallback-stats", "")
		if rec.Code != 200 || body["window_hours"].(float64) != 24 {
			t.Fatalf("default window = %d %v, want 24", rec.Code, body)
		}
		if _, body := do(h.HandleGetConfigFallbackStats, "GET", "/me/config-fallback-stats?window_hours=48", ""); body["window_hours"].(float64) != 48 {
			t.Fatalf("explicit window = %v, want 48", body["window_hours"])
		}
	})
}
