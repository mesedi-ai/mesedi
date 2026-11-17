package detectors

// effect_aliasing: two paths reach the same side effect and only one
// is watched.
//
// The class, its name and its one-line spec come from a thread in the
// LangChain community i-made-this channel, and its defining writeup is
// Alessandro Prandini's "Four ways an agent walked past my command
// hook" (digline.dev). A guard keyed to a name blocks the named path
// and the same effect arrives by another route; every call is
// permitted and looks clean.
//
// The check is declare-then-verify on destinations, the shape the
// environment check already uses. A run declares where it may make
// changes (EffectDeclarationPayload), optionally with the tools allowed
// to make each one; calls report what they changed (Effects). A change
// to a place not declared, or by a tool not listed for that place, is
// the event. It records destinations, not kinds of effect: not "this
// call was a write" but "this call wrote to X", because the question
// that matters is what else reaches X.
//
// What it cannot see is stated in the playbook: a destination with two
// names looks like two places (aliasing all the way down, at a higher
// price), a change made outside any recorded call leaves nothing to
// compare, and a run with no declaration is not checked.

import (
	"strings"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
)

// ToolEffects is one call's reported changes, named the way every tool
// check names a tool ("server/method" for MCP).
type ToolEffects struct {
	Source       string
	Destinations []string
}

// EffectScope is the union of a run's effect declarations.
type EffectScope struct {
	entries []effectScopeEntry
}

type effectScopeEntry struct {
	exact  string
	prefix string // set when the declared value ended in "*"
	anyVia bool
	via    map[string]bool
}

// NewEffectScope builds the scope from every declaration in the run.
// Declarations add up; a destination declared once without Via may be
// changed by any tool, whatever other declarations list for it.
// Returns nil when nothing usable was declared, meaning "not checked".
func NewEffectScope(decls []events.EffectDeclarationPayload) *EffectScope {
	scope := &EffectScope{}
	for _, d := range decls {
		for _, dest := range d.Destinations {
			v := strings.TrimSpace(dest.Destination)
			if v == "" || v == "*" {
				// A bare "*" would declare everything, which declares
				// nothing; it is ignored rather than turning the
				// check off without saying so.
				continue
			}
			e := effectScopeEntry{via: map[string]bool{}}
			if strings.HasSuffix(v, "*") {
				e.prefix = strings.TrimSuffix(v, "*")
			} else {
				e.exact = v
			}
			for _, t := range dest.Via {
				if t = strings.TrimSpace(t); t != "" {
					e.via[t] = true
				}
			}
			e.anyVia = len(e.via) == 0
			scope.entries = append(scope.entries, e)
		}
	}
	if len(scope.entries) == 0 {
		return nil
	}
	return scope
}

func (e effectScopeEntry) matches(dest string) bool {
	if e.prefix != "" {
		return strings.HasPrefix(dest, e.prefix)
	}
	return dest == e.exact
}

// DetectEffectAliasing returns the first change, in call order, that
// the scope does not allow:
//
//	"outside_scope:<destination>"            no declared place matches
//	"unlisted_path:<destination>:via:<tool>" a declared place matches,
//	                                         but not for this tool
//
// The destination is the one the call reported, so repeats of the same
// change by the same route cluster into one group. A nil scope (no
// declaration) never fires.
func DetectEffectAliasing(scope *EffectScope, calls []ToolEffects) (signature string, fired bool) {
	if scope == nil {
		return "", false
	}
	for _, c := range calls {
		for _, raw := range c.Destinations {
			dest := strings.TrimSpace(raw)
			if dest == "" {
				continue
			}
			declared, allowed := false, false
			for _, e := range scope.entries {
				if !e.matches(dest) {
					continue
				}
				declared = true
				if e.anyVia || e.via[c.Source] {
					allowed = true
					break
				}
			}
			switch {
			case !declared:
				return "outside_scope:" + dest, true
			case !allowed:
				return "unlisted_path:" + dest + ":via:" + c.Source, true
			}
		}
	}
	return "", false
}
