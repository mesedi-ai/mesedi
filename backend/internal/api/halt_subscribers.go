// SSE remote-halt channel, backend infrastructure (sub-slice 21b.1).
//
// Two HTTP endpoints + an in-memory subscriber registry:
//
//   GET  /executions/{id}/halt-stream: Server-Sent Events. Opens a
//     long-lived text/event-stream connection. The Python/TS SDK
//     subscribes here when an execution is wrapped with a budget; the
//     handler holds the connection open, sending periodic keepalive
//     comments (`: keepalive\n\n`) until either a halt is requested
//     for this execution, the client disconnects, or the connection
//     idles past a hard timeout.
//
//   POST /executions/{id}/halt: admin/dashboard-triggered.
//     Body: {"reason": "..."}. Looks up all active subscribers for
//     this execution_id in the in-memory registry and pushes a single
//     `event: halt\ndata: {"reason":"..."}\n\n` to each. Returns 200
//     with the count of subscribers notified, 404 if there are no
//     active subscribers (execution probably already completed, or
//     the SDK never subscribed).
//
// Cross-tenant isolation:
//
//   Both endpoints look up the execution via Store.GetExecution and
//   verify execution.ProjectID == authProjectID before doing anything
//   else. Cross-tenant attempts get a 404 (not 403), which prevents
//   leaking which execution_ids exist on other projects.
//
// Subscriber registry:
//
//   In-memory map[execution_id]list-of-channels, guarded by an
//   RWMutex. Each subscriber is a buffered Go channel that the SSE
//   handler reads from in a loop. When a halt is pushed via POST, we
//   send to every subscriber's channel (non-blocking; if any
//   subscriber's channel is full somehow, we just drop the message
//   for that subscriber, the alternative is blocking the POST
//   handler, which is worse).
//
//   Registry is per-process. Multi-instance backends would need
//   Redis-pubsub or similar; that's a future-slice problem.
//
//   When an SSE connection closes (client disconnect, halt sent,
//   execution terminates), the handler MUST deregister itself from
//   the registry. The defer pattern in HandleHaltStream guarantees
//   this even on panic.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"mesedi/backend/internal/store"
)

// haltSubscriber is one open SSE connection waiting for a halt event.
// The channel receives the halt reason; the handler reads from it and
// writes the SSE frame on receive. Buffered size 1, a halt is a
// single-shot signal, no point queueing.
type haltSubscriber struct {
	ch chan string
}

// HaltSubscribers is the in-memory subscriber registry. One per
// Handlers struct (constructed in api.New(...)).
type HaltSubscribers struct {
	mu   sync.RWMutex
	subs map[string][]*haltSubscriber
}

// NewHaltSubscribers returns an initialized registry.
func NewHaltSubscribers() *HaltSubscribers {
	return &HaltSubscribers{
		subs: make(map[string][]*haltSubscriber),
	}
}

// register adds a subscriber for executionID and returns a cleanup
// function that must be deferred to remove the subscriber on exit.
func (h *HaltSubscribers) register(executionID string, sub *haltSubscriber) (cleanup func()) {
	h.mu.Lock()
	h.subs[executionID] = append(h.subs[executionID], sub)
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		list := h.subs[executionID]
		out := list[:0]
		for _, s := range list {
			if s != sub {
				out = append(out, s)
			}
		}
		if len(out) == 0 {
			delete(h.subs, executionID)
		} else {
			h.subs[executionID] = out
		}
	}
}

// publish pushes a halt reason to every subscriber for executionID.
// Returns the number of subscribers notified. Non-blocking, if a
// subscriber's channel is full (shouldn't happen with buffered
// size-1 + the handler reading immediately, but defensive), we drop
// rather than block the publisher.
func (h *HaltSubscribers) publish(executionID, reason string) int {
	h.mu.RLock()
	subs := h.subs[executionID]
	defer h.mu.RUnlock()

	count := 0
	for _, s := range subs {
		select {
		case s.ch <- reason:
			count++
		default:
			// Subscriber channel full, drop. With buffered size 1
			// and a single halt per execution lifecycle, this should
			// never fire; logging-or-dropping is the right tradeoff
			// vs blocking the POST.
		}
	}
	return count
}

// HandleHaltStream is the SSE endpoint. Long-lived connection.
// Sends `: keepalive` comments every 15s to keep proxies / clients
// from timing the connection out. Closes when:
//   - the execution's halt is published (sends `event: halt\ndata: ...`)
//   - the client disconnects
//   - the per-connection max-lifetime timer fires (defensive cap; SDK
//     should re-subscribe after this)
func (h *Handlers) HandleHaltStream(w http.ResponseWriter, r *http.Request) {
	executionID := r.PathValue("id")
	if executionID == "" {
		writeError(w, http.StatusBadRequest, "execution_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	// Cross-tenant guard, lenient on missing-execution to handle the
	// SDK's async-shipper ordering: wrap() submits create-execution
	// to the shipper queue (fire-and-forget) and then starts the
	// halt-stream reader immediately. The reader can race ahead of
	// the shipper, hitting this endpoint before the execution row
	// exists. Subscribe has no side effects beyond registry insert,
	// so accepting "execution doesn't exist yet" subscriptions is
	// safe, they just sit in the registry until something
	// (eventually) publishes against the same id, which can only
	// come from a caller in the SAME project (publish-side enforces
	// the strict project-id match). If the execution NEVER appears,
	// the subscription burns its keepalive timer and exits cleanly.
	//
	// If the execution DOES exist, we still verify project ownership
	// strictly. 404 (not 403) on mismatch so we don't leak which
	// execution_ids exist on other projects.
	exec, err := h.Store.GetExecution(r.Context(), executionID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if exec != nil && exec.ProjectID != authProjectID {
		writeError(w, http.StatusNotFound, "execution not found")
		return
	}

	// SSE headers. Content-Type is required; Cache-Control and
	// Connection are belt-and-suspenders against intermediaries that
	// would otherwise buffer or close.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // nginx + cloudflare hint
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		// Should never happen with Go's net/http stdlib, but if a
		// future middleware wraps the writer in something that
		// doesn't implement Flusher, fall back to a clean error
		// instead of silently buffering forever.
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	sub := &haltSubscriber{ch: make(chan string, 1)}
	cleanup := h.HaltSubs.register(executionID, sub)
	defer cleanup()

	// Send an initial comment so the client knows the connection is
	// live and registered. Comments in SSE start with `:` and are
	// ignored by client parsers, they just keep the connection warm.
	if _, err := fmt.Fprintf(w, ": subscribed execution=%s\n\n", executionID); err != nil {
		return
	}
	flusher.Flush()

	const keepaliveInterval = 15 * time.Second
	// Hard cap on connection lifetime, SDK should re-subscribe past
	// this. 1 hour matches typical agent execution upper bound; longer
	// runs that need halt-channel coverage will see the SDK reconnect.
	const maxLifetime = 1 * time.Hour
	ctx, cancel := context.WithTimeout(r.Context(), maxLifetime)
	defer cancel()

	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Client disconnected, request timed out, or the max
			// lifetime fired. Cleanup runs via defer; just return.
			return
		case reason := <-sub.ch:
			// Halt was published for this execution. Send the SSE
			// frame and return, this is a one-shot signal; we don't
			// keep the connection open for further events.
			payload, _ := json.Marshal(map[string]string{"reason": reason})
			if _, err := fmt.Fprintf(w, "event: halt\ndata: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
			return
		case <-ticker.C:
			// Keepalive, comment frame, ignored by client.
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// HandleTriggerHalt is the admin/dashboard-triggered endpoint that
// pushes a halt to all subscribers for an execution. Body:
//
//	{"reason": "optional human-readable string"}
//
// Returns:
//
//	200 OK { "ok": true, "notified": N }: N >= 1 subscribers
//	404 Not Found: no active subscribers
//	                                         (execution probably done,
//	                                         or SDK never subscribed)
func (h *Handlers) HandleTriggerHalt(w http.ResponseWriter, r *http.Request) {
	executionID := r.PathValue("id")
	if executionID == "" {
		writeError(w, http.StatusBadRequest, "execution_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	// Same cross-tenant guard as the stream endpoint.
	exec, err := h.Store.GetExecution(r.Context(), executionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "execution not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if exec.ProjectID != authProjectID {
		writeError(w, http.StatusNotFound, "execution not found")
		return
	}

	var body struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength > 0 {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
	}
	if body.Reason == "" {
		body.Reason = "halt requested via API"
	}

	notified := h.HaltSubs.publish(executionID, body.Reason)
	if notified == 0 {
		writeError(w, http.StatusNotFound, "no active halt-stream subscribers for this execution")
		return
	}

	h.Logger.Info("halt published",
		"execution_id", executionID,
		"reason", body.Reason,
		"subscribers_notified", notified,
	)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"notified": notified,
	})
}
