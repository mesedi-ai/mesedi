package store

// Team / multi-seat store methods, Postgres implementation (#263).
// Postgres counterpart to sqlite_organizations.go. $N placeholders +
// EXCLUDED-keyword ON CONFLICT syntax.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// =====================================================================
// Organizations
// =====================================================================

func (s *PostgresStore) CreateOrganization(ctx context.Context, org *Organization) error {
	if org == nil || org.OrgID == "" || org.Name == "" {
		return fmt.Errorf("org_id and name required")
	}
	now := time.Now().UTC().Unix()
	org.CreatedAt = time.Unix(now, 0).UTC()
	org.UpdatedAt = org.CreatedAt
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO organizations (org_id, name, created_by_user_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5)
	`, org.OrgID, org.Name, org.CreatedByUserID, now, now)
	if err != nil {
		return fmt.Errorf("insert organization: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetOrganization(ctx context.Context, orgID string) (*Organization, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT org_id, name, created_by_user_id, created_at, updated_at
		FROM organizations WHERE org_id = $1
	`, orgID)
	return scanOrganizationPg(row)
}

func (s *PostgresStore) UpdateOrganizationName(ctx context.Context, orgID, name string) error {
	if name == "" {
		return fmt.Errorf("name required")
	}
	now := time.Now().UTC().Unix()
	res, err := s.db.ExecContext(ctx, `
		UPDATE organizations SET name = $1, updated_at = $2 WHERE org_id = $3
	`, name, now, orgID)
	if err != nil {
		return fmt.Errorf("update organization: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) ListOrganizationsForUser(ctx context.Context, userID string) ([]*Organization, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT o.org_id, o.name, o.created_by_user_id, o.created_at, o.updated_at
		FROM organizations o
		JOIN organization_members m ON m.org_id = o.org_id
		WHERE m.user_id = $1
		ORDER BY o.name ASC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("list organizations for user: %w", err)
	}
	defer rows.Close()
	out := make([]*Organization, 0, 4)
	for rows.Next() {
		o := &Organization{}
		var createdAt, updatedAt int64
		if err := rows.Scan(&o.OrgID, &o.Name, &o.CreatedByUserID, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan organization: %w", err)
		}
		o.CreatedAt = time.Unix(createdAt, 0).UTC()
		o.UpdatedAt = time.Unix(updatedAt, 0).UTC()
		out = append(out, o)
	}
	return out, rows.Err()
}

func scanOrganizationPg(row *sql.Row) (*Organization, error) {
	o := &Organization{}
	var createdAt, updatedAt int64
	err := row.Scan(&o.OrgID, &o.Name, &o.CreatedByUserID, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan organization: %w", err)
	}
	o.CreatedAt = time.Unix(createdAt, 0).UTC()
	o.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return o, nil
}

// =====================================================================
// Organization members
// =====================================================================

func (s *PostgresStore) AddOrganizationMember(ctx context.Context, m *OrganizationMember) error {
	if m == nil || m.OrgID == "" || m.UserID == "" || m.Role == "" {
		return fmt.Errorf("org_id, user_id, role required")
	}
	now := time.Now().UTC().Unix()
	m.AddedAt = time.Unix(now, 0).UTC()
	var email, addedBy sql.NullString
	if m.Email != "" {
		email = sql.NullString{String: m.Email, Valid: true}
	}
	if m.AddedByUserID != "" {
		addedBy = sql.NullString{String: m.AddedByUserID, Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO organization_members (
			org_id, user_id, role, email, added_by_user_id, added_at
		) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (org_id, user_id) DO UPDATE SET
			role             = EXCLUDED.role,
			email            = COALESCE(EXCLUDED.email, organization_members.email),
			added_by_user_id = COALESCE(EXCLUDED.added_by_user_id, organization_members.added_by_user_id)
	`, m.OrgID, m.UserID, m.Role, email, addedBy, now)
	if err != nil {
		return fmt.Errorf("insert organization_member: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetOrganizationMember(ctx context.Context, orgID, userID string) (*OrganizationMember, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT org_id, user_id, role, email, added_by_user_id, added_at
		FROM organization_members
		WHERE org_id = $1 AND user_id = $2
	`, orgID, userID)
	return scanOrganizationMemberPg(row)
}

func (s *PostgresStore) ListOrganizationMembers(ctx context.Context, orgID string) ([]*OrganizationMember, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT org_id, user_id, role, email, added_by_user_id, added_at
		FROM organization_members
		WHERE org_id = $1
		ORDER BY added_at ASC
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list organization_members: %w", err)
	}
	defer rows.Close()
	out := make([]*OrganizationMember, 0, 8)
	for rows.Next() {
		m, scanErr := scanOrganizationMemberRowsPg(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *PostgresStore) UpdateOrganizationMemberRole(ctx context.Context, orgID, userID, newRole string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE organization_members SET role = $1
		WHERE org_id = $2 AND user_id = $3
	`, newRole, orgID, userID)
	if err != nil {
		return fmt.Errorf("update organization_member role: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) RemoveOrganizationMember(ctx context.Context, orgID, userID string) error {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM organization_members
		WHERE org_id = $1 AND user_id = $2
	`, orgID, userID)
	if err != nil {
		return fmt.Errorf("delete organization_member: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanOrganizationMemberPg(row *sql.Row) (*OrganizationMember, error) {
	m := &OrganizationMember{}
	var email, addedBy sql.NullString
	var addedAt int64
	err := row.Scan(&m.OrgID, &m.UserID, &m.Role, &email, &addedBy, &addedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan organization_member: %w", err)
	}
	if email.Valid {
		m.Email = email.String
	}
	if addedBy.Valid {
		m.AddedByUserID = addedBy.String
	}
	m.AddedAt = time.Unix(addedAt, 0).UTC()
	return m, nil
}

func scanOrganizationMemberRowsPg(rows *sql.Rows) (*OrganizationMember, error) {
	m := &OrganizationMember{}
	var email, addedBy sql.NullString
	var addedAt int64
	if err := rows.Scan(&m.OrgID, &m.UserID, &m.Role, &email, &addedBy, &addedAt); err != nil {
		return nil, fmt.Errorf("scan organization_member row: %w", err)
	}
	if email.Valid {
		m.Email = email.String
	}
	if addedBy.Valid {
		m.AddedByUserID = addedBy.String
	}
	m.AddedAt = time.Unix(addedAt, 0).UTC()
	return m, nil
}

// =====================================================================
// Organization invites
// =====================================================================

func (s *PostgresStore) CreateOrganizationInvite(ctx context.Context, inv *OrganizationInvite) error {
	if inv == nil || inv.InviteID == "" || inv.OrgID == "" || inv.Email == "" || inv.Token == "" || inv.Role == "" {
		return fmt.Errorf("invite_id, org_id, email, role, token required")
	}
	now := time.Now().UTC().Unix()
	inv.CreatedAt = time.Unix(now, 0).UTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO organization_invites (
			invite_id, org_id, email, role, token,
			invited_by_user_id, created_at, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, inv.InviteID, inv.OrgID, inv.Email, inv.Role, inv.Token,
		inv.InvitedByUserID, now, inv.ExpiresAt.UTC().Unix())
	if err != nil {
		return fmt.Errorf("insert organization_invite: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetOrganizationInviteByToken(ctx context.Context, token string) (*OrganizationInvite, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT invite_id, org_id, email, role, token,
		       invited_by_user_id, created_at, expires_at,
		       accepted_at, accepted_by_user_id
		FROM organization_invites WHERE token = $1
	`, token)
	return scanOrganizationInvitePg(row)
}

func (s *PostgresStore) ListOrganizationInvites(ctx context.Context, orgID string, pendingOnly bool) ([]*OrganizationInvite, error) {
	query := `
		SELECT invite_id, org_id, email, role, '' as token,
		       invited_by_user_id, created_at, expires_at,
		       accepted_at, accepted_by_user_id
		FROM organization_invites
		WHERE org_id = $1
	`
	args := []any{orgID}
	if pendingOnly {
		query += " AND accepted_at IS NULL AND expires_at > $2"
		args = append(args, time.Now().UTC().Unix())
	}
	query += " ORDER BY created_at DESC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list organization_invites: %w", err)
	}
	defer rows.Close()
	out := make([]*OrganizationInvite, 0, 4)
	for rows.Next() {
		inv, scanErr := scanOrganizationInviteRowsPg(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

func (s *PostgresStore) MarkInviteAccepted(ctx context.Context, inviteID, acceptedByUserID string) error {
	now := time.Now().UTC().Unix()
	res, err := s.db.ExecContext(ctx, `
		UPDATE organization_invites
		SET accepted_at = $1, accepted_by_user_id = $2
		WHERE invite_id = $3 AND accepted_at IS NULL
	`, now, acceptedByUserID, inviteID)
	if err != nil {
		return fmt.Errorf("mark invite accepted: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		var exists int
		if qerr := s.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM organization_invites WHERE invite_id = $1
		`, inviteID).Scan(&exists); qerr == nil && exists > 0 {
			return ErrAlreadyAccepted
		}
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) RevokeOrganizationInvite(ctx context.Context, inviteID, orgID string) error {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM organization_invites
		WHERE invite_id = $1 AND org_id = $2 AND accepted_at IS NULL
	`, inviteID, orgID)
	if err != nil {
		return fmt.Errorf("revoke organization_invite: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanOrganizationInvitePg(row *sql.Row) (*OrganizationInvite, error) {
	inv := &OrganizationInvite{}
	var createdAt, expiresAt int64
	var acceptedAt sql.NullInt64
	var acceptedBy sql.NullString
	err := row.Scan(
		&inv.InviteID, &inv.OrgID, &inv.Email, &inv.Role, &inv.Token,
		&inv.InvitedByUserID, &createdAt, &expiresAt,
		&acceptedAt, &acceptedBy,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan organization_invite: %w", err)
	}
	inv.CreatedAt = time.Unix(createdAt, 0).UTC()
	inv.ExpiresAt = time.Unix(expiresAt, 0).UTC()
	if acceptedAt.Valid {
		t := time.Unix(acceptedAt.Int64, 0).UTC()
		inv.AcceptedAt = &t
	}
	if acceptedBy.Valid {
		inv.AcceptedByUserID = acceptedBy.String
	}
	return inv, nil
}

func scanOrganizationInviteRowsPg(rows *sql.Rows) (*OrganizationInvite, error) {
	inv := &OrganizationInvite{}
	var createdAt, expiresAt int64
	var acceptedAt sql.NullInt64
	var acceptedBy sql.NullString
	if err := rows.Scan(
		&inv.InviteID, &inv.OrgID, &inv.Email, &inv.Role, &inv.Token,
		&inv.InvitedByUserID, &createdAt, &expiresAt,
		&acceptedAt, &acceptedBy,
	); err != nil {
		return nil, fmt.Errorf("scan organization_invite row: %w", err)
	}
	inv.CreatedAt = time.Unix(createdAt, 0).UTC()
	inv.ExpiresAt = time.Unix(expiresAt, 0).UTC()
	if acceptedAt.Valid {
		t := time.Unix(acceptedAt.Int64, 0).UTC()
		inv.AcceptedAt = &t
	}
	if acceptedBy.Valid {
		inv.AcceptedByUserID = acceptedBy.String
	}
	return inv, nil
}

// =====================================================================
// Projects ↔ tenant lookups
// =====================================================================

func (s *PostgresStore) SetProjectTenantID(ctx context.Context, projectID, tenantID string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE projects SET tenant_id = $1 WHERE project_id = $2
	`, tenantID, projectID)
	if err != nil {
		return fmt.Errorf("set project tenant_id: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set project tenant_id rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) GetProjectTenantID(ctx context.Context, projectID string) (*string, error) {
	var tenantID sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT tenant_id FROM projects WHERE project_id = $1
	`, projectID).Scan(&tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get project tenant_id: %w", err)
	}
	if !tenantID.Valid {
		return nil, nil
	}
	t := tenantID.String
	return &t, nil
}

func (s *PostgresStore) ListProjectsByTenant(ctx context.Context, tenantID string) ([]*Project, error) {
	if tenantID == "" {
		return []*Project{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT project_id, name, owner_user_id, owner_email, created_at,
		       tier, stripe_customer_id, stripe_subscription_id,
		       current_period_start, current_period_end, executions_this_period,
		       granted_executions, granted_executions_expires_at, tier_expires_at
		FROM projects
		WHERE tenant_id = $1
		ORDER BY created_at ASC
	`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list projects by tenant: %w", err)
	}
	defer rows.Close()

	out := make([]*Project, 0, 4)
	for rows.Next() {
		p := &Project{}
		var owner, email, stripeCust, stripeSub sql.NullString
		var periodStart, periodEnd sql.NullInt64
		var grantExpires, tierExpires sql.NullInt64
		if err := rows.Scan(
			&p.ProjectID, &p.Name, &owner, &email, &p.CreatedAt,
			&p.Tier, &stripeCust, &stripeSub,
			&periodStart, &periodEnd, &p.ExecutionsThisPeriod,
			&p.GrantedExecutions, &grantExpires, &tierExpires,
		); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
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
		out = append(out, p)
	}
	return out, rows.Err()
}
