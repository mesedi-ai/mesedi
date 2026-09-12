// Failure group records: listing, resolution, analyses, severity hints.
// Split out of sqlite.go on 2026-09-12; every declaration moved verbatim.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// UpdateFailureGroupSeverityHint writes severity_hint on an existing
// row. Returns ErrNotFound when groupID doesn't exist.
func (s *SQLiteStore) UpdateFailureGroupSeverityHint(
	ctx context.Context,
	groupID string,
	severityHint string,
) error {
	if groupID == "" {
		return fmt.Errorf("groupID required")
	}
	var hint sql.NullString
	if severityHint != "" {
		hint = sql.NullString{String: severityHint, Valid: true}
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE failure_groups
		SET severity_hint = ?
		WHERE group_id = ?
	`, hint, groupID)
	if err != nil {
		return fmt.Errorf("update failure_group severity_hint: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetFailureGroupSeverityHint returns the per-group severity hint
// or empty string when none was set.
func (s *SQLiteStore) GetFailureGroupSeverityHint(
	ctx context.Context,
	groupID string,
) (string, error) {
	var hint sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT severity_hint FROM failure_groups WHERE group_id = ?
	`, groupID).Scan(&hint)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get failure_group severity_hint: %w", err)
	}
	if !hint.Valid {
		return "", nil
	}
	return hint.String, nil
}

// ListFailureGroups returns failure_groups for a project, sorted by
// most-recent first. Caller is responsible for sensible limit/offset
// bounds (handler enforces a max-limit ceiling).
//
// cost_wasted_usd is computed live as SUM(executions.estimated_cost_usd)
// across all executions linked to the group. The stored
// failure_groups.cost_wasted_usd column is currently unused, kept for
// a future "manual override / human-adjusted" path. For now the
// computed sum always wins.
func (s *SQLiteStore) ListFailureGroups(
	ctx context.Context,
	projectID string,
	opts ListFailureGroupsOpts,
) ([]*FailureGroup, error) {
	// Search filter (list-search-paginate wave): when Q is non-empty,
	// restrict to rows whose signature OR failure_class contains it,
	// case-insensitively. Resolved-visibility filter
	// (failure-group-resolve wave): when IncludeResolved is false
	// (default), drop rows with non-NULL resolved_at. Parameterized
	//, safe against injection. Empty opts skips both predicates so
	// internal callers stay on the unfiltered fast path.
	args := []any{projectID}
	whereClause := "fg.project_id = ?"
	if opts.Q != "" {
		whereClause += " AND (LOWER(fg.signature) LIKE '%' || LOWER(?) || '%'" +
			" OR LOWER(fg.failure_class) LIKE '%' || LOWER(?) || '%')"
		args = append(args, opts.Q, opts.Q)
	}
	if !opts.IncludeResolved {
		whereClause += " AND fg.resolved_at IS NULL"
	}
	args = append(args, opts.Limit, opts.Offset)

	rows, err := s.db.QueryContext(ctx, `
		SELECT
			fg.group_id, fg.project_id, fg.failure_class, fg.signature,
			fg.first_seen, fg.last_seen,
			fg.event_count, fg.affected_executions,
			COALESCE(SUM(e.estimated_cost_usd), 0) AS computed_cost,
			COALESCE(SUM(e.total_tokens_in), 0) AS computed_tokens_in,
			COALESCE(SUM(e.total_tokens_out), 0) AS computed_tokens_out,
			fg.sample_execution_id,
			fg.analysis_markdown, fg.analyzed_at, fg.analysis_model,
			fg.analysis_playbook_signature,
			fg.severity_hint,
			fg.resolved_at, fg.resolved_by
		FROM failure_groups fg
		LEFT JOIN executions e ON e.failure_group_id = fg.group_id
		WHERE `+whereClause+`
		GROUP BY fg.group_id
		ORDER BY fg.last_seen DESC
		LIMIT ? OFFSET ?
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("query failure_groups: %w", err)
	}
	defer rows.Close()

	var out []*FailureGroup
	for rows.Next() {
		g, err := scanFailureGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GetFailureGroup returns a single failure_group by its deterministic id.
// Same cost-computation path as ListFailureGroups.
func (s *SQLiteStore) GetFailureGroup(
	ctx context.Context,
	groupID string,
) (*FailureGroup, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT
			fg.group_id, fg.project_id, fg.failure_class, fg.signature,
			fg.first_seen, fg.last_seen,
			fg.event_count, fg.affected_executions,
			COALESCE(SUM(e.estimated_cost_usd), 0) AS computed_cost,
			COALESCE(SUM(e.total_tokens_in), 0) AS computed_tokens_in,
			COALESCE(SUM(e.total_tokens_out), 0) AS computed_tokens_out,
			fg.sample_execution_id,
			fg.analysis_markdown, fg.analyzed_at, fg.analysis_model,
			fg.analysis_playbook_signature,
			fg.severity_hint,
			fg.resolved_at, fg.resolved_by
		FROM failure_groups fg
		LEFT JOIN executions e ON e.failure_group_id = fg.group_id
		WHERE fg.group_id = ?
		GROUP BY fg.group_id
	`, groupID)
	g, err := scanFailureGroup(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query failure_group: %w", err)
	}
	return g, nil
}

// ResolveFailureGroup marks the group resolved (sets resolved_at +
// resolved_by). Tenant-scoped: the WHERE clause requires both
// group_id AND project_id, so a resolve attempt on another
// project's group returns ErrNotFound, no leak of group_id
// existence across tenants (same pattern as GetFailureGroup).
// Idempotent: re-resolving refreshes the timestamp.
func (s *SQLiteStore) ResolveFailureGroup(
	ctx context.Context,
	groupID, projectID, actorUserID string,
) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE failure_groups
		SET resolved_at = ?, resolved_by = ?
		WHERE group_id = ? AND project_id = ?
	`, time.Now().UTC().Format(time.RFC3339), actorUserID, groupID, projectID)
	if err != nil {
		return fmt.Errorf("resolve failure_group: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("resolve failure_group rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// UnresolveFailureGroup clears resolved_at + resolved_by. Same
// tenant-scope contract as ResolveFailureGroup. Idempotent.
func (s *SQLiteStore) UnresolveFailureGroup(
	ctx context.Context,
	groupID, projectID string,
) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE failure_groups
		SET resolved_at = NULL, resolved_by = NULL
		WHERE group_id = ? AND project_id = ?
	`, groupID, projectID)
	if err != nil {
		return fmt.Errorf("unresolve failure_group: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("unresolve failure_group rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetFailureGroupByClassSignature returns a failure_group by its
// natural key (project_id, failure_class, signature). Used by the
// webhook dispatcher to fetch the canonical sample_execution_id for
// the payload at first-occurrence time.
func (s *SQLiteStore) GetFailureGroupByClassSignature(
	ctx context.Context,
	projectID, failureClass, signature string,
) (*FailureGroup, error) {
	groupID := deriveGroupID(projectID, failureClass, signature)
	return s.GetFailureGroup(ctx, groupID)
}

func scanFailureGroup(r rowScanner) (*FailureGroup, error) {
	var (
		g                FailureGroup
		firstSeen        string
		lastSeen         string
		costWasted       sql.NullFloat64
		tokensIn         sql.NullInt64
		tokensOut        sql.NullInt64
		sampleID         sql.NullString
		analysisMarkdown sql.NullString
		analyzedAt       sql.NullTime
		analysisModel    sql.NullString
		playbookSig      sql.NullString
		severityHint     sql.NullString
		resolvedAt       sql.NullTime
		resolvedBy       sql.NullString
	)
	if err := r.Scan(
		&g.GroupID,
		&g.ProjectID,
		&g.FailureClass,
		&g.Signature,
		&firstSeen,
		&lastSeen,
		&g.EventCount,
		&g.AffectedExecutions,
		&costWasted,
		&tokensIn,
		&tokensOut,
		&sampleID,
		&analysisMarkdown,
		&analyzedAt,
		&analysisModel,
		&playbookSig,
		&severityHint,
		&resolvedAt,
		&resolvedBy,
	); err != nil {
		return nil, err
	}
	g.FirstSeen, _ = time.Parse(time.RFC3339, firstSeen)
	g.LastSeen, _ = time.Parse(time.RFC3339, lastSeen)
	if costWasted.Valid && costWasted.Float64 > 0 {
		// Only surface a positive computed cost. The COALESCE on the
		// SQL side makes Valid always true, so this prevents zero
		// values from leaking into the JSON as "cost_wasted_usd: 0"
		// when there's no actual cost to show.
		v := costWasted.Float64
		g.CostWastedUSD = &v
	}
	// Same "only-if-positive" pattern for the token rollups. COALESCE
	// on the SQL side makes Valid always true; the >0 guard prevents
	// zero-token rows (e.g. groups whose executions never made an LLM
	// call, like sandbox_escape or coordination_deadlock) from
	// leaking as "total_tokens_in: 0" in the JSON. Dashboard's
	// failureClassMetricPolicy() then decides whether to render the
	// field even when present, based on the failure_class tier.
	if tokensIn.Valid && tokensIn.Int64 > 0 {
		v := tokensIn.Int64
		g.TotalTokensIn = &v
	}
	if tokensOut.Valid && tokensOut.Int64 > 0 {
		v := tokensOut.Int64
		g.TotalTokensOut = &v
	}
	if (tokensIn.Valid && tokensIn.Int64 > 0) || (tokensOut.Valid && tokensOut.Int64 > 0) {
		v := tokensIn.Int64 + tokensOut.Int64
		g.TotalTokens = &v
	}
	if sampleID.Valid {
		g.SampleExecutionID = sampleID.String
	}
	if analysisMarkdown.Valid && analysisMarkdown.String != "" {
		v := analysisMarkdown.String
		g.AnalysisMarkdown = &v
	}
	if analyzedAt.Valid {
		t := analyzedAt.Time
		g.AnalyzedAt = &t
	}
	if analysisModel.Valid && analysisModel.String != "" {
		v := analysisModel.String
		g.AnalysisModel = &v
	}
	if playbookSig.Valid && playbookSig.String != "" {
		v := playbookSig.String
		g.AnalysisPlaybookSignature = &v
	}
	if severityHint.Valid && severityHint.String != "" {
		v := severityHint.String
		g.SeverityHint = &v
	}
	if resolvedAt.Valid {
		t := resolvedAt.Time
		g.ResolvedAt = &t
	}
	if resolvedBy.Valid && resolvedBy.String != "" {
		v := resolvedBy.String
		g.ResolvedBy = &v
	}
	return &g, nil
}

// SaveFailureGroupAnalysis persists the LLM-generated root-cause
// analysis on a failure_group row. Idempotent overwrite:
// repeated calls replace the previous analysis. Returns ErrNotFound
// when the group_id does not exist.
func (s *SQLiteStore) SaveFailureGroupAnalysis(
	ctx context.Context,
	groupID, analysisMarkdown, analysisModel string,
	analyzedAt time.Time,
	playbookSignature string,
) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE failure_groups
		SET analysis_markdown            = ?,
		    analyzed_at                  = ?,
		    analysis_model               = ?,
		    analysis_playbook_signature  = ?
		WHERE group_id = ?
	`, analysisMarkdown, analyzedAt, analysisModel,
		nullString(playbookSignature), groupID)
	if err != nil {
		return fmt.Errorf("save failure_group analysis: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// CountAIAnalysesSincePeriodStart counts failure_groups for projectID
// whose analyzed_at >= since. Fallback when a project has no
// tenant_id (legacy unbackfilled row). For all other projects the
// canonical query is CountAIAnalysesByTenantSince.
func (s *SQLiteStore) CountAIAnalysesSincePeriodStart(
	ctx context.Context, projectID string, since time.Time,
) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM failure_groups
		WHERE project_id = ?
		  AND analyzed_at IS NOT NULL
		  AND analyzed_at >= ?
	`, projectID, since).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count ai analyses since: %w", err)
	}
	return count, nil
}

// ListAIAnalysesUsageByProject returns per-project AI analysis
// counts for the admin breakdown view. One row per project
// with at least one analyzed failure_group since `since`. Sorted
// by count descending. The join hits failure_groups (small table)
// and projects (small table); ungroupable without a project_id
// because we LEFT JOIN projects to surface name/owner/tier even
// when failure_groups.project_id no longer resolves (shouldn't
// happen given FK constraints but guards us if it ever does).
func (s *SQLiteStore) ListAIAnalysesUsageByProject(
	ctx context.Context, since time.Time,
) ([]*AIAnalysesByProjectRow, error) {
	// filter chips: group_concat(DISTINCT) returns the comma-
	// joined list of failure_class slugs this project ran analyses
	// against in the window. Empty string when none (shouldn't
	// happen given the WHERE clause but defensive). Frontend splits
	// the CSV.
	rows, err := s.db.QueryContext(ctx, `
		SELECT fg.project_id, p.name, p.owner_email, p.tier, p.tenant_id,
		       COUNT(*) AS n,
		       group_concat(DISTINCT fg.failure_class) AS classes
		FROM failure_groups fg
		JOIN projects p ON p.project_id = fg.project_id
		WHERE fg.analyzed_at IS NOT NULL
		  AND fg.analyzed_at >= ?
		GROUP BY fg.project_id, p.name, p.owner_email, p.tier, p.tenant_id
		ORDER BY n DESC, p.name ASC
	`, since)
	if err != nil {
		return nil, fmt.Errorf("list ai analyses by project: %w", err)
	}
	defer rows.Close()

	out := make([]*AIAnalysesByProjectRow, 0, 8)
	for rows.Next() {
		r := &AIAnalysesByProjectRow{}
		var email, tenantID, classes sql.NullString
		if err := rows.Scan(&r.ProjectID, &r.Name, &email, &r.Tier, &tenantID, &r.Count, &classes); err != nil {
			return nil, fmt.Errorf("scan ai analyses by project row: %w", err)
		}
		if email.Valid {
			r.OwnerEmail = email.String
		}
		if tenantID.Valid {
			r.TenantID = tenantID.String
		}
		r.FailureClasses = splitFailureClassesCSV(classes)
		out = append(out, r)
	}
	return out, rows.Err()
}

// splitFailureClassesCSV parses the comma-delimited string from
// group_concat / string_agg into a deduped + non-empty slice. Same
// helper used by both sqlite + postgres so the slice contract is
// identical regardless of driver.
func splitFailureClassesCSV(s sql.NullString) []string {
	if !s.Valid || s.String == "" {
		return nil
	}
	parts := strings.Split(s.String, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		v := strings.TrimSpace(p)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// ListAnalyzedFailureGroupsByProject returns one row per failure
// group on the given project that has been analyzed (analyzed_at
// IS NOT NULL) on or after `since`. Used by the admin AI analyses
// breakdown so the founder can click a project row and see
// WHICH failure groups generated the count, not just the total.
// Ordered analyzed_at DESC so the most recent analysis lands at
// the top. Default limit 200 covers a heavy month even for the
// largest projected customer; callers can pass 0 to use the default.
//
// Column order MUST match scanFailureGroup so the canonical helper
// can do the string -> time.Time parse for first_seen / last_seen.
// Those columns are stored as TEXT on the Postgres side (latent
// scan-bug fix); scanning directly into time.Time blew up the live
// driver with "storing driver.Value type string into type *time.Time".
func (s *SQLiteStore) ListAnalyzedFailureGroupsByProject(
	ctx context.Context, projectID string, since time.Time, limit int,
) ([]*FailureGroup, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT group_id, project_id, failure_class, signature,
		       first_seen, last_seen, event_count, affected_executions,
		       cost_wasted_usd, sample_execution_id,
		       analysis_markdown, analyzed_at, analysis_model,
		       analysis_playbook_signature,
		       severity_hint
		FROM failure_groups
		WHERE project_id = ?
		  AND analyzed_at IS NOT NULL
		  AND analyzed_at >= ?
		ORDER BY analyzed_at DESC
		LIMIT ?
	`, projectID, since, limit)
	if err != nil {
		return nil, fmt.Errorf("list analyzed failure groups: %w", err)
	}
	defer rows.Close()

	out := make([]*FailureGroup, 0, 8)
	for rows.Next() {
		g, err := scanFailureGroup(rows)
		if err != nil {
			return nil, fmt.Errorf("scan analyzed failure group: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// CountAIAnalysesByTenantSince counts failure_groups summed across
// every project owned by tenantID whose analyzed_at >= since. This
// is the canonical Team-tier rate-limit query because the cap is
// per-organization per-period, not per-project; without the JOIN
// here a Team customer could trivially bypass the cap by spawning
// additional projects under the same org.
func (s *SQLiteStore) CountAIAnalysesByTenantSince(
	ctx context.Context, tenantID string, since time.Time,
) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM failure_groups fg
		JOIN projects p ON p.project_id = fg.project_id
		WHERE p.tenant_id = ?
		  AND fg.analyzed_at IS NOT NULL
		  AND fg.analyzed_at >= ?
	`, tenantID, since).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count ai analyses by tenant since: %w", err)
	}
	return count, nil
}
