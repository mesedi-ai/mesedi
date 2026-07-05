package store

// SQLite implementations of the HobbyBillingScheduler-specific store
// methods declared in store.go. Lives in its own file so the new
// scheduler-related queries don't bloat the main sqlite.go.
//
// The migration that ships these columns is 021; readers prior to
// that migration will see NULL/0 defaults which are safe values for
// every code path that consumes them.

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ListProjectsForHobbyBillingTick returns every Hobby-tier project
// the scheduler needs to consider this tick. See the interface
// docstring in store.go for the contract.
func (s *SQLiteStore) ListProjectsForHobbyBillingTick(
	ctx context.Context, now time.Time,
) ([]*Project, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT project_id, name, owner_user_id, owner_email, created_at,
		       tier, stripe_customer_id, stripe_subscription_id,
		       current_period_start, current_period_end, executions_this_period,
		       granted_executions, granted_executions_expires_at, tier_expires_at,
		       billing_cap_usd,
		       hobby_billing_last_attempt_at, hobby_billing_consecutive_failures
		FROM projects
		WHERE tier = 'hobby'
		  AND (
		        current_period_start IS NULL
		     OR current_period_end IS NULL
		     OR current_period_end <= ?
		      )
		ORDER BY created_at ASC
	`, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("list projects for hobby billing tick: %w", err)
	}
	defer rows.Close()

	out := make([]*Project, 0, 4)
	for rows.Next() {
		p := &Project{}
		var owner, email, stripeCust, stripeSub sql.NullString
		var periodStart, periodEnd sql.NullInt64
		var grantExpires, tierExpires sql.NullInt64
		var lastAttempt sql.NullInt64
		if err := rows.Scan(
			&p.ProjectID, &p.Name, &owner, &email, &p.CreatedAt,
			&p.Tier, &stripeCust, &stripeSub,
			&periodStart, &periodEnd, &p.ExecutionsThisPeriod,
			&p.GrantedExecutions, &grantExpires, &tierExpires,
			&p.BillingCapUSD,
			&lastAttempt, &p.HobbyBillingConsecutiveFailures,
		); err != nil {
			return nil, fmt.Errorf("scan hobby billing project: %w", err)
		}
		if owner.Valid {
			p.OwnerUserID = owner.String
		}
		if email.Valid {
			p.OwnerEmail = email.String
		}
		if stripeCust.Valid {
			p.StripeCustomerID = stripeCust.String
		}
		if stripeSub.Valid {
			p.StripeSubscriptionID = stripeSub.String
		}
		if periodStart.Valid {
			t := time.Unix(periodStart.Int64, 0).UTC()
			p.CurrentPeriodStart = &t
		}
		if periodEnd.Valid {
			t := time.Unix(periodEnd.Int64, 0).UTC()
			p.CurrentPeriodEnd = &t
		}
		if grantExpires.Valid {
			t := time.Unix(grantExpires.Int64, 0).UTC()
			p.GrantedExecutionsExpiresAt = &t
		}
		if tierExpires.Valid {
			t := time.Unix(tierExpires.Int64, 0).UTC()
			p.TierExpiresAt = &t
		}
		if lastAttempt.Valid {
			t := time.Unix(lastAttempt.Int64, 0).UTC()
			p.HobbyBillingLastAttemptAt = &t
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows.Err: %w", err)
	}
	return out, nil
}

// UpdateHobbyBillingState records the outcome of a charge attempt.
// On success the failure counter resets to zero; on failure it
// increments by one. The last_attempt_at column always advances to
// the new attemptAt so the every-other-day retry cadence works.
func (s *SQLiteStore) UpdateHobbyBillingState(
	ctx context.Context, projectID string, attemptAt time.Time, success bool,
) error {
	var query string
	if success {
		query = `
			UPDATE projects
			SET hobby_billing_last_attempt_at = ?,
			    hobby_billing_consecutive_failures = 0
			WHERE project_id = ?
		`
	} else {
		query = `
			UPDATE projects
			SET hobby_billing_last_attempt_at = ?,
			    hobby_billing_consecutive_failures = hobby_billing_consecutive_failures + 1
			WHERE project_id = ?
		`
	}
	res, err := s.db.ExecContext(ctx, query, attemptAt.Unix(), projectID)
	if err != nil {
		return fmt.Errorf("update hobby billing state: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// DetachHobbyCardForBillingFailure clears the saved card and resets
// the billing state in one transaction. Called when consecutive
// failures cross the configured ceiling; the project reverts to
// hard-capped "no card on file" semantics until the customer
// attaches a new card via Stripe Checkout.
func (s *SQLiteStore) DetachHobbyCardForBillingFailure(
	ctx context.Context, projectID string,
) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE projects
		SET stripe_customer_id = NULL,
		    card_on_file = 0,
		    hobby_billing_consecutive_failures = 0,
		    hobby_billing_last_attempt_at = NULL
		WHERE project_id = ?
	`, projectID)
	if err != nil {
		return fmt.Errorf("detach hobby card: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkCardDetached is the customer-initiated detach store method
//. See store.go for the contract.
func (s *SQLiteStore) MarkCardDetached(
	ctx context.Context, projectID string,
) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE projects SET card_on_file = 0 WHERE project_id = ?`,
		projectID,
	)
	if err != nil {
		return fmt.Errorf("mark card detached: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkCardAttached is the inverse: set card_on_file = TRUE. Called
// from handleSetupIntentSucceeded.
func (s *SQLiteStore) MarkCardAttached(
	ctx context.Context, projectID string,
) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE projects SET card_on_file = 1 WHERE project_id = ?`,
		projectID,
	)
	if err != nil {
		return fmt.Errorf("mark card attached: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}
