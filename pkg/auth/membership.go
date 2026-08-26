package auth

// Epic 15.3 per-Project RBAC memberships (ADR-035, ISI-2921).
//
// This is the write/read seam over auth.project_membership (db/migrations/0010): the (user, Project,
// role) grants that turn a base global_role=user into an authorized caller on a specific Project. The
// three-tier role vocabulary (viewer < contributor < maintainer) is already fixed in groupmapping.go
// (ProjectRole* constants); this file adds the durable store and the rank comparison the enforcement
// middleware (internal/apiserver/rbac.go, 15.4) applies.
//
// admin needs NO membership: global_role=admin is fleet-wide authority and short-circuits the RBAC
// check before any lookup here. A caller with no membership on a Project resolves to ErrNoMembership
// — fail-closed (the middleware maps that to a 404 existence-hiding response, never a 200).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrNoMembership is the "this user holds no role on this Project" sentinel. It is NOT an error
// condition to log — it is the ordinary deny outcome the middleware turns into a 404 (existence-hiding).
var ErrNoMembership = errors.New("auth: no project membership")

// roleRank orders the ADR-035 three-tier vocabulary. An unknown/empty role ranks 0 (deny) — a role
// the store's CHECK constraint would never have admitted, but ranking it 0 keeps RoleAtLeast
// fail-closed even if a row somehow carried a stale value.
var roleRank = map[string]int{
	ProjectRoleViewer:      1,
	ProjectRoleContributor: 2,
	ProjectRoleMaintainer:  3,
}

// RoleAtLeast reports whether the held Project role satisfies the required minimum, using the
// ADR-035 ordering viewer < contributor < maintainer. An unknown/empty held role never satisfies
// any requirement (deny-by-default); an unknown/empty required role is treated as "any membership
// suffices" only when the held role is itself a known role (rank ≥ 1).
func RoleAtLeast(have, min string) bool {
	h := roleRank[have]
	if h == 0 {
		return false // unknown/empty held role: deny
	}
	return h >= roleRank[min]
}

// MembershipStore is the persistence seam for auth.project_membership (Postgres-backed in
// production; fakes in unit tests). RoleForPrincipal is the enforcement hot path — it resolves a
// caller's identity string straight to their role on a Project in one join, so the middleware never
// carries a user UUID.
type MembershipStore interface {
	// RoleForPrincipal returns the caller's role on the named Project, or ErrNoMembership if the
	// principal is unknown or holds no grant there.
	RoleForPrincipal(ctx context.Context, principal, project string) (string, error)
	// ListForUser returns every (Project, role) grant a user holds (8.15 review surface).
	ListForUser(ctx context.Context, userID uuid.UUID) ([]ProjectMembership, error)
	// Grant upserts a user's role on a Project (one role per user per Project — the strongest
	// intended role, since callers collapse duplicates before writing). createdBy is the acting
	// admin's principal ("" ⇒ NULL, the 15.9 IdP-sync row).
	Grant(ctx context.Context, userID uuid.UUID, project, role, createdBy string) error
	// Revoke removes a user's grant on a Project (idempotent: no row ⇒ no error).
	Revoke(ctx context.Context, userID uuid.UUID, project string) error
}

// PostgresMembershipStore is the production MembershipStore over the shared *sql.DB.
type PostgresMembershipStore struct{ db *sql.DB }

// NewPostgresMembershipStore builds the production membership store.
func NewPostgresMembershipStore(db *sql.DB) *PostgresMembershipStore {
	return &PostgresMembershipStore{db: db}
}

// RoleForPrincipal joins auth.user (stable principal) → auth.project_membership in one query. A
// deactivated user resolves to ErrNoMembership too (deactivated_at IS NULL), so a soft-deleted
// account cannot ride a stale grant.
func (s *PostgresMembershipStore) RoleForPrincipal(ctx context.Context, principal, project string) (string, error) {
	var role string
	err := s.db.QueryRowContext(ctx, `
		SELECT m.role
		  FROM auth.project_membership m
		  JOIN auth.user u ON u.id = m.user_id
		 WHERE u.principal = $1 AND m.project = $2 AND u.deactivated_at IS NULL`,
		principal, project).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoMembership
	}
	if err != nil {
		return "", fmt.Errorf("auth: role for principal: %w", err)
	}
	return role, nil
}

// ListForUser returns the user's grants ordered by Project name (stable for the review surface).
func (s *PostgresMembershipStore) ListForUser(ctx context.Context, userID uuid.UUID) ([]ProjectMembership, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT project, role FROM auth.project_membership
		 WHERE user_id = $1 ORDER BY project`, userID)
	if err != nil {
		return nil, fmt.Errorf("auth: list memberships: %w", err)
	}
	defer rows.Close()
	var out []ProjectMembership
	for rows.Next() {
		var m ProjectMembership
		if err := rows.Scan(&m.Project, &m.Role); err != nil {
			return nil, fmt.Errorf("auth: scan membership: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Grant upserts against the UNIQUE(user_id, project) constraint: a re-grant updates the role and
// re-stamps provenance. An out-of-vocabulary role is refused by the DB CHECK (surfaced as an error).
func (s *PostgresMembershipStore) Grant(ctx context.Context, userID uuid.UUID, project, role, createdBy string) error {
	by := sql.NullString{String: createdBy, Valid: createdBy != ""}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO auth.project_membership (user_id, project, role, created_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, project)
		DO UPDATE SET role = EXCLUDED.role, created_by = EXCLUDED.created_by, created_at = now()`,
		userID, project, role, by)
	if err != nil {
		return fmt.Errorf("auth: grant membership: %w", err)
	}
	return nil
}

// Revoke deletes the grant (idempotent — a missing row is not an error).
func (s *PostgresMembershipStore) Revoke(ctx context.Context, userID uuid.UUID, project string) error {
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM auth.project_membership WHERE user_id = $1 AND project = $2`,
		userID, project); err != nil {
		return fmt.Errorf("auth: revoke membership: %w", err)
	}
	return nil
}
