// Failure group records: listing, resolution, analyses, severity hints.
// Split out of postgres.go on 2026-09-12; every declaration moved verbatim.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// UpdateFailureGroupSeverityHint, Postgres twin (validator_failures.G1).
func (s *PostgresStore) UpdateFailureGroupSeverityHint(
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
		SET severity_hint = $1
		WHERE group_id = $2
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

// GetFailureGroupSeverityHint, Postgres twin.
func (s *PostgresStore) GetFailureGroupSeverityHint(
	ctx context.Context,
	groupID string,
) (string, error) {
	var hint sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT severity_hint FROM failure_groups WHERE group_id = $1
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

func (s *PostgresStore) ListFailureGroups(ctx context.Context, projectID string, opts ListFailureGroupsOpts) ([]*FailureGroup, error) {
	// Search + resolved-visibility filters (list-search-paginate
	// + failure-group-resolve waves). When Q is non-empty: ILIKE
	// substring on signature + failure_class. When IncludeResolved
	// is false (default): WHERE resolved_at IS NULL hides resolved
	// rows. ILIKE is Postgres' native case-insensitive substring;
	// the SQLite twin uses LOWER() + LIKE for the same effect.
	args := []any{projectID}
	whereClause := "fg.project_id = $1"
	if opts.Q != "" {
		whereClause += " AND (fg.signature ILIKE '%' || $2 || '%'" +
			" OR fg.failure_class ILIKE '%' || $2 || '%')"
		args = append(args, opts.Q)
	}
	if !opts.IncludeResolved {
		whereClause += " AND fg.resolved_at IS NULL"
	}
	args = append(args, opts.Limit, opts.Offset)
	limitPlaceholder := fmt.Sprintf("$%d", len(args)-1)
	offsetPlaceholder := fmt.Sprintf("$%d", len(args))

	// G202: whereClause is built above from allowlisted fragments
	// (project_id / severity / resolved-status filters). limitPlaceholder
	// and offsetPlaceholder are server-controlled $N tokens, never user
	// input. All user-supplied values flow through args... as
	// parameterized placeholders.
	//nolint:gosec // G202: false positive, see comment above.
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
		GROUP BY fg.group_id, fg.project_id, fg.failure_class, fg.signature,
		         fg.first_seen, fg.last_seen, fg.event_count,
		         fg.affected_executions, fg.sample_execution_id,
		         fg.analysis_markdown, fg.analyzed_at, fg.analysis_model,
		         fg.analysis_playbook_signature,
		         fg.severity_hint, fg.resolved_at, fg.resolved_by
		ORDER BY fg.last_seen DESC
		LIMIT `+limitPlaceholder+` OFFSET `+offsetPlaceholder+`
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

// ResolveFailureGroup, Postgres twin of SQLiteStore.ResolveFailureGroup.
// See that method's doc comment.
func (s *PostgresStore) ResolveFailureGroup(
	ctx context.Context,
	groupID, projectID, actorUserID string,
) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE failure_groups
		SET resolved_at = NOW(), resolved_by = $1
		WHERE group_id = $2 AND project_id = $3
	`, actorUserID, groupID, projectID)
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

// UnresolveFailureGroup, Postgres twin of
// SQLiteStore.UnresolveFailureGroup. See that method's doc comment.
func (s *PostgresStore) UnresolveFailureGroup(
	ctx context.Context,
	groupID, projectID string,
) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE failure_groups
		SET resolved_at = NULL, resolved_by = NULL
		WHERE group_id = $1 AND project_id = $2
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

func (s *PostgresStore) GetFailureGroup(ctx context.Context, groupID string) (*FailureGroup, error) {
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
		WHERE fg.group_id = $1
		GROUP BY fg.group_id, fg.project_id, fg.failure_class, fg.signature,
		         fg.first_seen, fg.last_seen, fg.event_count,
		         fg.affected_executions, fg.sample_execution_id,
		         fg.analysis_markdown, fg.analyzed_at, fg.analysis_model,
		         fg.analysis_playbook_signature,
		         fg.severity_hint, fg.resolved_at, fg.resolved_by
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

// SaveFailureGroupAnalysis is the Postgres twin of the SQLite method
// of the same name.
func (s *PostgresStore) SaveFailureGroupAnalysis(
	ctx context.Context,
	groupID, analysisMarkdown, analysisModel string,
	analyzedAt time.Time,
	playbookSignature string,
) error {
	var sig any
	if playbookSignature == "" {
		sig = nil
	} else {
		sig = playbookSignature
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE failure_groups
		SET analysis_markdown           = $1,
		    analyzed_at                 = $2,
		    analysis_model              = $3,
		    analysis_playbook_signature = $4
		WHERE group_id = $5
	`, analysisMarkdown, analyzedAt, analysisModel, sig, groupID)
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
// tenant_id (legacy unbackfilled row). Postgres twin of the SQLite
// method.
func (s *PostgresStore) CountAIAnalysesSincePeriodStart(
	ctx context.Context, projectID string, since time.Time,
) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM failure_groups
		WHERE project_id = $1
		  AND analyzed_at IS NOT NULL
		  AND analyzed_at >= $2
	`, projectID, since).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count ai analyses since: %w", err)
	}
	return count, nil
}

// ListAIAnalysesUsageByProject is the Postgres twin of the SQLite
// method. See sqlite.go for the contract.
func (s *PostgresStore) ListAIAnalysesUsageByProject(
	ctx context.Context, since time.Time,
) ([]*AIAnalysesByProjectRow, error) {
	// filter chips: string_agg(DISTINCT ...) is the Postgres
	// twin of SQLite's group_concat(DISTINCT). Returns the comma-
	// joined list of failure_class slugs; splitFailureClassesCSV
	// parses it into a deduped slice.
	rows, err := s.db.QueryContext(ctx, `
		SELECT fg.project_id, p.name, p.owner_email, p.tier, p.tenant_id,
		       COUNT(*) AS n,
		       string_agg(DISTINCT fg.failure_class, ',') AS classes
		FROM failure_groups fg
		JOIN projects p ON p.project_id = fg.project_id
		WHERE fg.analyzed_at IS NOT NULL
		  AND fg.analyzed_at >= $1
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

// ListAnalyzedFailureGroupsByProject is the Postgres twin of the
// SQLite method. See sqlite.go for the contract.
//
// Uses the canonical scanFailureGroup helper because first_seen and
// last_seen are TEXT in the Postgres schema (not TIMESTAMP), and the
// driver returns them as strings; scanning directly into time.Time
// throws "storing driver.Value type string into type *time.Time"
// against the live Neon database.
func (s *PostgresStore) ListAnalyzedFailureGroupsByProject(
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
		WHERE project_id = $1
		  AND analyzed_at IS NOT NULL
		  AND analyzed_at >= $2
		ORDER BY analyzed_at DESC
		LIMIT $3
	`, projectID, since, limit)
	if err != nil {
		return nil, fmt.Errorf("list analyzed failure groups (postgres): %w", err)
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
// every project owned by tenantID whose analyzed_at >= since.
// Canonical Team-tier rate-limit query. Postgres twin of the
// SQLite method.
func (s *PostgresStore) CountAIAnalysesByTenantSince(
	ctx context.Context, tenantID string, since time.Time,
) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM failure_groups fg
		JOIN projects p ON p.project_id = fg.project_id
		WHERE p.tenant_id = $1
		  AND fg.analyzed_at IS NOT NULL
		  AND fg.analyzed_at >= $2
	`, tenantID, since).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count ai analyses by tenant since: %w", err)
	}
	return count, nil
}

func (s *PostgresStore) GetFailureGroupByClassSignature(ctx context.Context, projectID, failureClass, signature string) (*FailureGroup, error) {
	groupID := deriveGroupID(projectID, failureClass, signature)
	return s.GetFailureGroup(ctx, groupID)
}
