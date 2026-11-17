package api

// GET /executions/{id}/export: the single-run file (attest.RunExport).
//
// One agent run's reports as stored, their chain records as received,
// the run's recorded chain verdict and the project's signing keys, so
// mesedi-verify can check the run against the hourly export without
// trusting Mesedi (owner decision, 17 November 2026). Same project
// scoping and not-found answer as GET /executions/{id}.

import (
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest"
	"mesedi/backend/internal/store"
)

func (h *Handlers) HandleRunExport(w http.ResponseWriter, r *http.Request) {
	projectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	executionID := r.PathValue("id")
	exec, err := h.Store.GetExecution(r.Context(), executionID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && exec.ProjectID != projectID) {
		writeError(w, http.StatusNotFound, "execution not found")
		return
	}
	if err != nil {
		h.Logger.Error("run export: get execution failed", "execution_id", executionID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not read the run")
		return
	}

	all, err := h.Store.ListEventsForExecution(r.Context(), executionID)
	if err != nil {
		h.Logger.Error("run export: list events failed", "execution_id", executionID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not read the run's reports")
		return
	}
	covered, err := h.Store.ListDigestEventsForExecution(r.Context(), executionID)
	if err != nil {
		h.Logger.Error("run export: list anchored events failed", "execution_id", executionID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not read the run's reports")
		return
	}
	// Request-scoped maps: max one entry per report of this one run.
	anchored := make(map[string]bool, len(covered))
	for _, e := range covered {
		anchored[e.EventID] = true
	}
	rows, err := h.Store.ListReportChainRows(r.Context(), executionID)
	if err != nil {
		h.Logger.Error("run export: list chain failed", "execution_id", executionID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not read the run's chain")
		return
	}
	keys, err := h.Store.ListReportSigningKeys(r.Context(), projectID)
	if err != nil {
		h.Logger.Error("run export: list keys failed", "project_id", projectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not read the signing keys")
		return
	}

	out := attest.RunExport{
		Format:      attest.RunExportFormatV1,
		ProjectID:   projectID,
		ExecutionID: executionID,
		GeneratedAt: time.Now().UTC(),
		Events:      make([]attest.RunExportEvent, 0, len(all)),
		ReportChain: make([]attest.RunExportLink, 0, len(rows)),
	}
	stored := make(map[string]bool, len(all))
	for _, e := range all {
		if e == nil {
			continue
		}
		stored[e.EventID] = true
		out.Events = append(out.Events, attest.RunExportEvent{
			EventID:        e.EventID,
			EventType:      string(e.EventType),
			Sequence:       e.Sequence,
			Timestamp:      e.Timestamp,
			DurationMs:     e.DurationMs,
			PayloadB64:     base64.StdEncoding.EncodeToString(e.Payload),
			AfterAnchoring: !anchored[e.EventID],
		})
	}
	for _, row := range rows {
		// A row whose batch was then refused is not part of the record;
		// the completion check ignores it, and so does the file.
		if !stored[row.EventID] {
			continue
		}
		out.ReportChain = append(out.ReportChain, attest.RunExportLink{
			EventID: row.EventID, Prev: row.PrevHash, Hash: row.Hash,
			PayloadSHA256: row.PayloadSHA256, KeyID: row.KeyID, Signature: row.Signature,
		})
	}
	if row, err := h.Store.GetReportChainStatus(r.Context(), executionID); err == nil {
		out.ReportChainStatus, out.ReportChainDetail = row.Status, row.Detail
		if row.EndHash != "" {
			out.ReportChainEnd = &attest.RunExportEnd{Hash: row.EndHash, KeyID: row.EndKeyID, Signature: row.EndSignature}
		}
	}
	for _, k := range keys {
		out.SigningKeys = append(out.SigningKeys, attest.RunExportKey{
			KeyID: k.KeyID, PublicKey: k.PublicKey, RevokedAt: k.RevokedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
