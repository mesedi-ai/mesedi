package store

// AI analyses history table backing the founder cost-attribution
// surface (#199). See migrations/026_ai_analyses.sql for design
// rationale.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AIAnalysis is one row in the ai_analyses table — one call to the
// Anthropic Messages API by HandleAnalyzeFailureGroup. Re-runs
// produce additional rows so the founder accounting view shows
// every call, including ones whose markdown was later overwritten
// on the parent failure_groups row.
type AIAnalysis struct {
	AnalysisID       string    `json:"analysis_id"`
	FailureGroupID   string    `json:"failure_group_id"`
	ProjectID        string    `json:"project_id"`
	TenantID         string    `json:"tenant_id,omitempty"`
	ModelID          string    `json:"model_id"`
	InputTokens      int       `json:"input_tokens"`
	OutputTokens     int       `json:"output_tokens"`
	CostUSD          float64   `json:"cost_usd"`
	GeneratedAt      time.Time `json:"generated_at"`
	AnalysisMarkdown string    `json:"analysis_markdown,omitempty"`
	// Joined fields, populated by ListAIAnalyses for the founder UI.
	// Empty when reading via single-row paths.
	ProjectName string `json:"project_name,omitempty"`
	OwnerEmail  string `json:"owner_email,omitempty"`
	Tier        string `json:"tier,omitempty"`
	// FailureClass + Signature are pulled from the joined
	// failure_groups row so the founder sees what each call was
	// analyzing without a follow-up query.
	FailureClass string `json:"failure_class,omitempty"`
	Signature    string `json:"signature,omitempty"`
}

// AIAnalysesTotals is the lifetime + window summary (#199 top tile).
type AIAnalysesTotals struct {
	LifetimeCount       int     `json:"lifetime_count"`
	LifetimeCostUSD     float64 `json:"lifetime_cost_usd"`
	LifetimeInputTokens int64   `json:"lifetime_input_tokens"`
	LifetimeOutputTokens int64  `json:"lifetime_output_tokens"`
	MonthCount          int     `json:"month_count"`
	MonthCostUSD        float64 `json:"month_cost_usd"`
}

// --- SQLite impl --------------------------------------------------

// CreateAIAnalysis inserts one analysis row. Best-effort caller
// pattern: the customer-facing HandleAnalyzeFailureGroup writes the
// failure_groups cache first (so customers see their analysis even
// if the accounting row fails) and then calls this. A failure here
// logs a warning but does NOT roll back the analysis itself.
func (s *SQLiteStore) CreateAIAnalysis(
	ctx context.Context, a *AIAnalysis,
) error {
	if a == nil {
		return errors.New("nil analysis")
	}
	if a.AnalysisID == "" || a.FailureGroupID == "" || a.ProjectID == "" || a.ModelID == "" {
		return errors.New("ai analysis missing required field")
	}
	if a.GeneratedAt.IsZero() {
		a.GeneratedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO ai_analyses (
			analysis_id, failure_group_id, project_id, tenant_id,
			model_id, input_tokens, output_tokens, cost_usd,
			generated_at, analysis_markdown
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		a.AnalysisID, a.FailureGroupID, a.ProjectID, nullString(a.TenantID),
		a.ModelID, a.InputTokens, a.OutputTokens, a.CostUSD,
		a.GeneratedAt.UTC(), nullString(a.AnalysisMarkdown),
	)
	if err != nil {
		return fmt.Errorf("insert ai analysis: %w", err)
	}
	return nil
}

// ListAIAnalyses returns the most recent analyses across ALL
// projects + tenants, newest first. JOINs project + failure_group
// metadata so the founder view renders without follow-up queries.
// Caps at limit; pass 0 for the default (100).
func (s *SQLiteStore) ListAIAnalyses(
	ctx context.Context, limit, offset int,
) ([]*AIAnalysis, error) {
	if limit <= 0 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.analysis_id, a.failure_group_id, a.project_id, a.tenant_id,
		       a.model_id, a.input_tokens, a.output_tokens, a.cost_usd,
		       a.generated_at, a.analysis_markdown,
		       p.name, p.owner_email, p.tier,
		       fg.failure_class, fg.signature
		FROM ai_analyses a
		LEFT JOIN projects p ON p.project_id = a.project_id
		LEFT JOIN failure_groups fg ON fg.group_id = a.failure_group_id
		ORDER BY a.generated_at DESC
		LIMIT ? OFFSET ?
	`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list ai analyses: %w", err)
	}
	defer rows.Close()
	return scanAIAnalysesRows(rows)
}

// GetAIAnalysesTotals returns lifetime + this-month aggregate stats
// for the founder tile (#199). Two SELECTs because lifetime spans
// every row but the month window narrows.
func (s *SQLiteStore) GetAIAnalysesTotals(
	ctx context.Context,
) (*AIAnalysesTotals, error) {
	out := &AIAnalysesTotals{}
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(cost_usd), 0),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0)
		FROM ai_analyses
	`).Scan(
		&out.LifetimeCount, &out.LifetimeCostUSD,
		&out.LifetimeInputTokens, &out.LifetimeOutputTokens,
	)
	if err != nil {
		return nil, fmt.Errorf("lifetime ai analyses totals: %w", err)
	}
	monthStart := startOfCurrentMonthUTCStore()
	err = s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(cost_usd), 0)
		FROM ai_analyses
		WHERE generated_at >= ?
	`, monthStart).Scan(&out.MonthCount, &out.MonthCostUSD)
	if err != nil {
		return nil, fmt.Errorf("month ai analyses totals: %w", err)
	}
	return out, nil
}

// --- Postgres impl ------------------------------------------------

// CreateAIAnalysis is the Postgres twin.
func (s *PostgresStore) CreateAIAnalysis(
	ctx context.Context, a *AIAnalysis,
) error {
	if a == nil {
		return errors.New("nil analysis")
	}
	if a.AnalysisID == "" || a.FailureGroupID == "" || a.ProjectID == "" || a.ModelID == "" {
		return errors.New("ai analysis missing required field")
	}
	if a.GeneratedAt.IsZero() {
		a.GeneratedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO ai_analyses (
			analysis_id, failure_group_id, project_id, tenant_id,
			model_id, input_tokens, output_tokens, cost_usd,
			generated_at, analysis_markdown
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`,
		a.AnalysisID, a.FailureGroupID, a.ProjectID, nullString(a.TenantID),
		a.ModelID, a.InputTokens, a.OutputTokens, a.CostUSD,
		a.GeneratedAt.UTC(), nullString(a.AnalysisMarkdown),
	)
	if err != nil {
		return fmt.Errorf("insert ai analysis (postgres): %w", err)
	}
	return nil
}

// ListAIAnalyses is the Postgres twin.
func (s *PostgresStore) ListAIAnalyses(
	ctx context.Context, limit, offset int,
) ([]*AIAnalysis, error) {
	if limit <= 0 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.analysis_id, a.failure_group_id, a.project_id, a.tenant_id,
		       a.model_id, a.input_tokens, a.output_tokens, a.cost_usd,
		       a.generated_at, a.analysis_markdown,
		       p.name, p.owner_email, p.tier,
		       fg.failure_class, fg.signature
		FROM ai_analyses a
		LEFT JOIN projects p ON p.project_id = a.project_id
		LEFT JOIN failure_groups fg ON fg.group_id = a.failure_group_id
		ORDER BY a.generated_at DESC
		LIMIT $1 OFFSET $2
	`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list ai analyses (postgres): %w", err)
	}
	defer rows.Close()
	return scanAIAnalysesRows(rows)
}

// GetAIAnalysesTotals is the Postgres twin.
func (s *PostgresStore) GetAIAnalysesTotals(
	ctx context.Context,
) (*AIAnalysesTotals, error) {
	out := &AIAnalysesTotals{}
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(cost_usd), 0),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0)
		FROM ai_analyses
	`).Scan(
		&out.LifetimeCount, &out.LifetimeCostUSD,
		&out.LifetimeInputTokens, &out.LifetimeOutputTokens,
	)
	if err != nil {
		return nil, fmt.Errorf("lifetime ai analyses totals: %w", err)
	}
	monthStart := startOfCurrentMonthUTCStore()
	err = s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(cost_usd), 0)
		FROM ai_analyses
		WHERE generated_at >= $1
	`, monthStart).Scan(&out.MonthCount, &out.MonthCostUSD)
	if err != nil {
		return nil, fmt.Errorf("month ai analyses totals: %w", err)
	}
	return out, nil
}

// --- shared helpers -----------------------------------------------

// scanAIAnalysesRows reads one or more ai_analyses rows joined with
// project + failure_group metadata. Shared between the SQLite +
// Postgres readers so the scan order stays in lockstep.
func scanAIAnalysesRows(rows *sql.Rows) ([]*AIAnalysis, error) {
	out := make([]*AIAnalysis, 0, 32)
	for rows.Next() {
		a := &AIAnalysis{}
		var tenantID, markdown, name, email, tier, fc, sig sql.NullString
		if err := rows.Scan(
			&a.AnalysisID, &a.FailureGroupID, &a.ProjectID, &tenantID,
			&a.ModelID, &a.InputTokens, &a.OutputTokens, &a.CostUSD,
			&a.GeneratedAt, &markdown,
			&name, &email, &tier,
			&fc, &sig,
		); err != nil {
			return nil, fmt.Errorf("scan ai analysis: %w", err)
		}
		if tenantID.Valid {
			a.TenantID = tenantID.String
		}
		if markdown.Valid {
			a.AnalysisMarkdown = markdown.String
		}
		if name.Valid {
			a.ProjectName = name.String
		}
		if email.Valid {
			a.OwnerEmail = email.String
		}
		if tier.Valid {
			a.Tier = tier.String
		}
		if fc.Valid {
			a.FailureClass = fc.String
		}
		if sig.Valid {
			a.Signature = sig.String
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// startOfCurrentMonthUTCStore returns the first instant of the
// current UTC calendar month. Duplicated here (rather than reusing
// the api package helper) because the store layer should not
// depend on the api package — circular import risk.
func startOfCurrentMonthUTCStore() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}
