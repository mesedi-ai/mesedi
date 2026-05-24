// Webhook escalation auto-fire path (task #83 slice 3).
//
// Wires the failure-group creation events from the detector path
// (HandleUpdateExecution, HandleIngestEvents) into the dispatcher
// shipped in slice 2 (internal/webhooks). When any Group* method
// returns isNew=true, the handler calls dispatchFailureGroupCreated
// which spawns a goroutine that:
//
//  1. Fetches the failure_group's canonical row via
//     GetFailureGroupByClassSignature (gives us the
//     sample_execution_id Mesedi just assigned).
//  2. Lists every enabled webhook for the project.
//  3. Filters webhooks by the failure_class, webhooks with an empty
//     enabled_classes accept everything; others must include this
//     class explicitly.
//  4. Builds a payload, calls webhooks.Deliver, and records every
//     attempt to the webhook_deliveries log.
//
// The goroutine uses a fresh context (NOT the request context) so the
// HTTP handler returning doesn't cancel in-flight deliveries. A
// minute-long timeout caps runaway deliveries.
package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"mesedi/backend/internal/playbooks"
	"mesedi/backend/internal/store"
	"mesedi/backend/internal/webhooks"
)

// dispatchTimeout caps how long a single failure-group's worth of
// deliveries can run before the dispatcher's context cancels them.
// Three attempts with 1s + 4s backoff worst-case is ~5s per receiver,
// plus per-attempt timeouts, 60s is comfortable headroom even for
// projects with many webhooks registered.
const dispatchTimeout = 60 * time.Second

// dispatchFailureGroupCreated is the non-blocking entry point the
// handler calls when a Group* method reports isNew=true. Spawns a
// goroutine; never blocks the request path.
//
// dashboardBase is captured from the request (scheme + host) at the
// handler before goroutine spawn, by the time the goroutine runs,
// the original request is gone.
func (h *Handlers) dispatchFailureGroupCreated(
	projectID, failureClass, signature, dashboardBase string,
) {
	// Spawn-and-forget. Goroutine takes ownership of its own context
	// so the calling request can return immediately.
	go h.runFailureGroupDispatch(projectID, failureClass, signature, dashboardBase)
}

func (h *Handlers) runFailureGroupDispatch(
	projectID, failureClass, signature, dashboardBase string,
) {
	ctx, cancel := context.WithTimeout(context.Background(), dispatchTimeout)
	defer cancel()

	logger := h.Logger.With(
		"project_id", projectID,
		"failure_class", failureClass,
		"signature", signature,
	)

	// Fetch the failure_group row so we have the canonical
	// sample_execution_id for the payload. If lookup fails we still
	// dispatch with empty sample, the receiver gets less context but
	// the notification still fires.
	group, err := h.Store.GetFailureGroupByClassSignature(ctx, projectID, failureClass, signature)
	if err != nil {
		logger.Warn("webhook dispatch: failed to load failure_group (continuing with stub)",
			"error", err.Error(),
		)
		group = &store.FailureGroup{
			ProjectID:    projectID,
			FailureClass: failureClass,
			Signature:    signature,
		}
	}

	hooks, err := h.Store.ListEnabledProjectWebhooks(ctx, projectID)
	if err != nil {
		logger.Error("webhook dispatch: failed to list webhooks", "error", err.Error())
		return
	}
	if len(hooks) == 0 {
		// No webhooks configured for this project. Common case during
		// onboarding, don't warn, just return.
		return
	}

	// Pre-build the playbook URL once. Resolve() only tells us if a
	// playbook exists; the actual content is served at
	// /app/playbooks on the React dashboard (the URL search params
	// use "class", the dashboard side maps that to the backend's
	// "failure_class" query param for the JSON endpoint).
	var playbookURL string
	if _, ok := playbooks.Resolve(failureClass, signature); ok {
		playbookURL = dashboardBase + "/app/playbooks?class=" +
			url.QueryEscape(failureClass) + "&signature=" +
			url.QueryEscape(signature)
	}

	matched := 0
	for _, wh := range hooks {
		if !classMatchesFilter(failureClass, wh.EnabledClasses) {
			continue
		}
		matched++
		payload := webhooks.Payload{
			Version:           "1",
			Event:             "failure_group.created",
			ProjectID:         projectID,
			WebhookID:         wh.WebhookID,
			GroupID:           group.GroupID,
			FailureClass:      failureClass,
			Signature:         signature,
			SampleExecutionID: group.SampleExecutionID,
			// DashboardURL is the React-dashboard root (no trailing
			// slash). Receivers can build their own routes; first-party
			// adapters (Discord, Slack) know to append /app/executions
			// /{id} and similar.
			DashboardURL: dashboardBase,
			PlaybookURL:  playbookURL,
			DeliveryID:   newDispatchDeliveryID(),
			Timestamp:    time.Now().UTC(),
		}

		whLogger := logger.With("webhook_id", wh.WebhookID)
		result, attempts := webhooks.Deliver(ctx, whLogger, h.WebhookClient, wh, payload)

		for i := range attempts {
			if err := h.Store.RecordWebhookDelivery(ctx, &attempts[i]); err != nil {
				whLogger.Warn("record webhook delivery failed (continuing)",
					"attempt", attempts[i].Attempt,
					"error", err.Error(),
				)
			}
		}
		whLogger.Info("webhook dispatch complete",
			"status", result.Status,
			"attempts", result.Attempts,
			"duration_ms", result.DurationMs,
		)
	}

	if matched == 0 {
		logger.Debug("webhook dispatch: no webhooks matched class filter",
			"webhook_count", len(hooks),
		)
	}
}

// classMatchesFilter returns true iff this failure_class should be
// delivered to a webhook with the given enabled_classes filter.
// Empty/nil filter means "all classes."
func classMatchesFilter(failureClass string, enabledClasses []string) bool {
	if len(enabledClasses) == 0 {
		return true
	}
	for _, c := range enabledClasses {
		if c == failureClass {
			return true
		}
	}
	return false
}

// newDispatchDeliveryID returns a unique delivery identifier for the
// auto-fire path. Different from the test-endpoint id because we
// don't want manual-test and real-fire IDs to collide in the log.
func newDispatchDeliveryID() string {
	// Reuse hex(time.Now().UnixNano()), guaranteed unique within a
	// process, readable, sortable. Crypto-random would be overkill
	// for an internal identifier the receiver only uses for
	// idempotency.
	return "del-" + formatNanoHex(time.Now().UTC())
}

func formatNanoHex(t time.Time) string {
	const hexdigits = "0123456789abcdef"
	n := uint64(t.UnixNano())
	buf := make([]byte, 16)
	for i := 15; i >= 0; i-- {
		buf[i] = hexdigits[n&0xf]
		n >>= 4
	}
	return string(buf)
}

// resolveDashboardBase returns the React dashboard's externally-
// visible origin. Prefers the configured DashboardURL (set from
// MESEDI_DASHBOARD_URL at startup); falls back to deriving scheme +
// host from the inbound request for local-dev or unconfigured
// installs.
//
// Honors X-Forwarded-Proto on the fallback path so a TLS-terminating
// proxy gets the right scheme even when the backend sees plain HTTP.
func (h *Handlers) resolveDashboardBase(r *http.Request) string {
	if h.DashboardURL != "" {
		return h.DashboardURL
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// maybeFireWebhook is the post-Group* hook the handler calls after
// every detector classification. If isNew is true and the grouping
// itself succeeded, fire an async dispatch. Otherwise no-op.
func (h *Handlers) maybeFireWebhook(
	r *http.Request,
	projectID, failureClass, signature string,
	isNew bool,
	groupErr error,
) {
	if !isNew || groupErr != nil {
		return
	}
	h.dispatchFailureGroupCreated(projectID, failureClass, signature, h.resolveDashboardBase(r))
}

