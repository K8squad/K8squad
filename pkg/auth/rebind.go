package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// teamIDUpdater is the narrow user-store surface a rebind needs: the compare-and-
// swap that moves a caller's team_id and returns the affected user id.
type teamIDUpdater interface {
	UpdateTeamID(ctx context.Context, principal string, from, to uuid.UUID) (uuid.UUID, error)
}

// sessionRevoker is the narrow session-store surface a rebind needs: kill every
// live session of a user so the next login re-mints the JWT.
type sessionRevoker interface {
	RevokeAllForUser(ctx context.Context, userID uuid.UUID) error
}

// TenancyRebinder is the auth-side half of the compose first-team-create seam
// (ISI-3924 / ADR-0009). The compose write surface decides WHEN a rebind is
// legal (caller is a global admin whose team_id backs no Team CR); this type
// performs the two coupled writes that seam requires: the guarded team_id move
// and the session invalidation. Keeping both here means the compose service
// depends on ONE narrow behavior, not on the user AND session stores directly.
type TenancyRebinder struct {
	users    teamIDUpdater
	sessions sessionRevoker
}

// NewTenancyRebinder wires the rebinder over the auth stores (in production the
// Postgres user + session stores over the shared *sql.DB).
func NewTenancyRebinder(users teamIDUpdater, sessions sessionRevoker) *TenancyRebinder {
	return &TenancyRebinder{users: users, sessions: sessions}
}

// RebindTeamID compare-and-swaps `principal`'s team_id from `from` to `to` and,
// on a successful swap, revokes `principal`'s live sessions so their next login
// mints a JWT carrying the new tid. It returns whether a row was actually
// rebound. The CAS on `from` (in the store) guarantees an already-bound admin —
// whose team_id no longer equals the dangling root — matches nothing and is
// left untouched (rebound=false), so a second Team create is a normal scoped
// create, never a tenancy hijack. A `from`/`to` that are already equal is a
// no-op (nothing to move); the store's CAS still runs but the caller should not
// reach here in that case (the compose guard skips it).
//
// The session revoke runs AFTER the team_id write succeeds. A revoke failure is
// returned (surfaced + logged by the caller) rather than swallowed: the team_id
// is already correct, but a stale session would still carry the old tid until it
// expires, so the operator must know.
func (r *TenancyRebinder) RebindTeamID(ctx context.Context, principal string, from, to uuid.UUID) (bool, error) {
	if r == nil || r.users == nil {
		return false, errors.New("auth: tenancy rebinder not configured")
	}
	userID, err := r.users.UpdateTeamID(ctx, principal, from, to)
	if errors.Is(err, ErrNotFound) {
		// Nothing matched the CAS: already bound / already moved. Not an error.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if r.sessions != nil {
		if err := r.sessions.RevokeAllForUser(ctx, userID); err != nil {
			return true, fmt.Errorf("auth: team_id rebound but session invalidation failed: %w", err)
		}
	}
	return true, nil
}
