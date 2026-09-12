// Project webhook configuration and delivery records.
// Split out of postgres.go on 2026-09-12; every declaration moved verbatim.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func (s *PostgresStore) CreateProjectWebhook(ctx context.Context, wh *ProjectWebhook) error {
	if wh.WebhookID == "" {
		return fmt.Errorf("webhook_id required")
	}
	if wh.ProjectID == "" {
		return fmt.Errorf("project_id required")
	}
	if wh.URL == "" {
		return fmt.Errorf("url required")
	}
	if wh.Secret == "" {
		return fmt.Errorf("secret required")
	}
	if wh.CreatedAt.IsZero() {
		wh.CreatedAt = time.Now().UTC()
	}

	var classesJSON sql.NullString
	if len(wh.EnabledClasses) > 0 {
		b, err := json.Marshal(wh.EnabledClasses)
		if err != nil {
			return fmt.Errorf("marshal enabled_classes: %w", err)
		}
		classesJSON = sql.NullString{String: string(b), Valid: true}
	}

	recurrenceMode := wh.RecurrenceMode
	if recurrenceMode == "" {
		recurrenceMode = RecurrenceModeOff
	}
	var windowSeconds sql.NullInt64
	if recurrenceMode == RecurrenceModeThrottled && wh.RecurrenceWindowSeconds > 0 {
		windowSeconds = sql.NullInt64{Int64: int64(wh.RecurrenceWindowSeconds), Valid: true}
	}

	var authTokenNS sql.NullString
	if wh.AuthToken != "" {
		authTokenNS = sql.NullString{String: wh.AuthToken, Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO project_webhooks (
			webhook_id, project_id, name, url, secret, auth_token,
			enabled_classes, enabled, created_at, severity_filter,
			recurrence_mode, recurrence_window_seconds
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`,
		wh.WebhookID, wh.ProjectID, wh.Name, wh.URL, wh.Secret, authTokenNS,
		classesJSON, wh.Enabled, wh.CreatedAt.UTC(),
		wh.SeverityFilter,
		recurrenceMode, windowSeconds,
	)
	if err != nil {
		return fmt.Errorf("insert project_webhook: %w", err)
	}
	return nil
}

func (s *PostgresStore) ListProjectWebhooksForProject(ctx context.Context, projectID string) ([]*ProjectWebhook, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT webhook_id, project_id, name, url,
		       enabled_classes, enabled, created_at, severity_filter,
		       recurrence_mode, recurrence_window_seconds
		FROM project_webhooks
		WHERE project_id = $1
		ORDER BY created_at DESC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list project_webhooks: %w", err)
	}
	defer rows.Close()

	out := make([]*ProjectWebhook, 0, 8)
	for rows.Next() {
		var wh ProjectWebhook
		var classesJSON sql.NullString
		var windowSeconds sql.NullInt64
		if err := rows.Scan(
			&wh.WebhookID, &wh.ProjectID, &wh.Name, &wh.URL,
			&classesJSON, &wh.Enabled, &wh.CreatedAt, &wh.SeverityFilter,
			&wh.RecurrenceMode, &windowSeconds,
		); err != nil {
			return nil, fmt.Errorf("scan project_webhook: %w", err)
		}
		wh.EnabledClasses = parseEnabledClasses(classesJSON)
		if windowSeconds.Valid {
			wh.RecurrenceWindowSeconds = int(windowSeconds.Int64)
		}
		out = append(out, &wh)
	}
	return out, rows.Err()
}

func (s *PostgresStore) ListEnabledProjectWebhooks(ctx context.Context, projectID string) ([]*ProjectWebhook, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT webhook_id, project_id, name, url, secret, auth_token,
		       enabled_classes, enabled, created_at, severity_filter,
		       recurrence_mode, recurrence_window_seconds
		FROM project_webhooks
		WHERE project_id = $1 AND enabled = TRUE
		ORDER BY created_at ASC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list enabled project_webhooks: %w", err)
	}
	defer rows.Close()

	out := make([]*ProjectWebhook, 0, 8)
	for rows.Next() {
		var wh ProjectWebhook
		var classesJSON sql.NullString
		var authTokenNS sql.NullString
		var windowSeconds sql.NullInt64
		if err := rows.Scan(
			&wh.WebhookID, &wh.ProjectID, &wh.Name, &wh.URL, &wh.Secret, &authTokenNS,
			&classesJSON, &wh.Enabled, &wh.CreatedAt, &wh.SeverityFilter,
			&wh.RecurrenceMode, &windowSeconds,
		); err != nil {
			return nil, fmt.Errorf("scan project_webhook: %w", err)
		}
		if authTokenNS.Valid {
			wh.AuthToken = authTokenNS.String
		}
		wh.EnabledClasses = parseEnabledClasses(classesJSON)
		if windowSeconds.Valid {
			wh.RecurrenceWindowSeconds = int(windowSeconds.Int64)
		}
		out = append(out, &wh)
	}
	return out, rows.Err()
}

func (s *PostgresStore) DeleteProjectWebhook(ctx context.Context, webhookID, projectID string) error {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM project_webhooks
		WHERE webhook_id = $1 AND project_id = $2
	`, webhookID, projectID)
	if err != nil {
		return fmt.Errorf("delete project_webhook: %w", err)
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

func (s *PostgresStore) GetProjectWebhook(ctx context.Context, webhookID, projectID string) (*ProjectWebhook, error) {
	var wh ProjectWebhook
	var classesJSON sql.NullString
	var authTokenNS sql.NullString
	var windowSeconds sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT webhook_id, project_id, name, url, secret, auth_token,
		       enabled_classes, enabled, created_at, severity_filter,
		       recurrence_mode, recurrence_window_seconds
		FROM project_webhooks
		WHERE webhook_id = $1 AND project_id = $2
	`, webhookID, projectID).Scan(
		&wh.WebhookID, &wh.ProjectID, &wh.Name, &wh.URL, &wh.Secret, &authTokenNS,
		&classesJSON, &wh.Enabled, &wh.CreatedAt, &wh.SeverityFilter,
		&wh.RecurrenceMode, &windowSeconds,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get project_webhook: %w", err)
	}
	if authTokenNS.Valid {
		wh.AuthToken = authTokenNS.String
	}
	wh.EnabledClasses = parseEnabledClasses(classesJSON)
	if windowSeconds.Valid {
		wh.RecurrenceWindowSeconds = int(windowSeconds.Int64)
	}
	return &wh, nil
}

// GetWebhookRecurrenceLastFired returns when this webhook last fired
// for this failure group. ErrNotFound means "no row yet"; dispatcher
// treats that as "window elapsed."
func (s *PostgresStore) GetWebhookRecurrenceLastFired(
	ctx context.Context,
	webhookID, groupID string,
) (time.Time, error) {
	var t time.Time
	err := s.db.QueryRowContext(ctx, `
		SELECT last_fired_at FROM webhook_recurrence_state
		WHERE webhook_id = $1 AND group_id = $2
	`, webhookID, groupID).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrNotFound
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("get webhook_recurrence_state: %w", err)
	}
	return t, nil
}

// UpsertWebhookRecurrenceLastFired records or updates the last-fired
// timestamp for (webhook, group).
func (s *PostgresStore) UpsertWebhookRecurrenceLastFired(
	ctx context.Context,
	webhookID, groupID string,
	t time.Time,
) error {
	if webhookID == "" || groupID == "" {
		return fmt.Errorf("webhook_id and group_id required")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO webhook_recurrence_state (webhook_id, group_id, last_fired_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (webhook_id, group_id) DO UPDATE SET
			last_fired_at = EXCLUDED.last_fired_at
	`, webhookID, groupID, t.UTC())
	if err != nil {
		return fmt.Errorf("upsert webhook_recurrence_state: %w", err)
	}
	return nil
}

func (s *PostgresStore) RecordWebhookDelivery(ctx context.Context, d *WebhookDelivery) error {
	if d.WebhookID == "" {
		return fmt.Errorf("webhook_id required")
	}
	if d.ProjectID == "" {
		return fmt.Errorf("project_id required")
	}
	if d.Status == "" {
		return fmt.Errorf("status required")
	}
	if d.Attempt <= 0 {
		d.Attempt = 1
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now().UTC()
	}
	if d.DeliveryID == "" {
		raw := d.WebhookID + d.CreatedAt.Format(time.RFC3339Nano) +
			fmt.Sprintf("/%d", d.Attempt)
		sum := sha256.Sum256([]byte(raw))
		d.DeliveryID = "del-" + hex.EncodeToString(sum[:8])
	}

	const maxBodyBytes = 2048
	body := d.ResponseBody
	if len(body) > maxBodyBytes {
		body = body[:maxBodyBytes] + "…[truncated]"
	}

	var httpStatus sql.NullInt64
	if d.HTTPStatus != nil {
		httpStatus = sql.NullInt64{Int64: int64(*d.HTTPStatus), Valid: true}
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO webhook_deliveries (
			delivery_id, webhook_id, project_id,
			failure_class, signature, group_id,
			attempt, status, http_status, error, response_body,
			duration_ms, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`,
		d.DeliveryID, d.WebhookID, d.ProjectID,
		nullableString(d.FailureClass), nullableString(d.Signature), nullableString(d.GroupID),
		d.Attempt, d.Status, httpStatus, nullableString(d.Error), nullableString(body),
		d.DurationMs, d.CreatedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("insert webhook_delivery: %w", err)
	}
	return nil
}

func (s *PostgresStore) ListDeliveriesForWebhook(ctx context.Context, webhookID string, limit int) ([]*WebhookDelivery, error) {
	// Clamp limit to the package ceiling (alert ). Caller is
	// trusted to pass a sane value but a defensive cap here keeps a
	// future-bug caller from driving an unbounded allocation -- and
	// lets CodeQL see the upper bound at the make() site below.
	if limit <= 0 || limit > WebhookDeliveryListLimitMax {
		limit = WebhookDeliveryListLimitMax
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT delivery_id, webhook_id, project_id,
		       failure_class, signature, group_id,
		       attempt, status, http_status, error, response_body,
		       duration_ms, created_at
		FROM webhook_deliveries
		WHERE webhook_id = $1
		ORDER BY created_at DESC
		LIMIT $2
	`, webhookID, limit)
	if err != nil {
		return nil, fmt.Errorf("list webhook_deliveries: %w", err)
	}
	defer rows.Close()

	// Capacity hint uses the package-level constant so the upper
	// bound is visible to static analysis; `limit` is guaranteed
	// <= WebhookDeliveryListLimitMax by the clamp at function entry.
	out := make([]*WebhookDelivery, 0, WebhookDeliveryListLimitMax)
	for rows.Next() {
		var d WebhookDelivery
		var failureClass, signature, groupID, errMsg, respBody sql.NullString
		var httpStatus sql.NullInt64
		if err := rows.Scan(
			&d.DeliveryID, &d.WebhookID, &d.ProjectID,
			&failureClass, &signature, &groupID,
			&d.Attempt, &d.Status, &httpStatus, &errMsg, &respBody,
			&d.DurationMs, &d.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan webhook_delivery: %w", err)
		}
		if failureClass.Valid {
			d.FailureClass = failureClass.String
		}
		if signature.Valid {
			d.Signature = signature.String
		}
		if groupID.Valid {
			d.GroupID = groupID.String
		}
		if errMsg.Valid {
			d.Error = errMsg.String
		}
		if respBody.Valid {
			d.ResponseBody = respBody.String
		}
		if httpStatus.Valid {
			v := int(httpStatus.Int64)
			d.HTTPStatus = &v
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}
