package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ============================================================================
// User model (15.2) over auth.user (0006 + 0008). global_role ∈ {admin,user} is
// the base-permission axis; team_id stays NOT NULL (tenancy un-skippable — the
// API defaults a new user's team to the creating admin's team).
// ============================================================================

// Global role values (15.2).
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// User is the stored user record. PasswordHash is never serialized to API
// responses (internal/apiserver maps to a response shape without it).
type User struct {
	ID            uuid.UUID  `json:"id"`
	Username      string     `json:"username"`
	Principal     string     `json:"principal"`
	Email         *string    `json:"email,omitempty"`
	PasswordHash  string     `json:"-"`
	TeamID        uuid.UUID  `json:"teamId"`
	GlobalRole    string     `json:"globalRole"`
	CreatedAt     time.Time  `json:"createdAt"`
	CreatedBy     *string    `json:"createdBy,omitempty"`
	DeactivatedAt *time.Time `json:"deactivatedAt,omitempty"`
}

// IsAdmin reports the derived admin flag the resolver/AuthorContext carry.
func (u *User) IsAdmin() bool { return u != nil && u.GlobalRole == RoleAdmin }

// ErrNotFound is the store's not-there sentinel.
var ErrNotFound = errors.New("auth: not found")

// ErrLastAdmin is the lockout guard (PR #90 review finding 4): the mutation
// (demote / deactivate) would leave ZERO active admins, and bootstrapAdmin
// only runs on an empty user table — there would be no recovery path.
var ErrLastAdmin = errors.New("auth: refusing to remove the last active admin")

// UserStore is the persistence seam for auth.user (Postgres-backed in production;
// fakes in unit tests).
type UserStore interface {
	ByUsername(ctx context.Context, username string) (*User, error)
	ByID(ctx context.Context, id uuid.UUID) (*User, error)
	Create(ctx context.Context, u *User) error
	List(ctx context.Context, limit, offset int) ([]*User, int, error)
	Update(ctx context.Context, id uuid.UUID, upd UserUpdate) (*User, error)
	Deactivate(ctx context.Context, id uuid.UUID) error
	Count(ctx context.Context) (int, error)
}

// UserUpdate is the PATCH surface (15.2: role + deactivation + optional profile
// attrs). Zero fields are left untouched.
type UserUpdate struct {
	GlobalRole *string
	Email      *string // nil = unchanged; empty string = clear
}

// PostgresUserStore is the production UserStore over the shared *sql.DB.
type PostgresUserStore struct{ db *sql.DB }

// NewPostgresUserStore builds the production user store.
func NewPostgresUserStore(db *sql.DB) *PostgresUserStore { return &PostgresUserStore{db: db} }

const userColumns = `id, username, principal, email, password_hash, team_id, global_role, created_at, created_by, deactivated_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.Principal, &u.Email, &u.PasswordHash, &u.TeamID,
		&u.GlobalRole, &u.CreatedAt, &u.CreatedBy, &u.DeactivatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth: scan user: %w", err)
	}
	return &u, nil
}

// ByUsername fetches the user by login key (any state — the login path checks
// deactivation itself so the failure stays indistinguishable from unknown user).
func (s *PostgresUserStore) ByUsername(ctx context.Context, username string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM auth.user WHERE username = $1`, username))
}

// ByID fetches the user by id.
func (s *PostgresUserStore) ByID(ctx context.Context, id uuid.UUID) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM auth.user WHERE id = $1`, id))
}

// Create inserts a new user. The stable principal is minted here ("user:"+username)
// and can never be overridden by the caller — identity is immutable after this.
func (s *PostgresUserStore) Create(ctx context.Context, u *User) error {
	if u.Principal == "" {
		u.Principal = "user:" + u.Username
	}
	if u.GlobalRole == "" {
		u.GlobalRole = RoleUser
	}
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO auth.user (username, principal, password_hash, team_id, global_role, email, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, created_at`,
		u.Username, u.Principal, u.PasswordHash, u.TeamID, u.GlobalRole, u.Email, u.CreatedBy).
		Scan(&u.ID, &u.CreatedAt)
	if err != nil {
		return fmt.Errorf("auth: create user: %w", err)
	}
	return nil
}

// List returns one page of users ordered by creation (oldest first — stable for
// pagination) plus the total count.
func (s *PostgresUserStore) List(ctx context.Context, limit, offset int) ([]*User, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+userColumns+` FROM auth.user ORDER BY created_at, id LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("auth: list users: %w", err)
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("auth: list users: %w", err)
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM auth.user`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("auth: count users: %w", err)
	}
	return out, total, nil
}

// otherActiveAdminsSQL counts active admins EXCLUDING id — the last-admin guard.
const otherActiveAdminsSQL = `
SELECT count(*) FROM auth.user
 WHERE global_role = 'admin' AND deactivated_at IS NULL AND id <> $1`

// Update applies the PATCH surface and returns the updated row. Both column
// writes run in ONE transaction (PR #90 review: no torn half-updates), and a
// demotion that would leave zero active admins is refused with ErrLastAdmin.
func (s *PostgresUserStore) Update(ctx context.Context, id uuid.UUID, upd UserUpdate) (*User, error) {
	if upd.GlobalRole != nil {
		if *upd.GlobalRole != RoleAdmin && *upd.GlobalRole != RoleUser {
			return nil, fmt.Errorf("auth: global_role must be %q or %q", RoleAdmin, RoleUser)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("auth: update user: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if upd.GlobalRole != nil && *upd.GlobalRole != RoleAdmin {
		var others int
		if err := tx.QueryRowContext(ctx, otherActiveAdminsSQL, id).Scan(&others); err != nil {
			return nil, fmt.Errorf("auth: last-admin guard: %w", err)
		}
		if others == 0 {
			return nil, ErrLastAdmin
		}
	}
	if upd.GlobalRole != nil {
		if _, err := tx.ExecContext(ctx,
			`UPDATE auth.user SET global_role = $1 WHERE id = $2`, *upd.GlobalRole, id); err != nil {
			return nil, fmt.Errorf("auth: update user role: %w", err)
		}
	}
	if upd.Email != nil {
		if _, err := tx.ExecContext(ctx,
			`UPDATE auth.user SET email = NULLIF($1, '') WHERE id = $2`, *upd.Email, id); err != nil {
			return nil, fmt.Errorf("auth: update user email: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("auth: update user: %w", err)
	}
	return s.ByID(ctx, id)
}

// Deactivate soft-deletes (one-way deactivated_at stamp). Session revocation is the
// service layer's job (same transaction span is not required: the resolver filters
// deactivated_at IS NULL, so the cookie dies immediately regardless). Deactivating
// the LAST active admin is refused with ErrLastAdmin — there is no recovery path
// once the install has no admin and a non-empty user table.
func (s *PostgresUserStore) Deactivate(ctx context.Context, id uuid.UUID) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("auth: deactivate user: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var isAdmin bool
	err = tx.QueryRowContext(ctx,
		`SELECT (global_role = 'admin' AND deactivated_at IS NULL) FROM auth.user WHERE id = $1`, id).Scan(&isAdmin)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("auth: deactivate user: %w", err)
	}
	if isAdmin {
		var others int
		if err := tx.QueryRowContext(ctx, otherActiveAdminsSQL, id).Scan(&others); err != nil {
			return fmt.Errorf("auth: last-admin guard: %w", err)
		}
		if others == 0 {
			return ErrLastAdmin
		}
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE auth.user SET deactivated_at = now() WHERE id = $1 AND deactivated_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("auth: deactivate user: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("auth: deactivate user: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// Count returns the total user count (bootstrap idempotency probe).
func (s *PostgresUserStore) Count(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM auth.user`).Scan(&n); err != nil {
		return 0, fmt.Errorf("auth: count users: %w", err)
	}
	return n, nil
}

// ============================================================================
// Session store (15.1) over auth.session — the WRITE side of what
// PostgresSessionResolver reads. Tokens are 32 random bytes, base64url-encoded,
// handed to the caller ONLY to set as the HttpOnly cookie; the row stores
// sha256(token). Rotation mints a fresh token and revokes the old one atomically.
// ============================================================================

// SessionStore is the session persistence seam. Resolve carries the SAME
// fail-closed live-session predicate the apiserver's PostgresSessionResolver
// enforces (revoked_at IS NULL AND expires_at > now(), user not deactivated) —
// one source of truth for "is this token alive".
type SessionStore interface {
	Resolve(ctx context.Context, token string) (uuid.UUID, error)
	Create(ctx context.Context, userID uuid.UUID, ttl time.Duration) (Session, error)
	Rotate(ctx context.Context, token string, ttl time.Duration) (Session, error)
	Revoke(ctx context.Context, token string) error
	RevokeAllForUser(ctx context.Context, userID uuid.UUID) error
	PruneExpired(ctx context.Context) (int64, error)
}

// Session is a freshly minted session's caller-visible state (token + metadata).
type Session struct {
	Token     string
	ID        uuid.UUID
	UserID    uuid.UUID
	ExpiresAt time.Time
}

// PostgresSessionStore is the production SessionStore.
type PostgresSessionStore struct{ db *sql.DB }

// NewPostgresSessionStore builds the production session store.
func NewPostgresSessionStore(db *sql.DB) *PostgresSessionStore { return &PostgresSessionStore{db: db} }

// mintToken generates the opaque bearer token (256-bit entropy, base64url).
//
// CRITICAL (ISI-3541): the persisted token_hash MUST be sha256 of the EMITTED token
// STRING — the exact bytes handed out as the ksquad_session cookie — because every
// lookup (Resolve/Rotate/Logout here, and the §13 PostgresSessionResolver) computes
// sha256([]byte(token)) over that same cookie string. Hashing the raw entropy instead
// yields sha256(raw) != sha256(base64url(raw)), so no session ever resolves and every
// gated route 401s while login still "succeeds" (it only INSERTs, never looks up).
func mintToken() (string, []byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("auth: read session entropy: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	return token, sum[:], nil
}

// Create inserts a live session row and returns the bearer token (never persisted).
func (s *PostgresSessionStore) Create(ctx context.Context, userID uuid.UUID, ttl time.Duration) (Session, error) {
	token, hash, err := mintToken()
	if err != nil {
		return Session{}, err
	}
	var sess Session
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO auth.session (token_hash, user_id, expires_at)
		VALUES ($1, $2, now() + ($3 || ' seconds')::interval)
		RETURNING id, user_id, expires_at`,
		hash, userID, fmt.Sprintf("%d", int(ttl.Seconds()))).
		Scan(&sess.ID, &sess.UserID, &sess.ExpiresAt)
	if err != nil {
		return Session{}, fmt.Errorf("auth: create session: %w", err)
	}
	sess.Token = token
	return sess, nil
}

// Rotate atomically replaces a live session: a new row is inserted and the old one
// revoked in one transaction. An unknown/expired/revoked token rotates nothing
// (ErrNotFound) — refresh must fail closed exactly like resolution does.
func (s *PostgresSessionStore) Rotate(ctx context.Context, token string, ttl time.Duration) (Session, error) {
	sum := sha256.Sum256([]byte(token))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, fmt.Errorf("auth: rotate session: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		sess   Session
		userID uuid.UUID
	)
	err = tx.QueryRowContext(ctx, `
		UPDATE auth.session
		   SET revoked_at = now()
		 WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now()
		RETURNING user_id`, sum[:]).
		Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("auth: rotate session (revoke old): %w", err)
	}

	newToken, hash, err := mintToken()
	if err != nil {
		return Session{}, err
	}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO auth.session (token_hash, user_id, expires_at)
		VALUES ($1, $2, now() + ($3 || ' seconds')::interval)
		RETURNING id, expires_at`,
		hash, userID, fmt.Sprintf("%d", int(ttl.Seconds()))).
		Scan(&sess.ID, &sess.ExpiresAt)
	if err != nil {
		return Session{}, fmt.Errorf("auth: rotate session (mint new): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("auth: rotate session: %w", err)
	}
	sess.UserID = userID
	sess.Token = newToken
	return sess, nil
}

// Revoke signs out one session (idempotent — revoking a dead/unknown token is a no-op
// so logout can never fail on a stale cookie).
func (s *PostgresSessionStore) Revoke(ctx context.Context, token string) error {
	sum := sha256.Sum256([]byte(token))
	if _, err := s.db.ExecContext(ctx,
		`UPDATE auth.session SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL`, sum[:]); err != nil {
		return fmt.Errorf("auth: revoke session: %w", err)
	}
	return nil
}

// RevokeAllForUser kills every live session of a user (deactivation / password reset).
func (s *PostgresSessionStore) RevokeAllForUser(ctx context.Context, userID uuid.UUID) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE auth.session SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID); err != nil {
		return fmt.Errorf("auth: revoke user sessions: %w", err)
	}
	return nil
}

// PruneExpired is the janitor (0006 retention note): expired rows are operational
// residue, deleted so the table stays hot-index-only-live.
func (s *PostgresSessionStore) PruneExpired(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM auth.session WHERE expires_at < now()`)
	if err != nil {
		return 0, fmt.Errorf("auth: prune sessions: %w", err)
	}
	return res.RowsAffected()
}

// resolveLiveSQL is the fail-closed live-session lookup (mirrors the apiserver's
// PostgresSessionResolver, extended with the 0008 deactivated_at guard).
const resolveLiveSQL = `
SELECT s.user_id
  FROM auth.session s
  JOIN auth.user u ON u.id = s.user_id
 WHERE s.token_hash = $1
   AND s.revoked_at IS NULL
   AND s.expires_at > now()
   AND u.deactivated_at IS NULL`

// Resolve returns the userID bound to a LIVE session token (fail-closed on any doubt).
func (s *PostgresSessionStore) Resolve(ctx context.Context, token string) (uuid.UUID, error) {
	if token == "" {
		return uuid.UUID{}, ErrNotFound
	}
	sum := sha256.Sum256([]byte(token))
	var userID uuid.UUID
	err := s.db.QueryRowContext(ctx, resolveLiveSQL, sum[:]).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.UUID{}, ErrNotFound
	}
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("auth: resolve session: %w", err)
	}
	return userID, nil
}
