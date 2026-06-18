// Admin "Mark key compromised" action (#257).
//
// POST /admin/api-keys/{id}/mark-compromised
//
// Workflow: customer emails security@mesedi.ai about a leaked
// project-admin key. Operator confirms the report and clicks this
// endpoint with the key's id. The endpoint:
//
//	1. Validates the key has scope=admin. Read/write keys are
//	   refused with 422 because Mesedi does not get involved in
//	   read/write key rotation; the customer's own admin handles
//	   that via /app/api-keys.
//
//	2. Records an abuse_signal with kind=key_compromised so the
//	   audit trail is complete.
//
//	3. Suspends the affected project. A suspended project rejects
//	   every authenticated request (including from the leaked key
//	   itself), so the four-step Terms commitment is satisfied
//	   without escalating to project deletion.
//
//	4. Revokes the leaked key. Belt-and-suspenders since the project
//	   suspension already blocks it.
//
//	5. Builds and returns a recent-use report covering the last 90
//	   days of executions (#255 attribution) plus request_log rows
//	   (#256). The operator downloads this report and attaches it
//	   to the reply email to the customer.
//
// What is NOT done here:
//
//   - No customer email is sent automatically. The operator owns the
//     reply (solo-dev posture; no automated customer touchpoints).
//
//   - No project deletion. Suspension is sufficient because a
//     suspended project rejects all traffic. Keeping the project
//     reversible matches the existing Terms language.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"mesedi/backend/internal/events"
	"mesedi/backend/internal/store"
)

// adminMarkKeyCompromisedRequest is the JSON body for
// POST /admin/api-keys/{id}/mark-compromised. CustomerNote captures
// what the customer told the operator (name, email, when they
// reported, optional one-liner). Stored on the abuse_signal's detail
// field so the audit trail is searchable.
type adminMarkKeyCompromisedRequest struct {
	CustomerNote string `json:"customer_note"`
}

// adminMarkKeyCompromisedResponse is the operator's downloadable
// payload. Returned inline in the HTTP response; the dashboard
// converts it to a JSON file the operator can attach to their reply
// email. Shape is intentionally flat + JSON-friendly so the customer
// can read it directly without specialized tools.
type adminMarkKeyCompromisedResponse struct {
	OK              bool                       `json:"ok"`
	KeyID           string                     `json:"key_id"`
	KeyPrefix       string                     `json:"key_prefix"`
	ProjectID       string                     `json:"project_id"`
	SignalID        string                     `json:"signal_id"`
	SuspendedAt     string                     `json:"suspended_at"`
	WindowStart     string                     `json:"window_start"`
	WindowEnd       string                     `json:"window_end"`
	ExecutionCount  int                        `json:"execution_count"`
	RequestCount    int                        `json:"request_count"`
	Executions      []reportExecution          `json:"executions"`
	RequestLog      []reportRequest            `json:"request_log"`
	GeneratedAt     string                     `json:"generated_at"`
}

type reportExecution struct {
	ExecutionID    string `json:"execution_id"`
	Status         string `json:"status"`
	StartedAt      string `json:"started_at"`
	EndedAt        string `json:"ended_at,omitempty"`
	DurationMs     int64  `json:"duration_ms,omitempty"`
	CrashSignature string `json:"crash_signature,omitempty"`
	SDKLanguage    string `json:"sdk_language,omitempty"`
}

type reportRequest struct {
	ReceivedAt string `json:"received_at"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	StatusCode int    `json:"status_code"`
	IPAddress  string `json:"ip_address,omitempty"`
}

// recentUseWindow is how far back the recent-use report looks. Mirrors
// the request_log retention window so the report covers every row
// the table still has.
const recentUseWindow = 90 * 24 * time.Hour

// HandleAdminMarkKeyCompromised wires the four-step workflow.
func (h *Handlers) HandleAdminMarkKeyCompromised(w http.ResponseWriter, r *http.Request) {
	keyID := r.PathValue("id")
	if keyID == "" {
		writeError(w, http.StatusBadRequest, "missing key id")
		return
	}

	var req adminMarkKeyCompromisedRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, context.Canceled) {
		if err.Error() != "EOF" {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
	}

	// Step 1: validate scope.
	key, err := h.Store.GetAPIKeyByID(r.Context(), keyID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "api key not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "lookup api key: "+err.Error())
		return
	}
	if key.Scope != "admin" {
		writeError(w, http.StatusUnprocessableEntity,
			"this action only applies to admin-scope keys; read/write keys are self-service via /app/api-keys")
		return
	}

	now := time.Now().UTC()
	windowStart := now.Add(-recentUseWindow)

	// Step 2: record abuse signal. detail captures the operator's
	// note so the audit trail can be searched later.
	detail, _ := json.Marshal(map[string]any{
		"key_id":        key.KeyID,
		"key_prefix":    key.KeyPrefix,
		"key_name":      key.Name,
		"customer_note": req.CustomerNote,
		"reported_at":   now.Format(time.RFC3339),
	})
	signalID := fmt.Sprintf("abuse_%d_key_compromised", now.UnixNano())
	signal := &store.AbuseSignal{
		SignalID:   signalID,
		ProjectID:  key.ProjectID,
		Kind:       "key_compromised",
		Severity:   "critical",
		Detail:     string(detail),
		DetectedAt: now,
	}
	if err := h.Store.CreateAbuseSignal(r.Context(), signal); err != nil {
		writeError(w, http.StatusInternalServerError,
			"record abuse signal: "+err.Error())
		return
	}

	// Step 3: suspend the project + stamp the abuse signal's
	// suspended_at. The auth middleware refuses authenticated
	// requests for the project from this point forward.
	if err := h.Store.MarkAbuseSignalSuspended(r.Context(),
		signalID, key.ProjectID, "key_compromised", now); err != nil {
		writeError(w, http.StatusInternalServerError,
			"suspend project: "+err.Error())
		return
	}

	// Step 4: revoke the key. Project suspension already blocks
	// every key on the project, so this is belt-and-suspenders
	// against a future un-suspension that would otherwise leave
	// the leaked key valid.
	if err := h.Store.DeleteAPIKeyByID(r.Context(), keyID); err != nil {
		// The project is already suspended, so the attacker cannot
		// use the key. Log and continue rather than failing the
		// whole action; the operator can DELETE the key directly
		// via the existing endpoint if this branch fires.
		h.Logger.Warn("mark_key_compromised: revoke after suspend failed",
			"key_id", keyID,
			"project_id", key.ProjectID,
			"error", err.Error())
	}

	// Step 5: build the recent-use report. Errors here do not roll
	// back steps 2/3/4 because the customer is already protected;
	// log and return what we have.
	execs, err := h.Store.ListExecutionsByAPIKey(r.Context(), keyID, windowStart, now, 1000)
	if err != nil {
		h.Logger.Warn("mark_key_compromised: list executions failed",
			"key_id", keyID, "error", err.Error())
		execs = nil
	}
	reqLog, err := h.Store.ListRequestLog(r.Context(), store.RequestLogFilter{
		APIKeyID: keyID,
		T1:       windowStart,
		T2:       now,
		Limit:    5000,
	})
	if err != nil {
		h.Logger.Warn("mark_key_compromised: list request log failed",
			"key_id", keyID, "error", err.Error())
		reqLog = nil
	}

	resp := buildRecentUseReport(
		key, signalID, now, windowStart, execs, reqLog,
	)
	writeJSON(w, http.StatusOK, resp)

	h.Logger.Info("mark_key_compromised: completed",
		"key_id", keyID,
		"project_id", key.ProjectID,
		"signal_id", signalID,
		"execution_count", resp.ExecutionCount,
		"request_count", resp.RequestCount,
	)
}

// buildRecentUseReport materializes the operator's downloadable
// payload. Pulled out of the handler to keep the workflow readable
// and to make the report shape unit-testable in isolation.
func buildRecentUseReport(
	key *store.APIKey,
	signalID string,
	now, windowStart time.Time,
	execs []*events.Execution,
	reqLog []*store.RequestLog,
) adminMarkKeyCompromisedResponse {
	out := adminMarkKeyCompromisedResponse{
		OK:             true,
		KeyID:          key.KeyID,
		KeyPrefix:      key.KeyPrefix,
		ProjectID:      key.ProjectID,
		SignalID:       signalID,
		SuspendedAt:    now.Format(time.RFC3339),
		WindowStart:    windowStart.Format(time.RFC3339),
		WindowEnd:      now.Format(time.RFC3339),
		ExecutionCount: len(execs),
		RequestCount:   len(reqLog),
		Executions:     make([]reportExecution, 0, len(execs)),
		RequestLog:     make([]reportRequest, 0, len(reqLog)),
		GeneratedAt:    now.Format(time.RFC3339),
	}
	for _, e := range execs {
		row := reportExecution{
			ExecutionID:    e.ExecutionID,
			Status:         string(e.Status),
			StartedAt:      e.StartedAt.UTC().Format(time.RFC3339),
			DurationMs:     e.DurationMs,
			CrashSignature: e.CrashSignature,
			SDKLanguage:    e.SDKLanguage,
		}
		if e.EndedAt != nil {
			row.EndedAt = e.EndedAt.UTC().Format(time.RFC3339)
		}
		out.Executions = append(out.Executions, row)
	}
	for _, r := range reqLog {
		out.RequestLog = append(out.RequestLog, reportRequest{
			ReceivedAt: r.ReceivedAt.UTC().Format(time.RFC3339),
			Method:     r.Method,
			Path:       r.Path,
			StatusCode: r.StatusCode,
			IPAddress:  r.IPAddress,
		})
	}
	return out
}
