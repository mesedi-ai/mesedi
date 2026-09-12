// AI analysis of a failure group: prompt construction and the handler.
// Split out of handlers.go on 2026-09-12; every declaration moved verbatim.
package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
	"mesedi/backend/internal/anthropic"
	"mesedi/backend/internal/playbooks"
	"mesedi/backend/internal/store"
)

// analysisSystemPrompt is the system message sent to the LLM on every
// AI root-cause analysis call. Extracted as a named constant so the
// wording is editable in one place and unit-testable.
//
// The two tail sentences (per-project tuning knobs + signature
// decomposition) are the difference between B-grade and A-grade
// analyses in the post-audit
// (internal-extract/ai-analyses-vs-playbooks-audit.md). Without them
// the model sees Mesedi-specific knobs like custom_model_windows /
// severity_policy / detector thresholds in the playbook but doesn't
// surface them in remediation, and it treats decomposable signatures
// like cost_$0.10+ or context_overflow:warn:claude-haiku-4-5 as
// opaque strings rather than reading the bucket / level / model.
const analysisSystemPrompt = "You are Mesedi's senior on-call engineer for AI agent reliability. " +
	"You read structured failure-group telemetry and write a concise root-cause " +
	"analysis with two concrete remediation suggestions. Output is rendered as " +
	"Markdown in a dashboard card. Be precise, opinionated, and honest about " +
	"uncertainty. Never claim a specific fix will resolve the issue; frame " +
	"recommendations as hypotheses the operator should test. " +
	"When the playbook mentions per-project tuning knobs (thresholds, allowlists, " +
	"custom model windows, severity policies), reference them by name in your " +
	"remediation hypotheses when they apply to this customer's situation. " +
	"When the signature decomposes into meaningful parts (the bucket in " +
	"cost_$0.10+, the level + model in context_overflow:warn:claude-haiku-4-5, " +
	"the agent pair in coordination_deadlock:critique_agent:planner_agent), " +
	"interpret those parts explicitly rather than treating the signature as " +
	"an opaque string. " +
	"When the playbook block includes a 'Published at:' URL and you refer " +
	"to the playbook by name or by one of its numbered causes or shapes, " +
	"link it as a Markdown link on that reference so the reader can open " +
	"it directly. Link it at most once; do not repeat the URL. If no " +
	"'Published at:' line is present, refer to the playbook in plain text " +
	"and never invent a URL."

func (h *Handlers) HandleAnalyzeFailureGroup(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("id")
	if groupID == "" {
		writeError(w, http.StatusBadRequest, "group_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	if h.Anthropic == nil || !h.Anthropic.Enabled() {
		writeError(w, http.StatusServiceUnavailable,
			"AI analysis is not configured for this deployment (ANTHROPIC_API_KEY unset)")
		return
	}

	group, err := h.Store.GetFailureGroup(r.Context(), groupID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "failure group not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if group.ProjectID != authProjectID {
		writeError(w, http.StatusNotFound, "failure group not found")
		return
	}

	// Cache hit: return the existing analysis if it was generated
	// within the last 24 hours AND no new affected executions have
	// landed since (last_seen <= analyzed_at).
	if group.AnalyzedAt != nil && group.AnalysisMarkdown != nil {
		fresh := time.Since(*group.AnalyzedAt) < 24*time.Hour
		stable := !group.LastSeen.After(*group.AnalyzedAt)
		// The query param ?regenerate=1 forces a re-analysis even
		// when the cache is warm. Lets the customer ask for a
		// fresh take after deploying a fix.
		forced := r.URL.Query().Get("regenerate") == "1"
		if fresh && stable && !forced {
			writeJSON(w, http.StatusOK, group)
			return
		}
	}

	// Tier gate + per-period rate limit (added to protect
	// the operator's Anthropic bill from being subsidized by users
	// without payment on file and from a Team customer with thousands
	// of distinct failure groups generating unbounded LLM calls).
	//
	// Order:
	//   1. Load project to get tier + period bounds.
	//   2. Hobby (pay-per-use, $0.75 each, 50 / period cap):
	//      a. No Stripe card on file → refuse with 402; analyses
	//         are billed at period end and cannot be charged to a
	//         missing card. Dashboard surfaces "add a payment
	//         method on /app/billing to use AI root-cause."
	//      b. Card present + count >= HobbyAIAnalysisLimit (50)
	//         → refuse with 429 + upgrade nudge to Team.
	//      c. Otherwise allow; scheduler picks up the charge at
	//         period end (HobbyBillingScheduler combines analysis
	//         cost with execution overage in one PaymentIntent).
	//   3. Team: count analyses since current_period_start. If at
	//      or above TeamAIAnalysisLimit, refuse with 429; the
	//      dashboard renders "AI explanation rate limit reached
	//      this period" alongside the raw failure-group row.
	//   4. Enterprise: skip rate limit (contract-defined).
	//
	// Cache hits short-circuited above; only real LLM-calling
	// requests reach this block.
	proj, err := h.Store.GetProject(r.Context(), authProjectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"load project for tier check: "+err.Error())
		return
	}
	tier := normalizeTier(proj.Tier)
	if tier == TierHobby {
		if !proj.CardOnFile {
			writeError(w, http.StatusPaymentRequired,
				fmt.Sprintf("AI root-cause analysis on Cloud Hobby is pay-per-use at $%.2f per analysis. Add a payment method on /app/billing to enable it, or upgrade to Cloud Team for %d analyses included.",
					HobbyAIAnalysisPriceUSD, EffectiveTeamAIAnalysisLimit()))
			return
		}
		// Hobby is single-project tier; per-project count is the
		// canonical scope (no tenant fan-out needed here).
		since := time.Now().UTC().AddDate(0, -1, 0)
		if proj.CurrentPeriodStart != nil {
			since = *proj.CurrentPeriodStart
		}
		count, cErr := h.Store.CountAIAnalysesSincePeriodStart(
			r.Context(), authProjectID, since,
		)
		hobbyLimit := EffectiveHobbyAIAnalysisLimit()
		if cErr != nil {
			h.Logger.Warn("analyze: hobby count check failed",
				"project_id", authProjectID, "error", cErr.Error())
			// Fail open on the rate-limit query itself. A DB blip
			// should not block a legitimate analysis request.
		} else if count >= hobbyLimit {
			h.Logger.Info("analyze: hobby rate limit reached",
				"project_id", authProjectID,
				"count", count, "limit", hobbyLimit)
			writeError(w, http.StatusTooManyRequests,
				fmt.Sprintf("You have used %d of %d AI root-cause analyses this period on Cloud Hobby ($%.2f each, capped at %d to prevent surprise bills). Upgrade to Cloud Team at /app/billing for %d included per period plus unlimited projects + 90-day retention. Failure detection still works; AI explanations resume next billing period.",
					count, hobbyLimit, HobbyAIAnalysisPriceUSD,
					hobbyLimit, EffectiveTeamAIAnalysisLimit()))
			return
		}
	}
	if tier == TierTeam {
		// Use current_period_start as the rate-limit window. If it
		// is nil (race window between Stripe checkout and webhook),
		// fall back to a conservative 30-day rolling window so a
		// brand-new Team customer can still get analyses.
		since := time.Now().UTC().AddDate(0, -1, 0)
		if proj.CurrentPeriodStart != nil {
			since = *proj.CurrentPeriodStart
		}

		// Count analyses across the entire ORGANIZATION (tenant),
		// not just the calling project, because Team allows unlimited
		// projects under one org and a per-project cap would be
		// trivially bypassed by spawning more projects. Fall back to
		// per-project count only when the project has no tenant_id
		// (legacy row that escaped migration-013 backfill).
		tenantID, terr := h.Store.GetProjectTenantID(r.Context(), authProjectID)
		var count int
		var cErr error
		switch {
		case terr == nil && tenantID != nil && *tenantID != "":
			count, cErr = h.Store.CountAIAnalysesByTenantSince(
				r.Context(), *tenantID, since,
			)
		default:
			// tenant_id NULL or lookup failed: fall back to project
			// scope. This leaves an edge-case bypass for legacy
			// unbackfilled projects, accepted because (a) migration
			// 013 already ran on prod, (b) any remaining NULL rows
			// can be one-off backfilled if discovered, and (c)
			// blocking the request entirely would be worse UX than
			// occasionally over-allowing on a rare legacy edge case.
			count, cErr = h.Store.CountAIAnalysesSincePeriodStart(
				r.Context(), authProjectID, since,
			)
		}

		if cErr != nil {
			h.Logger.Warn("analyze: count check failed",
				"project_id", authProjectID, "error", cErr.Error())
			// Fail open on the rate-limit query itself. A DB blip
			// should not block a legitimate analysis request.
		} else if teamLimit := EffectiveTeamAIAnalysisLimit(); count >= teamLimit {
			// : Team that removed their card mid-cycle is
			// hard-capped at the included 200 because there's no
			// way to bill the $0.50 overage. The 402 message
			// guides them to either re-add a card or wait for
			// next period.
			if !proj.CardOnFile {
				h.Logger.Info("analyze: team no-card hard cap at included",
					"project_id", authProjectID,
					"count", count, "included", teamLimit)
				writeError(w, http.StatusPaymentRequired,
					fmt.Sprintf("You have used your included %d AI root-cause analyses for this period on Cloud Team and your card has been removed. Add a payment method on /app/billing to continue at $%.2f each, or wait for your next billing period.",
						teamLimit, TeamAIAnalysisOveragePriceUSD))
				return
			}
			// : Team with card past 200 → overage at
			// $0.50/each. Log for observability and continue;
			// invoice.upcoming pushes the InvoiceItem. Customers
			// who want a hard ceiling set BillingCapUSD on
			// /app/billing (same mechanism that protects them on
			// execution overage).
			h.Logger.Info("analyze: team in AI overage",
				"project_id", authProjectID,
				"tenant_id", tenantID,
				"count", count,
				"included", teamLimit,
				"overage_units", count-teamLimit+1,
				"overage_rate_usd", TeamAIAnalysisOveragePriceUSD)
		}
	}

	// Pull a small, recent sample of affected executions to give
	// the model concrete context. We cap deliberately small
	// (3 executions) so the LLM-side input bill stays bounded.
	sampleExecs, err := h.Store.ListExecutionsByFailureGroup(r.Context(), groupID, 3, 0)
	if err != nil {
		h.Logger.Warn("analyze: sample executions failed",
			"group_id", groupID, "error", err.Error())
	}

	// Pull the events for the FIRST (most recent) sample execution so
	// the AI can reference specific tool_call return shapes, llm_call
	// prompts/responses, dlp_scan_result fields, and checkpoint payloads
	// rather than just execution metadata., this is where the
	// AI's analysis becomes structurally different from the playbook:
	// instead of "your agent might have a token_waste pattern" the AI
	// can say "your prompts share this 1.8K-char prefix verbatim across
	// 3 llm_call events; enable prompt caching."
	//
	// Best-effort: if the event fetch fails (transient DB issue, no
	// captured events, etc.) we proceed with empty events and the
	// prompt builder skips the events section. Analysis still runs;
	// it just falls back to the metadata-only (-level) output.
	var sampleEvents []*events.Event
	if len(sampleExecs) > 0 && sampleExecs[0] != nil {
		evs, eerr := h.Store.ListEventsForExecution(r.Context(), sampleExecs[0].ExecutionID)
		if eerr != nil {
			h.Logger.Warn("analyze: sample events fetch failed",
				"group_id", groupID,
				"execution_id", sampleExecs[0].ExecutionID,
				"error", eerr.Error())
		} else {
			sampleEvents = evs
		}
	}

	prompt := buildFailureGroupAnalysisPrompt(
		group, sampleExecs, sampleEvents,
		playbookDocURL(h.docsBaseURL(), group.FailureClass),
	)

	// Capture the playbook signature for staleness tracking
	// (Wave ai-analysis-staleness-tracking). Empty string when
	// the failure_class/signature pair has no registered playbook
	// or its content file is absent, the dashboard treats NULL/
	// empty stored signatures as "outdated, recommend re-analyze."
	playbookSig, _ := playbooks.Signature(group.FailureClass, group.Signature)

	// Per-tier model. Hand-sold tiers (Production, Enterprise) get the
	// stronger model; self-serve tiers get the standard one. `tier` was
	// normalized above. See analysis_model.go for the cost math behind
	// the split.
	model := analysisModelForTier(tier)
	res, err := h.Anthropic.Call(r.Context(), anthropic.CallOptions{
		Model:  model,
		System: analysisSystemPrompt,
		User:   prompt,
		// Per-model cap: the premium model needs room to reason before
		// it writes. See analysisMaxTokensForModel.
		MaxTokens:   analysisMaxTokensForModel(model),
		Temperature: 0.2,
	})
	if err != nil {
		h.Logger.Error("anthropic call failed",
			"group_id", groupID, "error", err.Error())
		writeError(w, http.StatusBadGateway, "AI analysis failed: "+err.Error())
		return
	}

	// An empty completion is a FAILURE, not a success.
	//
	// Without this guard the handler persisted "" as the analysis and
	// returned 200: analyzed_at set, analysis_model set, and no text.
	// The customer clicks "Analyze with AI", waits thirteen seconds,
	// gets a blank card, and on Hobby is billed $0.75 for it. Observed
	// on 2026-08-24 with coordination_deadlock, reproducibly.
	//
	// Log StopReason: "max_tokens" means the output budget was spent
	// before any prose was emitted and the cap for this model needs
	// raising, which is a different fix from a genuine refusal.
	if strings.TrimSpace(res.Text) == "" {
		h.Logger.Error("anthropic returned an empty analysis",
			"group_id", groupID,
			"model", model,
			"stop_reason", res.StopReason,
			"input_tokens", res.InputTokens,
			"output_tokens", res.OutputTokens,
		)
		writeError(w, http.StatusBadGateway,
			"AI analysis returned no content. Nothing was saved or billed. "+
				"Please retry; if it persists, contact support.")
		return
	}

	now := time.Now().UTC()
	if err := h.Store.SaveFailureGroupAnalysis(
		r.Context(), groupID, res.Text, model, now, playbookSig,
	); err != nil {
		h.Logger.Error("save failure_group analysis failed",
			"group_id", groupID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "persist failed: "+err.Error())
		return
	}

	// record one ai_analyses row per call so the founder
	// accounting view sees actual model + token + cost (not just
	// the flat $0.03 estimate the dashboard quotes customers).
	// Best-effort: log and continue on failure so a transient DB
	// error on this insert never breaks the customer's analysis.
	tenantID := ""
	if group.ProjectID != "" {
		if tp, terr := h.Store.GetProjectTenantID(r.Context(), group.ProjectID); terr == nil && tp != nil {
			tenantID = *tp
		}
	}
	cost := anthropic.ComputeCostUSD(model, res.InputTokens, res.OutputTokens)
	if err := h.Store.CreateAIAnalysis(r.Context(), &store.AIAnalysis{
		AnalysisID:        newAIAnalysisID(),
		FailureGroupID:    groupID,
		ProjectID:         group.ProjectID,
		TenantID:          tenantID,
		ModelID:           model,
		InputTokens:       res.InputTokens,
		OutputTokens:      res.OutputTokens,
		CostUSD:           cost,
		GeneratedAt:       now,
		AnalysisMarkdown:  res.Text,
		PlaybookSignature: playbookSig,
	}); err != nil {
		h.Logger.Warn("record ai_analyses row failed (accounting only)",
			"group_id", groupID,
			"input_tokens", res.InputTokens,
			"output_tokens", res.OutputTokens,
			"error", err.Error())
	}

	// Re-read so the response matches what a subsequent GET would
	// return (with the freshly-persisted analysis populated).
	refreshed, err := h.Store.GetFailureGroup(r.Context(), groupID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, refreshed)
}

// newAIAnalysisID returns a stable "ai_<32 hex>" identifier. Uses
// the same random-id pattern as the audit-event id helper so the
// PK shape is consistent across Mesedi tables.
func newAIAnalysisID() string {
	return "ai_" + newAuditEventID()[len("audit_"):]
}

// MaxSampleEventsInPrompt caps how many events from the first sample
// execution we render into the AI analysis prompt., 30 is
// chosen to keep total input tokens under ~10K (~6K for events at
// 200 tokens each + ~3K for playbook + ~500 for metadata + ~500 for
// task instruction) while still capturing the diagnostic-relevant
// portion of most agent traces. Events are emitted in chronological
// order; capping at 30 takes the FIRST 30 by sequence, which is
// usually where the failure is set up. (If the failure is at event
// , the operator should click into the execution directly; the
// AI analysis isn't trying to replace that workflow.)
const MaxSampleEventsInPrompt = 30

// MaxEventPayloadCharsInPrompt caps how much of each event's payload
// is rendered. 500 chars is enough to capture the relevant JSON keys
// + the first value or two; longer payloads get a ...[truncated]
// suffix. Customers needing the full payload click into the execution
// detail page.
const MaxEventPayloadCharsInPrompt = 500

// buildFailureGroupAnalysisPrompt assembles the structured prompt
// sent to the LLM. Kept small so the input bill stays bounded and
// the model's output stays focused.
//
// The prompt has four parts in order:
//
//  1. The canonical Mesedi playbook for the (failure_class, signature)
//     pair, loaded via playbooks.Load. This is the interpretation
//     framework, what the signature means, the named causes, the
//     diagnostic surface, the recommended fixes, the per-project
//     tuning knobs, the related-detector cross-links. Without this,
//     the model produces generic distributed-systems advice that
//     misses Mesedi-specific features (allowlists, thresholds,
//     redaction-at-ingest, topology view, etc.), see audit at
//     internal-extract/ai-analyses-vs-playbooks-audit.md.
//
//  2. The specific failure group's metadata + sample executions.
//
//  3. () Sample events from the first sample execution, with
//     each event's payload truncated to MaxEventPayloadCharsInPrompt
//     chars. This is where the model gets actual data to reason
//     about, tool_call return shapes, llm_call prompts/responses,
//     dlp_scan_result rule IDs + matched fields, checkpoint state.
//
//  4. A task instruction telling the model to APPLY the playbook to
//     this specific failure group rather than invent generic
//     speculation.
//
// If the playbook lookup fails (very rare, every shipping detector
// class has a registered playbook), the prompt falls back to its
// pre-playbook shape so the analysis still runs. If sampleEvents is
// nil/empty (event-fetch failed, or execution has no events),
// the events section is omitted and the prompt looks identical to its
// pre-K.2 shape.
func buildFailureGroupAnalysisPrompt(
	group *store.FailureGroup,
	sampleExecs []*events.Execution,
	sampleEvents []*events.Event,
	playbookURL string,
) string {
	var sb strings.Builder

	// Part 1: the canonical playbook. The model treats this as its
	// interpretation framework; the task instruction below names it
	// explicitly so the model anchors its analysis on the playbook's
	// causes + fixes rather than its own training-data priors.
	playbookContent, playbookErr := playbooks.Load(group.FailureClass, group.Signature)
	if playbookErr == nil && playbookContent != "" {
		sb.WriteString("# Mesedi playbook for this failure class\n\n")
		// Give the model the canonical URL so when it references the
		// playbook (which it does naturally, "this matches the
		// playbook's cause #2") it can link instead of leaving the
		// reader to go find the doc. Omitted entirely when no URL can
		// be built, so the model is never handed a broken link.
		if playbookURL != "" {
			sb.WriteString(fmt.Sprintf(
				"Published at: %s\n\n", playbookURL))
		}
		sb.WriteString(playbookContent)
		sb.WriteString("\n\n---\n\n")
	}

	// Part 2: failure group metadata + sample executions.
	sb.WriteString("# Failure group context\n\n")
	sb.WriteString(fmt.Sprintf("- **failure_class**: %s\n", group.FailureClass))
	sb.WriteString(fmt.Sprintf("- **signature**: %s\n", group.Signature))
	sb.WriteString(fmt.Sprintf("- **first_seen**: %s\n", group.FirstSeen.Format(time.RFC3339)))
	sb.WriteString(fmt.Sprintf("- **last_seen**: %s\n", group.LastSeen.Format(time.RFC3339)))
	sb.WriteString(fmt.Sprintf("- **affected_executions**: %d\n", group.AffectedExecutions))
	sb.WriteString(fmt.Sprintf("- **event_count**: %d\n", group.EventCount))
	if group.CostWastedUSD != nil {
		sb.WriteString(fmt.Sprintf("- **estimated_cost_usd**: %.4f\n", *group.CostWastedUSD))
	}
	if group.SampleExecutionID != "" {
		sb.WriteString(fmt.Sprintf("- **sample_execution_id**: %s\n", group.SampleExecutionID))
	}
	if len(sampleExecs) > 0 {
		sb.WriteString("\n## Sample executions (most recent)\n\n")
		for _, exec := range sampleExecs {
			if exec == nil {
				continue
			}
			sb.WriteString(fmt.Sprintf("### %s\n\n", exec.ExecutionID))
			sb.WriteString(fmt.Sprintf("- status: %s\n", exec.Status))
			sb.WriteString(fmt.Sprintf("- duration_ms: %d\n", exec.DurationMs))
			if exec.CrashSignature != "" {
				sb.WriteString(fmt.Sprintf("- crash_signature: %s\n", exec.CrashSignature))
			}
			if exec.SDKLanguage != "" {
				sb.WriteString(fmt.Sprintf("- sdk_language: %s\n", exec.SDKLanguage))
			}
			sb.WriteString("\n")
		}
	}

	// Part 3 (): sample events from the first execution. This
	// is the data layer that makes the AI analysis structurally
	// different from the static playbook, the model can now point
	// to specific tool_call return shapes, llm_call prompt/response
	// content, dlp_scan_result rule IDs + matched fields, etc.
	//
	// Cap: MaxSampleEventsInPrompt events (chronologically first).
	// Each payload truncated to MaxEventPayloadCharsInPrompt chars
	// with a ...[truncated] suffix. We use the raw JSON bytes from
	// event.Payload so the model sees exactly what the SDK shipped ,
	// no re-serialization, no field selection.
	if len(sampleEvents) > 0 && len(sampleExecs) > 0 && sampleExecs[0] != nil {
		sb.WriteString(fmt.Sprintf("\n## Sample events from execution %s\n\n",
			sampleExecs[0].ExecutionID))
		eventLimit := len(sampleEvents)
		if eventLimit > MaxSampleEventsInPrompt {
			eventLimit = MaxSampleEventsInPrompt
		}
		for i := 0; i < eventLimit; i++ {
			ev := sampleEvents[i]
			if ev == nil {
				continue
			}
			sb.WriteString(fmt.Sprintf("### event %d: %s (seq=%d)\n\n",
				i+1, ev.EventType, ev.Sequence))
			sb.WriteString(fmt.Sprintf("- timestamp: %s\n", ev.Timestamp.Format(time.RFC3339)))
			if ev.DurationMs > 0 {
				sb.WriteString(fmt.Sprintf("- duration_ms: %d\n", ev.DurationMs))
			}
			payloadStr := string(ev.Payload)
			if len(payloadStr) > MaxEventPayloadCharsInPrompt {
				payloadStr = payloadStr[:MaxEventPayloadCharsInPrompt] + "...[truncated]"
			}
			if payloadStr != "" {
				sb.WriteString(fmt.Sprintf("- payload: %s\n", payloadStr))
			}
			sb.WriteString("\n")
		}
		if len(sampleEvents) > MaxSampleEventsInPrompt {
			sb.WriteString(fmt.Sprintf("_(showing first %d of %d events; click into the execution for the full timeline)_\n\n",
				MaxSampleEventsInPrompt, len(sampleEvents)))
		}
	}

	// Part 4: the task instruction. Phrasing is deliberate, the
	// model is told to USE the playbook as its framework, not to
	// invent a root cause from scratch. Two-hypothesis remediation
	// stays the existing contract; the difference is that the
	// hypotheses are now drawn from the playbook's named fixes
	// applied to this customer's specific data, not from generic
	// distributed-systems training.
	sb.WriteString("\n## Task\n\n")
	if playbookErr == nil && playbookContent != "" {
		sb.WriteString("Use the Mesedi playbook above as your interpretation framework. " +
			"For this specific failure group, identify which of the playbook's " +
			"named causes most plausibly fits the signature and sample executions, " +
			"then propose two concrete remediation hypotheses drawn from the " +
			"playbook's recommended fixes: applied to this customer's specific " +
			"data, not stated in the abstract. Reference the playbook's " +
			"diagnostic surfaces (events to read, fields to inspect, related " +
			"detectors to cross-check, per-project knobs to tune) when they " +
			"apply. If the sample executions are too sparse to discriminate " +
			"between the playbook's causes, say so explicitly and name the " +
			"specific additional data the operator should collect.\n\n")
	}
	sb.WriteString("Write a short Markdown analysis with three sections:\n")
	sb.WriteString("1. **Likely cause**: one paragraph naming the most plausible root cause given the signature and the sample executions.\n")
	sb.WriteString("2. **Two remediation hypotheses**: two bullet items, each a concrete action the operator can test.\n")
	sb.WriteString("3. **Confidence**: one line: how confident you are (low / medium / high) and the single biggest unknown.\n")
	sb.WriteString("\nKeep the entire output under 250 words. Do not include a disclaimer about the analysis being non-authoritative; the dashboard already renders one.\n")
	return sb.String()
}
