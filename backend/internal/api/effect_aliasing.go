package api

// effect_aliasing at run completion. Reads the run's effect
// declarations and each tool and MCP call's reported changes from the
// event list the completion pass already loaded, so it costs no extra
// query. See detectors/effect_aliasing.go for the rule and its source.

import (
	"encoding/json"
	"net/http"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
	"mesedi/backend/internal/detectors"
	"mesedi/backend/internal/store"
)

func (h *Handlers) runEffectAliasingDetector(
	r *http.Request,
	executionID, projectID string,
	evts []*events.Event,
) {
	scope, calls := effectInputs(evts)
	sig, fired := detectors.DetectEffectAliasing(scope, calls)
	if !fired {
		return
	}
	isNew, gErr := h.Store.GroupEffectAliasing(r.Context(), executionID, projectID, sig)
	if gErr != nil {
		h.Logger.Warn("effect-aliasing grouping failed (continuing)",
			"execution_id", executionID,
			"signature", sig,
			"error", gErr.Error())
	}
	h.maybeFireWebhook(r, projectID, store.FailureClassEffectAliasing, sig, isNew, gErr)
}

// effectInputs collects the declarations and, in sequence order, every
// call that reported a change. An unreadable payload is skipped: it
// cannot have declared or reported anything Mesedi can compare.
func effectInputs(evts []*events.Event) (*detectors.EffectScope, []detectors.ToolEffects) {
	var decls []events.EffectDeclarationPayload
	var calls []detectors.ToolEffects
	for _, e := range evts {
		if e == nil || len(e.Payload) == 0 {
			continue
		}
		switch e.EventType {
		case events.EventTypeEffectDeclaration:
			var d events.EffectDeclarationPayload
			if json.Unmarshal(e.Payload, &d) == nil {
				decls = append(decls, d)
			}
		case events.EventTypeToolCall, events.EventTypeMCPCall:
			var pm map[string]any
			if json.Unmarshal(e.Payload, &pm) != nil {
				continue
			}
			list, _ := pm["effects"].([]any) // any: a decoded JSON array, checked element by element below
			var dests []string
			for _, v := range list {
				if s, ok := v.(string); ok {
					dests = append(dests, s)
				}
			}
			if len(dests) > 0 {
				calls = append(calls, detectors.ToolEffects{
					Source:       toolSourceName(e.EventType, pm),
					Destinations: dests,
				})
			}
		}
	}
	return detectors.NewEffectScope(decls), calls
}
