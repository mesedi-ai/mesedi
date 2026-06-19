// Webhook escalation auto-fire path (task #83 slice 3 + #249 recurrence
// modes).
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
// #249 adds the recurrence path. When the handler calls
// dispatchFailureGroupRecurrence for an existing failure group, the
// same runFailureGroupDispatch function runs with isRecurrence=true,
// which:
//
//   * Sets the payload event name to "failure_group.recurred" so
//     receivers can tell repeats apart from first-time failures.
//   * Applies the per-webhook RecurrenceMode policy: "off" skips this
//     webhook entirely; "every_event" fires every time; "throttled"
//     fires only when the rolling window has elapsed since the last
//     fire for this (webhook, group) pair.
//   * On every fired recurrence (regardless of delivery outcome),
//     upserts webhook_recurrence_state so the throttle baseline
//     advances. We update on attempt rather than on success so a
//     broken receiver mid-storm doesn't cause us to retry every
//     event the receiver is missing.
//
// The goroutine uses a fresh context (NOT the request context) so the
// HTTP handler returning doesn't cancel in-flight deliveries. A
// minute-long timeout caps runaway deliveries.
package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"mesedi/backend/internal/playbooks"
	"mesedi/backend/internal/severity"
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
	go h.runFailureGroupDispatch(projectID, failureClass, signature, dashboardBase, false)
}

// dispatchFailureGroupRecurrence is the non-blocking entry point for
// recurrence events (#249). Same shape as dispatchFailureGroupCreated
// but with isRecurrence=true so the dispatcher applies per-webhook
// RecurrenceMode policy and labels the payload accordingly.
func (h *Handlers) dispatchFailureGroupRecurrence(
	projectID, failureClass, signature, dashboardBase string,
) {
	go h.runFailureGroupDispatch(projectID, failureClass, signature, dashboardBase, true)
}

func (h *Handlers) runFailureGroupDispatch(
	projectID, failureClass, signature, dashboardBase string,
	isRecurrence bool,
) {
	ctx, cancel := context.WithTimeout(context.Background(), dispatchTimeout)
	defer cancel()

	logger := h.Logger.With(
		"project_id", projectID,
		"failure_class", failureClass,
		"signature", signature,
		"recurrence", isRecurrence,
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

	// Compute event severity ONCE for this failure_group (#261).
	// Override-first lookup: if the customer has set a per-class
	// override for (project, failure_class), use it; otherwise fall
	// back to severity.Default for the class. The result rides on the
	// webhook payload AND determines per-webhook filter eligibility
	// in the loop below.
	eventSeverity := severity.Default(failureClass)
	if override, oerr := h.Store.GetProjectClassSeverity(ctx, projectID, failureClass); oerr == nil && override != nil {
		if severity.Valid(override.Severity) {
			eventSeverity = severity.Severity(override.Severity)
		}
	} else if oerr != nil && !errors.Is(oerr, store.ErrNotFound) {
		// Non-trivial DB error reading the override. Log and
		// continue with the default so we don't drop notifications
		// over a config-lookup hiccup.
		logger.Warn("webhook dispatch: severity override lookup failed (using default)",
			"error", oerr.Error())
	}

	eventName := "failure_group.created"
	if isRecurrence {
		eventName = "failure_group.recurred"
	}

	matched := 0
	for _, wh := range hooks {
		if !classMatchesFilter(failureClass, wh.EnabledClasses) {
			continue
		}
		// Severity filter (#261). Empty SeverityFilter = fire on
		// every severity (backward compatible with pre-#261 webhooks).
		// Non-empty SeverityFilter = drop the event when its severity
		// is not in the allow-list.
		if filter := severity.ParseFilter(wh.SeverityFilter); !severity.Allows(filter, eventSeverity) {
			continue
		}

		// #249 recurrence mode gate. Only applies on recurrences;
		// "new failure group" deliveries always fire as before.
		if isRecurrence && !recurrenceShouldFire(ctx, h.Store, &wh.RecurrenceMode, wh.WebhookID, group.GroupID, wh.RecurrenceWindowSeconds, logger) {
			continue
		}

		matched++
		payload := webhooks.Payload{
			Version:           "1",
			Event:             eventName,
			ProjectID:         projectID,
			WebhookID:         wh.WebhookID,
			GroupID:           group.GroupID,
			FailureClass:      failureClass,
			Severity:          string(eventSeverity),
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

		// #249: advance the throttle baseline on every recurrence
		// attempt, success or failure. Upserting on attempt (not on
		// success) prevents a broken receiver during an event storm
		// from causing us to retry every event the receiver missed.
		// The customer fixes the receiver and sees the next ping
		// after the configured window elapses.
		if isRecurrence {
			if uerr := h.Store.UpsertWebhookRecurrenceLastFired(
				ctx, wh.WebhookID, group.GroupID, time.Now().UTC(),
			); uerr != nil {
				whLogger.Warn("upsert webhook_recurrence_state failed (next throttle check may misfire once)",
					"error", uerr.Error(),
				)
			}
		}
	}

	if matched == 0 {
		logger.Debug("webhook dispatch: no webhooks matched filter or recurrence policy",
			"webhook_count", len(hooks),
		)
	}
}

// recurrenceLastFiredReader is the minimal slice of store.Store that
// recurrenceShouldFire needs. Lets the unit test pass in a tiny fake
// without standing up a full Store implementation.
type recurrenceLastFiredReader interface {
	GetWebhookRecurrenceLastFired(ctx context.Context, webhookID, groupID string) (time.Time, error)
}

// recurrenceWarnLogger is the tiny logging surface recurrenceShouldFire
// uses. slog.Logger satisfies it. Decouples the function from a
// concrete logger so tests can pass a no-op.
type recurrenceWarnLogger interface {
	Warn(msg string, args ...any)
}

// recurrenceShouldFire returns true iff this webhook should send a
// recurrence ping for this failure group right now. Honors the
// per-webhook RecurrenceMode and, for "throttled" mode, the
// RecurrenceWindowSeconds vs. the last-fired timestamp.
//
// Defensive defaults:
//   - Empty / unknown RecurrenceMode is treated as "off" so a
//     malformed row never spams.
//   - Window below RecurrenceMinWindowSeconds is floored to the min
//     so a 1-second misconfiguration cannot turn into an every_event
//     firehose with extra DB round-trips.
//   - DB errors on the last_fired lookup default to "fire" so we
//     err on the side of customer visibility, not silence. The error
//     is logged for operator visibility.
func recurrenceShouldFire(
	ctx context.Context,
	st recurrenceLastFiredReader,
	modePtr *string,
	webhookID, groupID string,
	windowSeconds int,
	logger recurrenceWarnLogger,
) bool {
	mode := store.RecurrenceModeOff
	if modePtr != nil && *modePtr != "" {
		mode = *modePtr
	}
	switch mode {
	case store.RecurrenceModeOff:
		return false
	case store.RecurrenceModeEveryEvent:
		return true
	case store.RecurrenceModeThrottled:
		if windowSeconds < store.RecurrenceMinWindowSeconds {
			windowSeconds = store.RecurrenceMinWindowSeconds
		}
		last, err := st.GetWebhookRecurrenceLastFired(ctx, webhookID, groupID)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				logger.Warn("webhook recurrence: last_fired lookup failed (firing to err on visibility)",
					"webhook_id", webhookID,
					"group_id", groupID,
					"error", err.Error(),
				)
			}
			return true
		}
		return time.Since(last) >= time.Duration(windowSeconds)*time.Second
	default:
		// Unknown mode value, treat as off rather than misfiring.
		return false
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
// every detector classification. Routes to one of two dispatch paths:
//
//   - isNew=true: dispatchFailureGroupCreated (fires
//     "failure_group.created" on every matching webhook regardless
//     of recurrence mode — a brand-new failure group is interesting
//     news no matter what).
//   - isNew=false: dispatchFailureGroupRecurrence (#249 path: applies
//     per-webhook RecurrenceMode policy).
//
// groupErr abort: if the grouping itself failed we have no
// failure_group row to attach the dispatch to, so do nothing.
func (h *Handlers) maybeFireWebhook(
	r *http.Request,
	projectID, failureClass, signature string,
	isNew bool,
	groupErr error,
) {
	if groupErr != nil {
		return
	}
	dashboardBase := h.resolveDashboardBase(r)
	if isNew {
		h.dispatchFailureGroupCreated(projectID, failureClass, signature, dashboardBase)
		return
	}
	h.dispatchFailureGroupRecurrence(projectID, failureClass, signature, dashboardBase)
}
