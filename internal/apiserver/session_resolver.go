package apiserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
)

// ============================================================================
// PostgresSessionResolver — the production §13 identity backing (ADR-033 / §12.3, ISI-2758)
// ============================================================================
//
// This is the resolver auth.go's SessionResolver seam has been waiting for: the store that turns the
// opaque, forwarded `ksquad_session` cookie into a server-derived AuthorContext. It reads the auth
// schema (db/migrations/0006_auth_schema.sql):
//
//	SELECT u.principal, u.team_id, u.is_admin
//	  FROM auth.session s JOIN auth.user u ON u.id = s.user_id
//	 WHERE s.token_hash = sha256($cookie) AND s.revoked_at IS NULL AND s.expires_at > now();
//
// It is FAIL-CLOSED by construction:
//   - the cookie value is hashed (sha256) before it touches SQL — we never store or compare the
//     plaintext bearer token, so a DB read cannot replay a live session, and the lookup is a plain
//     equality on the digest (no LIKE, no injection surface);
//   - the WHERE clause admits ONLY a live session (not revoked, not expired), so an invalidated or
//     stale cookie resolves to nothing;
//   - sql.ErrNoRows — and an empty/missing token — map to ErrNoSession, so the §13 choke point answers
//     an indistinguishable 401 for "no such session" / "expired" / "revoked" (no enumeration oracle);
//   - any OTHER database error is returned as-is (still non-nil ⇒ CookieAuthenticator denies), so a
//     backing outage denies rather than fails open.
//
// A resolved console session is always human-authored: AgentID/RunID stay nil (agent-vs-human is
// DERIVED from those being set, §7.5). An agent posting from within a Run carries its identity through
// the Run's own credential path, not a browser session cookie.
type PostgresSessionResolver struct {
	db *sql.DB
}

// NewPostgresSessionResolver builds the production resolver over the shared Postgres DSN (the same
// *sql.DB the host opened and pinged at startup).
func NewPostgresSessionResolver(db *sql.DB) *PostgresSessionResolver {
	return &PostgresSessionResolver{db: db}
}

// resolveSessionSQL is the fail-closed lookup. It selects an identity ONLY for a live session; the
// (revoked_at IS NULL AND expires_at > now()) predicate is the fail-closed guard, evaluated in the
// database so a caller can never widen it.
const resolveSessionSQL = `
SELECT u.principal, u.team_id, u.is_admin
  FROM auth.session s
  JOIN auth.user u ON u.id = s.user_id
 WHERE s.token_hash = $1
   AND s.revoked_at IS NULL
   AND s.expires_at > now()`

// Resolve implements SessionResolver. It hashes the opaque token and looks up the single live session
// bound to it, returning the joined user's server-derived identity + Team scope. Any doubt — empty
// token, no live session, or a resolver that was built without a DB — fails closed with ErrNoSession.
func (r *PostgresSessionResolver) Resolve(ctx context.Context, token string) (discussion.AuthorContext, error) {
	if r == nil || r.db == nil || token == "" {
		return discussion.AuthorContext{}, ErrNoSession
	}

	// Hash the bearer token before it reaches SQL; the auth.session store only ever holds sha256(token).
	sum := sha256.Sum256([]byte(token))

	var (
		principal string
		teamID    uuid.UUID
		isAdmin   bool
	)
	err := r.db.QueryRowContext(ctx, resolveSessionSQL, sum[:]).Scan(&principal, &teamID, &isAdmin)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// No live session for this token (absent / expired / revoked) — deny indistinguishably.
		return discussion.AuthorContext{}, ErrNoSession
	case err != nil:
		// A backing outage denies (non-nil ⇒ 401) rather than failing open.
		return discussion.AuthorContext{}, err
	}
	if principal == "" {
		// Defence in depth: a NOT-NULL principal is guaranteed by the schema, but never hand back an
		// empty identity even if that invariant were violated.
		return discussion.AuthorContext{}, ErrNoSession
	}

	return discussion.AuthorContext{
		Principal: principal,
		TeamID:    teamID,
		IsAdmin:   isAdmin,
		// AgentID / RunID intentionally nil: a browser session is a human post (§7.5).
	}, nil
}
