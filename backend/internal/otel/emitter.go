// OpenTelemetry parallel emission (Mesedi #22).
//
// The Emitter sits beside Mesedi's native pipeline. When an
// execution reaches a terminal status, the handler hands the full
// (execution, events) tuple to Emit, which translates it into an
// OTel trace (one root span for the execution + one child span
// per event) and ships it via OTLP/HTTP to the customer-configured
// collector endpoint. The emitter is opt-in: if OTEL_EXPORTER_OTLP_ENDPOINT
// is unset, Init returns a no-op emitter and every Emit call short-
// circuits with no side effects.
//
// Design choices:
//
//   - Each execution is its own OTel trace. Multi-agent linkage
//     (parent_execution_id) is recorded as the attribute
//     `mesedi.parent_execution_id` on the root span so downstream
//     backends (Datadog, Honeycomb, Grafana Tempo) can join across
//     traces by that attribute. We deliberately do NOT propagate
//     parent_execution_id as an OTel parent context because doing
//     so requires us to remember the parent's trace_id across
//     processes, which we don't store today.
//
//   - Events get child spans timed to the event's timestamp + the
//     event's duration_ms (when known). The span is opened with
//     trace.WithTimestamp on Start and End so the recorded times
//     match the Mesedi data exactly, not the moment we emit.
//
//   - Emission is fire-and-forget: a goroutine reads the events,
//     constructs the spans, and lets the OTel SDK batch them.
//     Failures log + continue; they never block the PATCH.
//
//   - GenAI semantic conventions are honored per the existing
//     SemConvMode in config.go (#23). llm_call events emit
//     gen_ai.* attributes when the customer opted into that
//     incubating track.
package otel

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"mesedi/backend/internal/events"
)

// Emitter encapsulates the OTel SDK state. The zero value is a
// safe no-op; callers should always hold the pointer returned by
// Init and check Enabled before doing meaningful work.
type Emitter struct {
	enabled  bool
	provider *sdktrace.TracerProvider
	tracer   trace.Tracer
	logger   *slog.Logger
}

// Init constructs the global emitter from env. Reads:
//
//   - OTEL_EXPORTER_OTLP_ENDPOINT  required; absence disables
//   - OTEL_EXPORTER_OTLP_HEADERS   optional; comma-separated
//     "key=value" pairs (Datadog API key, Honeycomb token, etc.)
//   - OTEL_SERVICE_NAME            optional; defaults to "mesedi"
//
// Returns a non-nil Emitter even when disabled; callers always
// invoke Emit unconditionally and the no-op short-circuits.
func Init(ctx context.Context, logger *slog.Logger) (*Emitter, error) {
	endpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if endpoint == "" {
		logger.Info("otel: parallel emission disabled (OTEL_EXPORTER_OTLP_ENDPOINT unset)")
		return &Emitter{enabled: false, logger: logger}, nil
	}

	// Parse the endpoint so otlptracehttp can be pointed at it
	// correctly. The exporter wants a host (no scheme) when using
	// WithEndpoint, and an explicit WithInsecure / WithTLSConfig.
	// We accept the standard OTel env var form which includes a
	// scheme.
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("otel: parse endpoint: %w", err)
	}
	insecure := u.Scheme == "http"
	host := u.Host
	if host == "" {
		host = endpoint
	}

	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(host),
	}
	if insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	if path := strings.TrimSuffix(u.Path, "/"); path != "" {
		opts = append(opts, otlptracehttp.WithURLPath(path))
	}
	if hdrs := parseHeaders(os.Getenv("OTEL_EXPORTER_OTLP_HEADERS")); len(hdrs) > 0 {
		opts = append(opts, otlptracehttp.WithHeaders(hdrs))
	}

	exporter, err := otlptrace.New(ctx, otlptracehttp.NewClient(opts...))
	if err != nil {
		return nil, fmt.Errorf("otel: build exporter: %w", err)
	}

	serviceName := strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME"))
	if serviceName == "" {
		serviceName = "mesedi"
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(serviceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("otel: build resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	tracer := provider.Tracer("mesedi/backend")

	logger.Info("otel: parallel emission enabled",
		"endpoint", endpoint,
		"service_name", serviceName,
		"semconv_mode", semConvLabel(SemConv()),
	)

	return &Emitter{
		enabled:  true,
		provider: provider,
		tracer:   tracer,
		logger:   logger,
	}, nil
}

// Shutdown flushes pending spans and closes the exporter. Safe to
// call on a disabled emitter (no-op).
func (e *Emitter) Shutdown(ctx context.Context) error {
	if e == nil || !e.enabled || e.provider == nil {
		return nil
	}
	return e.provider.Shutdown(ctx)
}

// Enabled reports whether the emitter has a live OTLP exporter
// configured. Hot-path predicate so callers can skip building the
// (execution, events) tuple when nothing will be done with it.
func (e *Emitter) Enabled() bool {
	return e != nil && e.enabled
}

// Emit translates one terminal execution into an OTel trace.
// One root span timed to (started_at -> ended_at) plus one child
// span per event timed to (timestamp -> timestamp+duration). The
// caller should run this in a goroutine so OTel batching does not
// block the main PATCH path.
//
// Best-effort throughout: any error during emission is logged but
// not returned, because OTel emission failing must never break
// the customer-facing pipeline.
func (e *Emitter) Emit(ctx context.Context, exec *events.Execution, evts []*events.Event) {
	if !e.Enabled() || exec == nil {
		return
	}
	mode := SemConv()

	endedAt := time.Now()
	if exec.EndedAt != nil {
		endedAt = *exec.EndedAt
	}

	// Root span for the execution.
	rootCtx, root := e.tracer.Start(
		ctx,
		"mesedi.execution",
		trace.WithTimestamp(exec.StartedAt),
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(executionAttributes(exec, mode)...),
	)
	if exec.Status != events.StatusCompleted {
		root.SetStatus(codes.Error, string(exec.Status))
	} else {
		root.SetStatus(codes.Ok, "")
	}
	if exec.CrashSignature != "" {
		root.SetAttributes(attribute.String("mesedi.crash_signature", exec.CrashSignature))
	}

	// Child span per event. Slice elements are pointers because
	// store.ListEventsForExecution returns []*events.Event; we
	// take that shape directly to avoid copying the payload bytes.
	for _, evt := range evts {
		if evt == nil {
			continue
		}
		evtEnd := evt.Timestamp
		if evt.DurationMs > 0 {
			evtEnd = evt.Timestamp.Add(time.Duration(evt.DurationMs) * time.Millisecond)
		}
		_, span := e.tracer.Start(
			rootCtx,
			"mesedi."+string(evt.EventType),
			trace.WithTimestamp(evt.Timestamp),
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(eventAttributes(evt, mode)...),
		)
		span.End(trace.WithTimestamp(evtEnd))
	}

	root.End(trace.WithTimestamp(endedAt))
}

// parseHeaders converts the OTel env-var format "k1=v1,k2=v2" into
// a header map. Tokens are trimmed; malformed pairs are dropped.
func parseHeaders(raw string) map[string]string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		if key == "" || val == "" {
			continue
		}
		out[key] = val
	}
	return out
}

// executionAttributes builds the attribute set for the root span.
// The set spans Mesedi-native fields (project_id, execution_id,
// tenant_id, cost) and, when SemConv mode allows, GenAI
// incubating attributes that downstream backends can index on.
func executionAttributes(exec *events.Execution, mode SemConvMode) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("mesedi.execution_id", exec.ExecutionID),
		attribute.String("mesedi.project_id", exec.ProjectID),
		attribute.String("mesedi.status", string(exec.Status)),
		attribute.Int64("mesedi.duration_ms", exec.DurationMs),
		attribute.Int("mesedi.total_tokens_in", exec.TotalTokensIn),
		attribute.Int("mesedi.total_tokens_out", exec.TotalTokensOut),
		attribute.Float64("mesedi.estimated_cost_usd", exec.EstimatedCostUSD),
	}
	if exec.ParentExecutionID != nil {
		attrs = append(attrs, attribute.String("mesedi.parent_execution_id", *exec.ParentExecutionID))
	}
	if exec.TenantID != nil {
		attrs = append(attrs, attribute.String("mesedi.tenant_id", *exec.TenantID))
	}
	if exec.SDKLanguage != "" {
		attrs = append(attrs, attribute.String("mesedi.sdk_language", exec.SDKLanguage))
	}
	if exec.SDKVersion != "" {
		attrs = append(attrs, attribute.String("mesedi.sdk_version", exec.SDKVersion))
	}
	if exec.FailureGroupID != nil {
		attrs = append(attrs, attribute.String("mesedi.failure_group_id", *exec.FailureGroupID))
	}
	if exec.PauseCount > 0 {
		attrs = append(attrs, attribute.Int("mesedi.pause_count", exec.PauseCount))
		attrs = append(attrs, attribute.Int64("mesedi.total_paused_ms", exec.TotalPausedMs))
	}
	// GenAI incubating attribute set (only when opted in via #23).
	if mode.EmitIncubating() {
		attrs = append(attrs, attribute.Int("gen_ai.usage.input_tokens", exec.TotalTokensIn))
		attrs = append(attrs, attribute.Int("gen_ai.usage.output_tokens", exec.TotalTokensOut))
	}
	return attrs
}

// eventAttributes builds the attribute set for one event's child
// span. The first switch maps Mesedi event types onto OTel
// GenAI operation names where possible; the second pass attaches
// type-specific payload extracts.
func eventAttributes(evt *events.Event, mode SemConvMode) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("mesedi.event.type", string(evt.EventType)),
		attribute.String("mesedi.event.id", evt.EventID),
		attribute.Int("mesedi.event.sequence", evt.Sequence),
	}
	if evt.DurationMs > 0 {
		attrs = append(attrs, attribute.Int64("mesedi.event.duration_ms", evt.DurationMs))
	}

	// Lightweight payload peek: extract a few well-known fields
	// per event type into typed OTel attributes. We do NOT attach
	// the entire payload as a stringified blob; backends index
	// the typed fields and the rest is preserved on the Mesedi
	// side via the event_id link.
	switch evt.EventType {
	case events.EventTypeLLMCall:
		// Field names match events.LLMCallPayload (json:
		// "input_tokens", "output_tokens"). Decoding into a local
		// struct keeps the OTel emitter independent of payload-
		// shape changes elsewhere and lets us pick only the
		// attributes we want to ship downstream.
		var p struct {
			Provider     string  `json:"provider"`
			Model        string  `json:"model"`
			InputTokens  int     `json:"input_tokens"`
			OutputTokens int     `json:"output_tokens"`
			CostUSD      float64 `json:"cost_usd"`
			FinishReason string  `json:"finish_reason"`
		}
		if err := json.Unmarshal(evt.Payload, &p); err == nil {
			if p.Provider != "" {
				attrs = append(attrs, attribute.String("mesedi.llm.provider", p.Provider))
			}
			if p.Model != "" {
				attrs = append(attrs, attribute.String("mesedi.llm.model", p.Model))
			}
			if p.InputTokens > 0 {
				attrs = append(attrs, attribute.Int("mesedi.llm.input_tokens", p.InputTokens))
			}
			if p.OutputTokens > 0 {
				attrs = append(attrs, attribute.Int("mesedi.llm.output_tokens", p.OutputTokens))
			}
			if p.CostUSD > 0 {
				attrs = append(attrs, attribute.Float64("mesedi.llm.cost_usd", p.CostUSD))
			}
			if p.FinishReason != "" {
				attrs = append(attrs, attribute.String("mesedi.llm.finish_reason", p.FinishReason))
			}
			if mode.EmitIncubating() {
				if p.Provider != "" {
					attrs = append(attrs, attribute.String("gen_ai.system", p.Provider))
				}
				if p.Model != "" {
					attrs = append(attrs, attribute.String("gen_ai.request.model", p.Model))
				}
				if p.InputTokens > 0 {
					attrs = append(attrs, attribute.Int("gen_ai.usage.input_tokens", p.InputTokens))
				}
				if p.OutputTokens > 0 {
					attrs = append(attrs, attribute.Int("gen_ai.usage.output_tokens", p.OutputTokens))
				}
				attrs = append(attrs, attribute.String("gen_ai.operation.name", "chat"))
			}
		}
	case events.EventTypeToolCall:
		var p struct {
			ToolName   string `json:"tool_name"`
			ErrorClass string `json:"error_class"`
		}
		if err := json.Unmarshal(evt.Payload, &p); err == nil {
			if p.ToolName != "" {
				attrs = append(attrs, attribute.String("mesedi.tool.name", p.ToolName))
				if mode.EmitIncubating() {
					attrs = append(attrs, attribute.String("gen_ai.tool.name", p.ToolName))
				}
			}
			if p.ErrorClass != "" {
				attrs = append(attrs, attribute.String("mesedi.tool.error_class", p.ErrorClass))
			}
		}
	case events.EventTypeMCPCall:
		var p struct {
			ServerName string `json:"server_name"`
			Method     string `json:"method"`
			ErrorClass string `json:"error_class"`
		}
		if err := json.Unmarshal(evt.Payload, &p); err == nil {
			if p.ServerName != "" {
				attrs = append(attrs, attribute.String("mesedi.mcp.server_name", p.ServerName))
			}
			if p.Method != "" {
				attrs = append(attrs, attribute.String("mesedi.mcp.method", p.Method))
			}
			if p.ErrorClass != "" {
				attrs = append(attrs, attribute.String("mesedi.mcp.error_class", p.ErrorClass))
			}
		}
	case events.EventTypeAgentHandoff:
		var p struct {
			FromAgent        string `json:"from_agent"`
			ToAgent          string `json:"to_agent"`
			HandoffKind      string `json:"handoff_kind"`
			ChildExecutionID string `json:"child_execution_id"`
		}
		if err := json.Unmarshal(evt.Payload, &p); err == nil {
			if p.FromAgent != "" {
				attrs = append(attrs, attribute.String("mesedi.handoff.from_agent", p.FromAgent))
			}
			if p.ToAgent != "" {
				attrs = append(attrs, attribute.String("mesedi.handoff.to_agent", p.ToAgent))
			}
			if p.HandoffKind != "" {
				attrs = append(attrs, attribute.String("mesedi.handoff.kind", p.HandoffKind))
			}
			if p.ChildExecutionID != "" {
				attrs = append(attrs, attribute.String("mesedi.handoff.child_execution_id", p.ChildExecutionID))
			}
		}
	case events.EventTypeHumanIntervention:
		var p struct {
			ResponseKind   string `json:"response_kind"`
			WaitDurationMs int64  `json:"wait_duration_ms"`
			SLASeconds     int64  `json:"sla_seconds"`
			DecidedBy      string `json:"decided_by"`
		}
		if err := json.Unmarshal(evt.Payload, &p); err == nil {
			if p.ResponseKind != "" {
				attrs = append(attrs, attribute.String("mesedi.hitl.response_kind", p.ResponseKind))
			}
			if p.WaitDurationMs > 0 {
				attrs = append(attrs, attribute.Int64("mesedi.hitl.wait_duration_ms", p.WaitDurationMs))
			}
			if p.SLASeconds > 0 {
				attrs = append(attrs, attribute.Int64("mesedi.hitl.sla_seconds", p.SLASeconds))
			}
			if p.DecidedBy != "" {
				attrs = append(attrs, attribute.String("mesedi.hitl.decided_by", p.DecidedBy))
			}
		}
	case events.EventTypeEvalScore:
		var p struct {
			EvaluatorID string  `json:"evaluator_id"`
			MetricType  string  `json:"metric_type"`
			Score       float64 `json:"score"`
			Passed      bool    `json:"passed"`
		}
		if err := json.Unmarshal(evt.Payload, &p); err == nil {
			if p.EvaluatorID != "" {
				attrs = append(attrs, attribute.String("mesedi.eval.evaluator_id", p.EvaluatorID))
			}
			if p.MetricType != "" {
				attrs = append(attrs, attribute.String("mesedi.eval.metric_type", p.MetricType))
			}
			attrs = append(attrs, attribute.Float64("mesedi.eval.score", p.Score))
			attrs = append(attrs, attribute.Bool("mesedi.eval.passed", p.Passed))
		}
	}
	return attrs
}

// semConvLabel renders the SemConv mode as a short readable
// string for log output. Logging the raw int would be unfriendly
// to operators tailing journalctl.
func semConvLabel(m SemConvMode) string {
	switch m {
	case ModeGenAI:
		return "gen_ai"
	case ModeGenAIDup:
		return "gen_ai/dup"
	default:
		return "stable"
	}
}
