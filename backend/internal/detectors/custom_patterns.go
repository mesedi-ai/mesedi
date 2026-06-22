// CustomPattern: per-project regex pattern loaded from the
// project_patterns table (Wave 2.1.a) and compiled by the API
// layer's loader (Wave 2.1.b). Detectors stay pure functions: they
// accept a (possibly nil) slice of CustomPattern and never touch
// the store. The API layer loads + compiles + passes in; detectors
// scan; the handler increments match_count on hits.

package detectors

import "regexp"

// CustomPattern is one customer-defined detector rule.
type CustomPattern struct {
	// PatternID matches the project_patterns.pattern_id column;
	// the caller uses it to call IncrementPatternMatchCount when
	// this pattern fires.
	PatternID string
	// Pattern is the raw RE2 source (useful for logging /
	// dashboard display; not used by the scan path which uses
	// Compiled).
	Pattern string
	// Severity matches the project_patterns.severity column
	// ('low' / 'medium' / 'high'). Detectors don't currently
	// route on severity — Wave 4.x splits all-matches-recorded
	// per audit gap, and that's when severity routing lands.
	Severity string
	// Compiled is the prepared regex. The API layer's loader
	// compiles each pattern once per load; the scan path matches
	// many inputs against it.
	Compiled *regexp.Regexp
}
