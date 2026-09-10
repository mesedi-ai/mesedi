package api

import "strings"

// normalizeListQuery sanitizes a list-endpoint search query string for
// safe SQL use. Trims whitespace, drops control characters, caps at 256
// bytes (a search for hundreds of chars is almost certainly an exploit
// attempt or a paste accident). The SQL layer parameterizes the value
// so there's no injection risk; this is purely about response sanity
// and avoiding pathological substring scans.
//
// Moved verbatim out of handlers.go during the split, offsetting the
// dispatch-tracking field the same commit added there.
func normalizeListQuery(raw string) string {
	const maxLen = 256
	out := strings.TrimSpace(raw)
	if out == "" {
		return ""
	}
	// Strip control chars (0x00-0x1F + 0x7F) which can't appear in
	// meaningful customer-visible signature or execution_id text.
	out = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7F {
			return -1
		}
		return r
	}, out)
	if len(out) > maxLen {
		out = out[:maxLen]
	}
	return out
}
