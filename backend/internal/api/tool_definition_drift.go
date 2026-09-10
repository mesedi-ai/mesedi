package api

// Definition drift, the mcp-pin gap closed: a tool whose DECLARED
// input schema changes while its description stays byte-identical.
// Sibling of tool_description_drift.go with the same baseline
// discipline (history excludes the calling execution; rows without
// the field are skipped, never counted, so pre-upgrade traffic
// cannot form a baseline that indicts every upgraded client).
//
// Coverage is honest and stated: the SDKs compute input_schema_hash
// only where a declared schema exists, MCP tools and framework
// adapters. A plain decorated function declares no schema, sends
// nothing, and this detector correctly has no opinion about it.

import (
	"context"

	"mesedi/backend/internal/detectors"
)

const toolDefinitionHistoryLimit = 100

func (h *Handlers) detectToolDefinitionDrift(
	ctx context.Context,
	projectID, executionID, toolName string,
	thresholds detectors.ToolSchemaDriftThresholds,
) (signature string, detected bool) {
	current, err := h.Store.ListToolInputSchemaHashes(ctx, projectID, toolName, "", 1)
	if err != nil || len(current) == 0 {
		// No declared schema on this call: pre-upgrade SDK, or a
		// tool with nothing to declare. Nothing to compare, not a
		// fault.
		return "", false
	}

	history, err := h.Store.ListToolInputSchemaHashes(
		ctx, projectID, toolName, executionID, toolDefinitionHistoryLimit,
	)
	if err != nil {
		return "", false
	}
	counts := map[string]int{}
	for _, hash := range history {
		counts[hash]++
	}

	return detectors.DetectToolDefinitionDrift(toolName, current[0], counts, thresholds)
}
