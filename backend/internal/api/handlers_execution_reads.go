// Execution read endpoints: list, get, digest, topology.
// Split out of handlers.go on 2026-09-12; every declaration moved verbatim.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/mesedi-ai/mesedi/backend/attest"
	"github.com/mesedi-ai/mesedi/backend/attest/events"
	"mesedi/backend/internal/store"
)

// HandleListExecutions returns the calling project's executions, sorted
// by started_at DESC. Supports `limit` (default 50, max 200), `offset`
// (default 0), and `q` (server-side case-insensitive substring search on
// execution_id + crash_signature, max 256 chars).
func (h *Handlers) HandleListExecutions(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	limit := parseIntQuery(r, "limit", 50, 1, 200)
	offset := parseIntQuery(r, "offset", 0, 0, 1_000_000)
	q := normalizeListQuery(r.URL.Query().Get("q"))

	execs, err := h.Store.ListExecutions(r.Context(), authProjectID, q, limit, offset)
	if err != nil {
		h.Logger.Error("list executions failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "list failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"executions": execs,
		"count":      len(execs),
		"limit":      limit,
		"offset":     offset,
		"q":          q,
	})
}

// HandleGetExecution returns a single execution + its events (sorted by
// sequence ASC). Cross-tenant access returns 404 to avoid leaking which
// execution IDs exist on other projects.
func (h *Handlers) HandleGetExecution(w http.ResponseWriter, r *http.Request) {
	executionID := r.PathValue("id")
	if executionID == "" {
		writeError(w, http.StatusBadRequest, "execution_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	exec, err := h.Store.GetExecution(r.Context(), executionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "execution not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if exec.ProjectID != authProjectID {
		writeError(w, http.StatusNotFound, "execution not found")
		return
	}

	evts, err := h.Store.ListEventsForExecution(r.Context(), executionID)
	if err != nil {
		h.Logger.Warn("list events failed (returning execution without events)",
			"execution_id", executionID,
			"error", err.Error(),
		)
		evts = nil
	}

	// Server-side token aggregation: if the SDK didn't explicitly PATCH
	// total_tokens_in / total_tokens_out on the terminal-status update,
	// derive them from the llm_call event payloads. Lets adapters
	// (LangChain, CrewAI, etc.) and bare emit_llm_call() callers show
	// accurate execution-level totals without requiring every SDK to
	// thread a running counter into the @wrap exit path.
	//
	// SDK-supplied values win when present (non-zero), a future SDK
	// slice can authoritatively report totals (e.g. accumulating
	// across streaming chunks the event payloads can't see) and the
	// dashboard will trust that report.
	if exec.TotalTokensIn == 0 && exec.TotalTokensOut == 0 {
		var sumIn, sumOut int
		for _, e := range evts {
			if e == nil || e.EventType != events.EventTypeLLMCall {
				continue
			}
			if e.Payload == nil {
				continue
			}
			// Payload is a json.RawMessage; cheaply parse just the
			// two numeric fields we need.
			var p struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			}
			if err := json.Unmarshal(e.Payload, &p); err == nil {
				sumIn += p.InputTokens
				sumOut += p.OutputTokens
			}
		}
		exec.TotalTokensIn = sumIn
		exec.TotalTokensOut = sumOut
	}

	// If this execution was clustered into a failure_group by the
	// detection pipeline, also fetch the group so the dashboard can
	// render a "Flagged by [class] / [signature]" banner with the
	// underlying reason + a deep-link back to the group's detail page.
	// Failure to load the group is non-fatal (the page still renders;
	// the banner just doesn't), so any store error is logged and the
	// response continues without the failure_group field.
	var failureGroup *store.FailureGroup
	if exec.FailureGroupID != nil && *exec.FailureGroupID != "" {
		fg, err := h.Store.GetFailureGroup(r.Context(), *exec.FailureGroupID)
		if err != nil {
			h.Logger.Warn("get failure_group failed (rendering execution without banner)",
				"execution_id", executionID,
				"group_id", *exec.FailureGroupID,
				"error", err.Error(),
			)
		} else {
			failureGroup = fg
		}
	}

	resp := map[string]any{
		"ok":        true,
		"execution": exec,
		"events":    evts,
	}
	if failureGroup != nil {
		resp["failure_group"] = failureGroup
	}
	writeJSON(w, http.StatusOK, resp)
}

// HandleGetExecutionDigest returns the canonical Merkle digest of one
// execution's event record, plus an inclusion proof when a leaf index
// is supplied via ?leaf=N.
//
// READ THE PACKAGE COMMENT ON internal/attest BEFORE DOCUMENTING THIS
// AS EVIDENCE. A digest Mesedi computes over a record Mesedi stores
// proves nothing to anyone who does not already trust Mesedi. This
// endpoint exists because it is the half we are entitled to compute:
// a canonical, reproducible summary a customer can recompute for
// themselves from the events we also return elsewhere. It becomes
// evidence only once the root is anchored somewhere outside our
// control, which is a separate service and a separate wave.
//
// Computed on read rather than stored. The digest is a pure function
// of the events, so persisting it would store a derivable value AND
// create a second thing that can drift from the record it summarises.
//
// RATE LIMIT TIER: standard authenticated read, same tier as
// GET /executions/{id}. No dedicated throttle and none warranted ,
// this endpoint issues no more work than the execution-detail read it
// sits beside (one execution lookup, one event list), adds no external
// call, and returns a bounded response. If a customer can afford to
// hammer the detail page they can afford to hammer this, and the
// answer to both is the same project-scoped limit.
//
// Revisit the moment the Verdifax anchor lands: THAT path costs money
// per submission to a public transparency log, and a per-project cap
// belongs on it rather than here.
func (h *Handlers) HandleGetExecutionDigest(w http.ResponseWriter, r *http.Request) {
	executionID := r.PathValue("id")
	if executionID == "" {
		writeError(w, http.StatusBadRequest, "execution_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	exec, err := h.Store.GetExecution(r.Context(), executionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "execution not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Same not-found-on-cross-project response as HandleGetExecution.
	// Deliberately not 403: telling a caller an execution exists in
	// somebody else's project is itself a disclosure.
	if exec.ProjectID != authProjectID {
		writeError(w, http.StatusNotFound, "execution not found")
		return
	}

	evts, err := h.Store.ListEventsForExecution(r.Context(), executionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list events failed: "+err.Error())
		return
	}

	digest, err := attest.Compute(executionID, evts)
	if err != nil {
		if errors.Is(err, attest.ErrNoEvents) {
			// 404 rather than an empty digest. An execution with no
			// events has no record to summarise, and returning the
			// empty-tree root would let "we have nothing" masquerade
			// as "here is the record".
			writeError(w, http.StatusNotFound, "execution has no events to digest")
			return
		}
		writeError(w, http.StatusInternalServerError, "digest failed: "+err.Error())
		return
	}

	resp := map[string]any{
		"ok":     true,
		"digest": digest,
	}

	// Optional inclusion proof for a single leaf. Present so a
	// customer can show a third party that ONE event was in the
	// record without disclosing the rest of the execution.
	if leafParam := r.URL.Query().Get("leaf"); leafParam != "" {
		idx, convErr := strconv.Atoi(leafParam)
		if convErr != nil {
			writeError(w, http.StatusBadRequest, "leaf must be an integer index")
			return
		}
		proof, proofErr := attest.Prove(digest, idx)
		if proofErr != nil {
			writeError(w, http.StatusBadRequest, proofErr.Error())
			return
		}
		resp["inclusion_proof"] = proof
	}

	writeJSON(w, http.StatusOK, resp)
}

// HandleGetExecutionTopology returns the parent + child tree of the
// supplied execution within the calling project. The
// response is a flat list of TopologyNode, ordered by depth ASC then
// started_at ASC, so the dashboard can render the tree without
// re-sorting. Cross-project edges are silently dropped at query
// time; an execution that the caller is not authorized to see does
// not appear in the response (and an attempt to seed the topology
// at a foreign execution_id returns an empty array, matching the
// 404-on-cross-tenant policy used by HandleGetExecution).
//
// Query param ?depth=N caps traversal in both directions (default
// 8, max 32). The cap defends against pathological parent chains
// and bounds the response size.
func (h *Handlers) HandleGetExecutionTopology(w http.ResponseWriter, r *http.Request) {
	executionID := r.PathValue("id")
	if executionID == "" {
		writeError(w, http.StatusBadRequest, "execution_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	depth := parseIntQuery(r, "depth", 8, 1, 32)
	nodes, err := h.Store.GetExecutionTopology(r.Context(), authProjectID, executionID, depth)
	if err != nil {
		h.Logger.Error("get execution topology failed",
			"execution_id", executionID,
			"error", err.Error(),
		)
		writeError(w, http.StatusInternalServerError, "topology query failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"nodes":      nodes,
		"count":      len(nodes),
		"depth":      depth,
		"seed_id":    executionID,
		"project_id": authProjectID,
	})
}

// HandleListExecutionsInFailureGroup returns the executions that belong
// to a given failure_group. Verifies cross-tenant access by first
// fetching the group and confirming group.project_id == auth project ,
// 404 if it doesn't match (don't leak group_id existence).
func (h *Handlers) HandleListExecutionsInFailureGroup(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("id")
	if groupID == "" {
		writeError(w, http.StatusBadRequest, "group_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	// Authorization: verify the group belongs to the caller's project.
	group, err := h.Store.GetFailureGroup(r.Context(), groupID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "failure group not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if group.ProjectID != authProjectID {
		writeError(w, http.StatusNotFound, "failure group not found")
		return
	}

	limit := parseIntQuery(r, "limit", 50, 1, 200)
	offset := parseIntQuery(r, "offset", 0, 0, 1_000_000)

	execs, err := h.Store.ListExecutionsByFailureGroup(r.Context(), groupID, limit, offset)
	if err != nil {
		h.Logger.Error("list executions by failure_group failed",
			"group_id", groupID,
			"error", err.Error(),
		)
		writeError(w, http.StatusInternalServerError, "list failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"executions": execs,
		"count":      len(execs),
		"limit":      limit,
		"offset":     offset,
		"group_id":   groupID,
	})
}
