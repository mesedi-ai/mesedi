package store

// Anonymized failure_class aggregates for monthly LinkedIn trend
// reports. See migrations/027_failure_class_aggregates.sql
// for design + privacy rationale.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// FailureClassAggregateRow is one row of the
// failure_class_aggregates table. Shape returned to the founder
// LinkedIn-drafting surface; never surfaced to customer-facing
// dashboards because the cross-tenant aggregate would itself be
// information disclosure.
type FailureClassAggregateRow struct {
	PeriodYearMonth         string    `json:"period_year_month"`
	FailureClass            string    `json:"failure_class"`
	DistinctTenantsCount    int       `json:"distinct_tenants_count"`
	DistinctProjectsCount   int       `json:"distinct_projects_count"`
	FailureGroupsCount      int       `json:"failure_groups_count"`
	AffectedExecutionsCount int64     `json:"affected_executions_count"`
	AggregatedAt            time.Time `json:"aggregated_at"`
}

// --- SQLite impl --------------------------------------------------

// AggregateFailureClassesForMonth recomputes the per-class counts
// for the given "YYYY-MM" period and upserts them into the
// failure_class_aggregates table. Idempotent: running twice for
// the same period replaces the row with fresh counts.
//
// startInclusive + endExclusive define the window; the caller is
// expected to pass the first-instant-of-month and
// first-instant-of-next-month so the boundary math stays in api/.
//
// Performance: one GROUP BY scan of failure_groups joined with
// projects. The dataset is small (1 row per failure_group) so this
// is a cheap query even at scale.
func (s *SQLiteStore) AggregateFailureClassesForMonth(
	ctx context.Context,
	period string,
	startInclusive time.Time,
	endExclusive time.Time,
) (int, error) {
	if period == "" {
		return 0, errors.New("period required (YYYY-MM)")
	}
	// SELECT into temp, then UPSERT one row at a time. Doing it in
	// a single INSERT ... SELECT works on both drivers but the
	// loop is clearer and the row count is small.
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			fg.failure_class,
			COUNT(DISTINCT p.tenant_id) AS distinct_tenants,
			COUNT(DISTINCT fg.project_id) AS distinct_projects,
			COUNT(*) AS group_count,
			COALESCE(SUM(fg.affected_executions), 0) AS affected_execs
		FROM failure_groups fg
		JOIN projects p ON p.project_id = fg.project_id
		WHERE fg.first_seen >= ? AND fg.first_seen < ?
		GROUP BY fg.failure_class
	`, startInclusive.UTC().Format(time.RFC3339), endExclusive.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("aggregate failure classes: %w", err)
	}
	defer rows.Close()

	now := time.Now().UTC()
	written := 0
	for rows.Next() {
		var fc string
		var tenants, projects, groups int
		var execs int64
		if err := rows.Scan(&fc, &tenants, &projects, &groups, &execs); err != nil {
			return 0, fmt.Errorf("scan aggregate row: %w", err)
		}
		_, err := s.db.ExecContext(ctx, `
			INSERT OR REPLACE INTO failure_class_aggregates (
				period_year_month, failure_class,
				distinct_tenants_count, distinct_projects_count,
				failure_groups_count, affected_executions_count,
				aggregated_at
			)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, period, fc, tenants, projects, groups, execs, now)
		if err != nil {
			return 0, fmt.Errorf("upsert aggregate: %w", err)
		}
		written++
	}
	return written, rows.Err()
}

// ListFailureClassAggregates returns aggregates ordered period DESC,
// failure_class ASC. Pass kAnonymity > 0 to drop rows below the
// threshold (recommended for any caller that will surface the data
// publicly). kAnonymity = 0 returns everything for internal review.
// Limit caps the number of rows; pass 0 for the default (500).
func (s *SQLiteStore) ListFailureClassAggregates(
	ctx context.Context, kAnonymity, limit int,
) ([]*FailureClassAggregateRow, error) {
	if limit <= 0 {
		limit = 500
	}
	q := `
		SELECT period_year_month, failure_class,
		       distinct_tenants_count, distinct_projects_count,
		       failure_groups_count, affected_executions_count,
		       aggregated_at
		FROM failure_class_aggregates
		WHERE distinct_tenants_count >= ?
		ORDER BY period_year_month DESC, failure_class ASC
		LIMIT ?
	`
	rows, err := s.db.QueryContext(ctx, q, kAnonymity, limit)
	if err != nil {
		return nil, fmt.Errorf("list failure class aggregates: %w", err)
	}
	defer rows.Close()
	return scanFailureClassAggregateRows(rows)
}

// --- Postgres impl ------------------------------------------------

// AggregateFailureClassesForMonth is the Postgres twin.
func (s *PostgresStore) AggregateFailureClassesForMonth(
	ctx context.Context,
	period string,
	startInclusive time.Time,
	endExclusive time.Time,
) (int, error) {
	if period == "" {
		return 0, errors.New("period required (YYYY-MM)")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			fg.failure_class,
			COUNT(DISTINCT p.tenant_id) AS distinct_tenants,
			COUNT(DISTINCT fg.project_id) AS distinct_projects,
			COUNT(*) AS group_count,
			COALESCE(SUM(fg.affected_executions), 0) AS affected_execs
		FROM failure_groups fg
		JOIN projects p ON p.project_id = fg.project_id
		WHERE fg.first_seen >= $1 AND fg.first_seen < $2
		GROUP BY fg.failure_class
	`, startInclusive.UTC().Format(time.RFC3339), endExclusive.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("aggregate failure classes (postgres): %w", err)
	}
	defer rows.Close()

	now := time.Now().UTC()
	written := 0
	for rows.Next() {
		var fc string
		var tenants, projects, groups int
		var execs int64
		if err := rows.Scan(&fc, &tenants, &projects, &groups, &execs); err != nil {
			return 0, fmt.Errorf("scan aggregate row: %w", err)
		}
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO failure_class_aggregates (
				period_year_month, failure_class,
				distinct_tenants_count, distinct_projects_count,
				failure_groups_count, affected_executions_count,
				aggregated_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (period_year_month, failure_class) DO UPDATE SET
				distinct_tenants_count = EXCLUDED.distinct_tenants_count,
				distinct_projects_count = EXCLUDED.distinct_projects_count,
				failure_groups_count = EXCLUDED.failure_groups_count,
				affected_executions_count = EXCLUDED.affected_executions_count,
				aggregated_at = EXCLUDED.aggregated_at
		`, period, fc, tenants, projects, groups, execs, now)
		if err != nil {
			return 0, fmt.Errorf("upsert aggregate (postgres): %w", err)
		}
		written++
	}
	return written, rows.Err()
}

// ListFailureClassAggregates is the Postgres twin.
func (s *PostgresStore) ListFailureClassAggregates(
	ctx context.Context, kAnonymity, limit int,
) ([]*FailureClassAggregateRow, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT period_year_month, failure_class,
		       distinct_tenants_count, distinct_projects_count,
		       failure_groups_count, affected_executions_count,
		       aggregated_at
		FROM failure_class_aggregates
		WHERE distinct_tenants_count >= $1
		ORDER BY period_year_month DESC, failure_class ASC
		LIMIT $2
	`, kAnonymity, limit)
	if err != nil {
		return nil, fmt.Errorf("list failure class aggregates (postgres): %w", err)
	}
	defer rows.Close()
	return scanFailureClassAggregateRows(rows)
}

// --- shared scan helper -------------------------------------------

func scanFailureClassAggregateRows(rows *sql.Rows) ([]*FailureClassAggregateRow, error) {
	out := make([]*FailureClassAggregateRow, 0, 16)
	for rows.Next() {
		r := &FailureClassAggregateRow{}
		if err := rows.Scan(
			&r.PeriodYearMonth, &r.FailureClass,
			&r.DistinctTenantsCount, &r.DistinctProjectsCount,
			&r.FailureGroupsCount, &r.AffectedExecutionsCount,
			&r.AggregatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan aggregate row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
