package api

// First-seen tool contracts, and the new-tool notice.
//
// The history checks (tool_description_drift.go, tool_definition_drift.go)
// need ten calls and a majority, so a tool whose description changes on
// its second call goes unseen, and once the changed text is the majority
// it is the baseline. Pins close that for tools an operator approved.
// This closes it for every other tool: the first description and the
// first declared-schema hash Mesedi records for a tool are kept
// (tool_first_seen, migration 066) and any run whose calls differ is
// reported, from the first such run.
//
// The first-seen value is set once, from the oldest call Mesedi holds,
// and never moves. To accept a changed tool an operator pins the new
// version; the pin then replaces this baseline for that half of the
// contract. The ten-call history check, its setting and pins all stay.
//
// The first run that uses a tool Mesedi has never seen in the project
// gets an info-level new_tool notice, linked to the run without ever
// becoming its classification.

import (
	"context"
	"net/http"

	"mesedi/backend/internal/detectors"
	"mesedi/backend/internal/store"
)

// recordToolsFirstSeen makes sure every tool in the run has its
// first-seen rows, raising the new_tool notice for any tool whose first
// call in the project is in this run. It runs over every tool before
// any drift check, because the drift loop stops at the first finding
// and a tool it never reached would otherwise miss its notice.
func (h *Handlers) recordToolsFirstSeen(
	r *http.Request,
	executionID, projectID string,
	toolNames []string,
) map[string]map[string]string {
	// Lives for one run's completion: max one entry per tool in the run.
	out := make(map[string]map[string]string, len(toolNames))
	for _, toolName := range toolNames {
		firstSeen, isNew := h.ensureToolFirstSeen(r.Context(), projectID, executionID, toolName)
		out[toolName] = firstSeen
		if !isNew {
			continue
		}
		created, gErr := h.Store.GroupNewTool(r.Context(), executionID, projectID, toolName)
		if gErr != nil {
			h.Logger.Warn("new-tool notice grouping failed (continuing)",
				"execution_id", executionID,
				"tool_name", toolName,
				"error", gErr.Error())
		}
		h.maybeFireWebhook(r, projectID, store.FailureClassNewTool, toolName, created, gErr)
	}
	return out
}

// ensureToolFirstSeen returns kind -> hash for the tool, filling any
// missing row from the oldest stored calls. isNew is true only for the
// run that wrote the tool's "tool" row and holds its first call, so
// two runs finishing together raise one notice between them.
//
// Errors cost the signal, never the request: a failed read returns
// what is known, and the drift check then has nothing to compare.
func (h *Handlers) ensureToolFirstSeen(
	ctx context.Context,
	projectID, executionID, toolName string,
) (firstSeen map[string]string, isNew bool) {
	firstSeen, err := h.Store.GetToolFirstSeen(ctx, projectID, toolName)
	if err != nil {
		h.Logger.Warn("read tool first-seen failed",
			"project_id", projectID, "tool_name", toolName, "error", err.Error())
		return map[string]string{}, false
	}
	_, haveTool := firstSeen[store.ToolFirstSeenKindTool]
	_, haveDesc := firstSeen[store.ToolFirstSeenKindDescription]
	_, haveDef := firstSeen[store.ToolFirstSeenKindDefinition]
	if haveTool && haveDesc && haveDef {
		return firstSeen, false
	}

	earliest, err := h.Store.EarliestToolContract(ctx, projectID, toolName)
	if err != nil {
		h.Logger.Warn("read earliest tool contract failed",
			"project_id", projectID, "tool_name", toolName, "error", err.Error())
		return firstSeen, false
	}

	record := func(kind, hash, fromExecution string) bool {
		inserted, err := h.Store.RecordToolFirstSeen(ctx, projectID, toolName, kind, hash, fromExecution)
		if err != nil {
			h.Logger.Warn("record tool first-seen failed",
				"project_id", projectID, "tool_name", toolName, "kind", kind, "error", err.Error())
			return false
		}
		return inserted
	}

	if !haveTool {
		origin := earliest.ExecutionID
		if origin == "" {
			origin = executionID
		}
		isNew = record(store.ToolFirstSeenKindTool, "", origin) && origin == executionID
	}
	if !haveDesc {
		if hash := detectors.DescriptionHash(earliest.Description); hash != "" {
			record(store.ToolFirstSeenKindDescription, hash, earliest.DescriptionExecutionID)
		}
	}
	if !haveDef && earliest.SchemaHash != "" {
		record(store.ToolFirstSeenKindDefinition, earliest.SchemaHash, earliest.SchemaExecutionID)
	}

	// Read back rather than trust what was written: when another run
	// wrote a row first, its value is the baseline, not ours.
	if again, err := h.Store.GetToolFirstSeen(ctx, projectID, toolName); err == nil {
		firstSeen = again
	}
	return firstSeen, isNew
}

// detectFirstSeenContractDrift compares this run's description and
// declared schema for the tool against its first-seen values,
// description first (the tool-poisoning reading outranks a schema
// change). A pinned half is skipped: the pin is the operator's chosen
// baseline and already checked it.
func (h *Handlers) detectFirstSeenContractDrift(
	ctx context.Context,
	executionID, toolName string,
	firstSeen, pins map[string]string,
) (signature string, detected bool) {
	if len(firstSeen) == 0 {
		return "", false
	}
	current, err := h.Store.LatestToolContractInExecution(ctx, executionID, toolName)
	if err != nil {
		return "", false
	}
	checks := []struct {
		kind    string
		current string
	}{
		{detectors.PinKindDescription, detectors.DescriptionHash(current.Description)},
		{detectors.PinKindDefinition, current.SchemaHash},
	}
	for _, c := range checks {
		if _, pinned := pins[c.kind]; pinned {
			continue
		}
		if sig, fired := detectors.DetectFirstSeenContractDrift(
			toolName, c.kind, c.current, firstSeen[c.kind],
		); fired {
			return sig, true
		}
	}
	return "", false
}
