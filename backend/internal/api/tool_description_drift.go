package api

// Description-drift helper for the tool_schema_drift detector.
//
// Lives in its own file rather than inline in handlers.go for two
// reasons: handlers.go is already 2,000+ lines and flagged by the
// audit's file-length check, and the query-then-detect shape here is
// testable on its own without standing up a request.
//
// See internal/detectors/tool_schema_drift.go for why description
// drift is a separate signal from return-shape drift rather than a
// wider hash over both.

import (
	"context"

	"mesedi/backend/internal/detectors"
)

// toolDescriptionHistoryLimit matches the return-shape history depth.
// Kept identical on purpose: the two signals share a MinHistoryCalls
// floor and a majority rule, so giving them different window sizes
// would make "10 calls of history" mean two different things
// depending on which half of the tool contract you were asking about.
const toolDescriptionHistoryLimit = 100

// detectToolDescriptionDrift reports whether this tool's description
// has changed away from a stable majority baseline.
//
// Returns ("", false) on any error or missing data rather than
// surfacing the error. Drift detection is advisory: a failed history
// query should cost a signal, never a customer's ingest request. The
// caller logs nothing here for the same reason the shape path does
// not, an absent baseline is the normal case for a new tool.
func (h *Handlers) detectToolDescriptionDrift(
	ctx context.Context,
	projectID, executionID, toolName string,
	thresholds detectors.ToolSchemaDriftThresholds,
) (signature string, detected bool) {
	// Current description: the most recent tool_call for this tool
	// project-wide, which includes the execution being evaluated.
	// Mirrors how currentShape is obtained in the caller.
	current, err := h.Store.ListToolDescriptions(ctx, projectID, toolName, "", 1)
	if err != nil || len(current) == 0 {
		// No description on this call. Either the customer is on an
		// SDK predating tool_description, or the tool genuinely has
		// none. Both are "nothing to compare", not a fault.
		return "", false
	}
	currentHash := detectors.DescriptionHash(current[0])
	if currentHash == "" {
		return "", false
	}

	// History EXCLUDES the calling execution, so the baseline is
	// prior runs rather than this one. Getting this wrong is not
	// hypothetical: the schema-drift seeder originally made all its
	// calls inside a single execution, the baseline therefore did not
	// exist as persisted rows at evaluation time, and the detector
	// correctly declined while looking like it was broken.
	history, err := h.Store.ListToolDescriptions(
		ctx, projectID, toolName, executionID, toolDescriptionHistoryLimit,
	)
	if err != nil {
		return "", false
	}

	counts := map[string]int{}
	for _, d := range history {
		hash := detectors.DescriptionHash(d)
		if hash == "" {
			continue
		}
		counts[hash]++
	}

	return detectors.DetectDescriptionDrift(toolName, currentHash, counts, thresholds)
}
