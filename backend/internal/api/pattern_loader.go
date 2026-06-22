package api

// Loader that bridges store.ProjectPattern (Wave 2.1.a) into the
// detectors package's CustomPattern type (Wave 2.1.b). Lives in the
// API package so detectors stay pure-function and never import
// store. The handler call sites use loadCustomPatternsForDetector
// before invoking the detector's With Custom variant.

import (
	"context"
	"regexp"

	"mesedi/backend/internal/detectors"
	"mesedi/backend/internal/store"
)

// loadCustomPatternsForDetector reads enabled custom patterns for
// (projectID, detector) and compiles them into the
// detectors.CustomPattern shape the With Custom detector variants
// accept. Returns nil on store error (callers should log and
// degrade to built-ins-only); the empty / nil slice is the
// no-op input.
//
// dropped is the count of stored patterns that failed to compile
// (extremely rare — the API layer RE2-validates at PUT). The
// caller can log the drop count for ops visibility.
func (h *Handlers) loadCustomPatternsForDetector(
	ctx context.Context,
	projectID, detector string,
) (compiled []*detectors.CustomPattern, dropped int) {
	rows, err := h.Store.ListProjectPatterns(ctx, projectID, detector, true)
	if err != nil {
		h.Logger.Warn("list project_patterns failed; falling back to built-ins only",
			"project_id", projectID, "detector", detector,
			"error", err.Error())
		return nil, 0
	}
	if len(rows) == 0 {
		return nil, 0
	}
	out := make([]*detectors.CustomPattern, 0, len(rows))
	for _, r := range rows {
		if !r.Enabled {
			continue
		}
		re, compErr := regexp.Compile(r.Pattern)
		if compErr != nil {
			// Should be unreachable — POST/PATCH RE2-validates
			// before insert. Log and skip so one corrupt row
			// doesn't kill the whole detector.
			h.Logger.Warn("stored project_pattern failed to compile; skipping",
				"project_id", projectID, "detector", detector,
				"pattern_id", r.PatternID, "error", compErr.Error())
			dropped++
			continue
		}
		out = append(out, &detectors.CustomPattern{
			PatternID: r.PatternID,
			Pattern:   r.Pattern,
			Severity:  r.Severity,
			Compiled:  re,
		})
	}
	return out, dropped
}

// incrementCustomPatternMatch increments match_count synchronously on
// a custom-pattern hit. Telemetry is best-effort; an error here is
// logged and swallowed (the detector verdict still fires regardless).
func (h *Handlers) incrementCustomPatternMatch(
	ctx context.Context,
	projectID, patternID string,
) {
	if patternID == "" {
		return
	}
	if err := h.Store.IncrementPatternMatchCount(ctx, projectID, patternID, 1); err != nil {
		// ErrNotFound here means the customer deleted the pattern
		// between the load and the increment — race window is
		// narrow and benign; don't log noisily.
		if !errIsNotFound(err) {
			h.Logger.Warn("increment project_pattern match_count failed",
				"project_id", projectID, "pattern_id", patternID,
				"error", err.Error())
		}
	}
}

// errIsNotFound mirrors store.ErrNotFound semantics without an
// import cycle (this file is in the api package, store is the
// concrete dependency).
func errIsNotFound(err error) bool {
	if err == nil {
		return false
	}
	return err.Error() == store.ErrNotFound.Error()
}

// uniqueStrings returns the input slice with duplicate entries
// removed, preserving the first-occurrence order. Used by the DLP
// hot path to de-dup pattern_id increments — a single pattern
// firing on multiple events in the same batch should bump the
// counter once, not once per match.
func uniqueStrings(in []string) []string {
	if len(in) <= 1 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
