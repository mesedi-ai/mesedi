// Project webhook configuration and delivery records.
// Split out of sqlite.go on 2026-09-12; every declaration moved verbatim.
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

// CreateProjectWebhook inserts a new webhook configuration row. The
// caller is expected to set WebhookID + Secret; CreatedAt is set if
// zero. EnabledClasses is JSON-encoded into the TEXT column, nil/empty
// slice persists as NULL (interpreted as "all classes" by the
// dispatcher).
func (s *SQLiteStore) CreateProjectWebhook(ctx context.Context, wh *ProjectWebhook) error {
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

	enabled := 0
	if wh.Enabled {
		enabled = 1
	}

	recurrenceMode := wh.RecurrenceMode
	if recurrenceMode == "" {
		recurrenceMode = RecurrenceModeOff
	}
	var windowSeconds sql.NullInt64
	if recurrenceMode == RecurrenceModeThrottled && wh.RecurrenceWindowSeconds > 0 {
		windowSeconds = sql.NullInt64{Int64: int64(wh.RecurrenceWindowSeconds), Valid: true}
	}

	// auth_token is nullable in the schema; NULL for non-PagerDuty
	// webhooks so the reverse migration is a clean drop.
	var authTokenNS sql.NullString
	if wh.AuthToken != "" {
		authTokenNS = sql.NullString{String: wh.AuthToken, Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO project_webhooks (
			webhook_id, project_id, name, url, secret, auth_token,
			enabled_classes, enabled, created_at, severity_filter,
			recurrence_mode, recurrence_window_seconds
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		wh.WebhookID, wh.ProjectID, wh.Name, wh.URL, wh.Secret, authTokenNS,
		classesJSON, enabled, wh.CreatedAt.UTC().Format(time.RFC3339),
		wh.SeverityFilter,
		recurrenceMode, windowSeconds,
	)
	if err != nil {
		return fmt.Errorf("insert project_webhook: %w", err)
	}
	return nil
}

// ListProjectWebhooksForProject returns every webhook for a project,
// sorted newest first. The Secret field is intentionally NOT populated
// it's only ever surfaced once at creation time, never on list.
func (s *SQLiteStore) ListProjectWebhooksForProject(
	ctx context.Context,
	projectID string,
) ([]*ProjectWebhook, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT webhook_id, project_id, name, url,
		       enabled_classes, enabled, created_at, severity_filter,
		       recurrence_mode, recurrence_window_seconds
		FROM project_webhooks
		WHERE project_id = ?
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
		var createdAt string
		var enabled int
		var windowSeconds sql.NullInt64
		if err := rows.Scan(
			&wh.WebhookID, &wh.ProjectID, &wh.Name, &wh.URL,
			&classesJSON, &enabled, &createdAt, &wh.SeverityFilter,
			&wh.RecurrenceMode, &windowSeconds,
		); err != nil {
			return nil, fmt.Errorf("scan project_webhook: %w", err)
		}
		wh.Enabled = enabled != 0
		wh.EnabledClasses = parseEnabledClasses(classesJSON)
		if windowSeconds.Valid {
			wh.RecurrenceWindowSeconds = int(windowSeconds.Int64)
		}
		if t, perr := time.Parse(time.RFC3339, createdAt); perr == nil {
			wh.CreatedAt = t
		}
		out = append(out, &wh)
	}
	return out, rows.Err()
}

// ListEnabledProjectWebhooks returns only the enabled webhooks for a
// project, WITH the Secret populated. Used by the dispatcher to sign
// payloads. Never call this from a handler that returns the result to
// a client, the secret is sensitive.
func (s *SQLiteStore) ListEnabledProjectWebhooks(
	ctx context.Context,
	projectID string,
) ([]*ProjectWebhook, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT webhook_id, project_id, name, url, secret, auth_token,
		       enabled_classes, enabled, created_at, severity_filter,
		       recurrence_mode, recurrence_window_seconds
		FROM project_webhooks
		WHERE project_id = ? AND enabled = 1
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
		var createdAt string
		var enabled int
		var windowSeconds sql.NullInt64
		if err := rows.Scan(
			&wh.WebhookID, &wh.ProjectID, &wh.Name, &wh.URL, &wh.Secret, &authTokenNS,
			&classesJSON, &enabled, &createdAt, &wh.SeverityFilter,
			&wh.RecurrenceMode, &windowSeconds,
		); err != nil {
			return nil, fmt.Errorf("scan project_webhook: %w", err)
		}
		if authTokenNS.Valid {
			wh.AuthToken = authTokenNS.String
		}
		wh.Enabled = enabled != 0
		wh.EnabledClasses = parseEnabledClasses(classesJSON)
		if windowSeconds.Valid {
			wh.RecurrenceWindowSeconds = int(windowSeconds.Int64)
		}
		if t, perr := time.Parse(time.RFC3339, createdAt); perr == nil {
			wh.CreatedAt = t
		}
		out = append(out, &wh)
	}
	return out, rows.Err()
}

// DeleteProjectWebhook hard-deletes a webhook by id, scoped to project.
// Returns ErrNotFound if the webhook is absent OR belongs to another
// project, don't leak cross-tenant existence via id-guessing.
func (s *SQLiteStore) DeleteProjectWebhook(
	ctx context.Context,
	webhookID, projectID string,
) error {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM project_webhooks
		WHERE webhook_id = ? AND project_id = ?
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

// GetProjectWebhook returns one webhook by id with the Secret
// populated. Project-scoped, passing a webhook_id that belongs to a
// different project returns ErrNotFound (not 403) so we don't leak
// cross-tenant existence.
func (s *SQLiteStore) GetProjectWebhook(
	ctx context.Context,
	webhookID, projectID string,
) (*ProjectWebhook, error) {
	var wh ProjectWebhook
	var classesJSON sql.NullString
	var authTokenNS sql.NullString
	var createdAt string
	var enabled int
	var windowSeconds sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT webhook_id, project_id, name, url, secret, auth_token,
		       enabled_classes, enabled, created_at, severity_filter,
		       recurrence_mode, recurrence_window_seconds
		FROM project_webhooks
		WHERE webhook_id = ? AND project_id = ?
	`, webhookID, projectID).Scan(
		&wh.WebhookID, &wh.ProjectID, &wh.Name, &wh.URL, &wh.Secret, &authTokenNS,
		&classesJSON, &enabled, &createdAt, &wh.SeverityFilter,
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
	wh.Enabled = enabled != 0
	wh.EnabledClasses = parseEnabledClasses(classesJSON)
	if windowSeconds.Valid {
		wh.RecurrenceWindowSeconds = int(windowSeconds.Int64)
	}
	if t, perr := time.Parse(time.RFC3339, createdAt); perr == nil {
		wh.CreatedAt = t
	}
	return &wh, nil
}

// GetWebhookRecurrenceLastFired returns when this webhook last fired
// for this failure group. Returns ErrNotFound if there is no row yet
// (the dispatcher treats that as "the window has elapsed" so the
// first recurrence ping always goes out).
func (s *SQLiteStore) GetWebhookRecurrenceLastFired(
	ctx context.Context,
	webhookID, groupID string,
) (time.Time, error) {
	var ts string
	err := s.db.QueryRowContext(ctx, `
		SELECT last_fired_at FROM webhook_recurrence_state
		WHERE webhook_id = ? AND group_id = ?
	`, webhookID, groupID).Scan(&ts)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrNotFound
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("get webhook_recurrence_state: %w", err)
	}
	t, perr := time.Parse(time.RFC3339, ts)
	if perr != nil {
		return time.Time{}, fmt.Errorf("parse last_fired_at: %w", perr)
	}
	return t, nil
}

// UpsertWebhookRecurrenceLastFired records or updates the timestamp
// of the most recent fire for (webhook, group). Called from the
// dispatcher on every successful fire so the next throttle check
// observes the right baseline.
func (s *SQLiteStore) UpsertWebhookRecurrenceLastFired(
	ctx context.Context,
	webhookID, groupID string,
	t time.Time,
) error {
	if webhookID == "" || groupID == "" {
		return fmt.Errorf("webhook_id and group_id required")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO webhook_recurrence_state (webhook_id, group_id, last_fired_at)
		VALUES (?, ?, ?)
		ON CONFLICT(webhook_id, group_id) DO UPDATE SET
			last_fired_at = excluded.last_fired_at
	`, webhookID, groupID, t.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("upsert webhook_recurrence_state: %w", err)
	}
	return nil
}

// RecordWebhookDelivery persists one delivery-attempt row. DeliveryID
// and CreatedAt are set if zero. ResponseBody is truncated to ~2KB to
// bound storage growth from chatty receivers.
func (s *SQLiteStore) RecordWebhookDelivery(
	ctx context.Context,
	d *WebhookDelivery,
) error {
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
		// Deterministic-ish: hash (webhook_id + created_at_nano + attempt).
		// Doesn't need to be cryptographically unique, collision space is
		// minuscule and a collision would just upsert one row.
		raw := d.WebhookID + d.CreatedAt.Format(time.RFC3339Nano) +
			fmt.Sprintf("/%d", d.Attempt)
		sum := sha256.Sum256([]byte(raw))
		d.DeliveryID = "del-" + hex.EncodeToString(sum[:8])
	}

	// Truncate response body to bound storage.
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
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		d.DeliveryID, d.WebhookID, d.ProjectID,
		nullableString(d.FailureClass), nullableString(d.Signature), nullableString(d.GroupID),
		d.Attempt, d.Status, httpStatus, nullableString(d.Error), nullableString(body),
		d.DurationMs, d.CreatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("insert webhook_delivery: %w", err)
	}
	return nil
}

// ListDeliveriesForWebhook returns the most recent N delivery attempts
// for a webhook, newest first. limit <= 0 defaults to 50.
func (s *SQLiteStore) ListDeliveriesForWebhook(
	ctx context.Context,
	webhookID string,
	limit int,
) ([]*WebhookDelivery, error) {
	// Mirror Postgres clamp (alert ). Negative / zero falls
	// back to a sane default; anything above the package ceiling is
	// clamped so a future-bug caller can not drive an unbounded
	// allocation -- and CodeQL sees a constant upper bound at the
	// make() site below.
	if limit <= 0 {
		limit = 50
	}
	if limit > WebhookDeliveryListLimitMax {
		limit = WebhookDeliveryListLimitMax
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT delivery_id, webhook_id, project_id,
		       failure_class, signature, group_id,
		       attempt, status, http_status, error, response_body,
		       duration_ms, created_at
		FROM webhook_deliveries
		WHERE webhook_id = ?
		ORDER BY created_at DESC
		LIMIT ?
	`, webhookID, limit)
	if err != nil {
		return nil, fmt.Errorf("list webhook_deliveries: %w", err)
	}
	defer rows.Close()

	// Capacity hint uses the package-level constant; see Postgres
	// counterpart for the static-analysis rationale.
	out := make([]*WebhookDelivery, 0, WebhookDeliveryListLimitMax)
	for rows.Next() {
		var d WebhookDelivery
		var failureClass, signature, groupID, errMsg, respBody sql.NullString
		var httpStatus sql.NullInt64
		var createdAt string
		if err := rows.Scan(
			&d.DeliveryID, &d.WebhookID, &d.ProjectID,
			&failureClass, &signature, &groupID,
			&d.Attempt, &d.Status, &httpStatus, &errMsg, &respBody,
			&d.DurationMs, &createdAt,
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
		if t, perr := time.Parse(time.RFC3339Nano, createdAt); perr == nil {
			d.CreatedAt = t
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}

// parseEnabledClasses converts the JSON TEXT column into a Go slice.
// Returns nil if the column is NULL or malformed, both are
// interpreted as "all classes" by the dispatcher, so the conservative
// fallback is safe.
func parseEnabledClasses(s sql.NullString) []string {
	if !s.Valid || s.String == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s.String), &out); err != nil {
		return nil
	}
	return out
}
