package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ============================================================================
// Service — the 15.1 auth core: login / refresh / logout / me. It wires the user
// store, session store, JWT issuer, and per-IP failure limiter into the one place
// that mints identity. Every failure that is caller-visible collapses to the
// opaque ErrInvalidCredentials (no user enumeration, no oracle); the limiter
// answers ErrRateLimited before any credential work runs, and only FAILED
// attempts consume its budget (PR #90 review finding 5).
// ============================================================================

// Caller-visible service errors.
var (
	// ErrInvalidCredentials is login's single failure for unknown user / wrong
	// password / deactivated account (indistinguishable by design).
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	// ErrRateLimited is the brute-force brake answer (15.1: 5 failures/15min/IP default).
	ErrRateLimited = errors.New("auth: too many attempts")
	// ErrSessionExpired covers refresh/me/logout over a dead session token.
	ErrSessionExpired = errors.New("auth: session expired")
)

// ServiceConfig carries the tunables (chart ConfigMap surface, 9.5).
type ServiceConfig struct {
	SessionTTL time.Duration // edge session lifetime; default 24h
}

// Service is the auth core. Construct with NewService; zero-value fields fail closed.
type Service struct {
	Users    UserStore
	Sessions SessionStore
	JWT      *JWTIssuer
	Limiter  *RateLimiter
	cfg      ServiceConfig
}

// NewService assembles the core. Defaults: session TTL 24h.
func NewService(users UserStore, sessions SessionStore, jwt *JWTIssuer, limiter *RateLimiter, cfg ServiceConfig) *Service {
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 24 * time.Hour
	}
	return &Service{Users: users, Sessions: sessions, JWT: jwt, Limiter: limiter, cfg: cfg}
}

// SessionTTL reports the edge session lifetime (cookie Max-Age / response expiry).
func (s *Service) SessionTTL() time.Duration { return s.cfg.SessionTTL }

// dummyHash burns one argon2id derivation so an unknown-username login costs the
// same wall time as a wrong password (timing-side enumeration defense). Seeded
// lazily on first use, then reused — it only needs to be EXPENSIVE, not fresh.
var dummyHash = func() func() string {
	var cached string
	return func() string {
		if cached == "" {
			h, err := HashPassword("ksquad-timing-dummy-password")
			if err != nil {
				// entropy failure is unrecoverable for hashing generally
				panic(fmt.Sprintf("auth: seed timing dummy: %v", err))
			}
			cached = h
		}
		return cached
	}
}()

// LoginResult is the successful login's mint: the opaque session token (for the
// HttpOnly cookie), the internal JWT, and the authenticated user.
type LoginResult struct {
	SessionToken string
	AccessToken  string
	ExpiresIn    int64 // JWT lifetime, seconds
	User         *User
}

// Login authenticates username+password and mints the edge session + internal JWT.
// clientIP feeds the per-IP failure limiter.
//
// There is deliberately NO group/claim input here (PR #90 review finding 3):
// group→access mapping (15.9) consumes claims from a TRUSTED OIDC token
// exchange, never from the client's request body. That leg lands with the OIDC
// login flow; the mapping itself lives in groupmapping.go.
func (s *Service) Login(ctx context.Context, username, password, clientIP string) (*LoginResult, error) {
	if !s.Limiter.Allow(clientIP) {
		return nil, ErrRateLimited
	}
	u, err := s.Users.ByUsername(ctx, username)
	if err != nil {
		_ = VerifyPassword(password, dummyHash()) // equalize timing
		s.Limiter.Failure(clientIP)
		return nil, ErrInvalidCredentials
	}
	if u.DeactivatedAt != nil {
		_ = VerifyPassword(password, dummyHash())
		s.Limiter.Failure(clientIP)
		return nil, ErrInvalidCredentials
	}
	if err := VerifyPassword(password, u.PasswordHash); err != nil {
		s.Limiter.Failure(clientIP)
		return nil, ErrInvalidCredentials
	}
	s.Limiter.Success(clientIP) // authentic login: clear the brute-force window

	sess, err := s.Sessions.Create(ctx, u.ID, s.cfg.SessionTTL)
	if err != nil {
		return nil, fmt.Errorf("auth: mint session: %w", err)
	}
	access, err := s.mintJWT(u, sess.ID)
	if err != nil {
		_ = s.Sessions.Revoke(ctx, sess.Token) // never leak a session the JWT failed for
		return nil, err
	}
	return &LoginResult{SessionToken: sess.Token, AccessToken: access, ExpiresIn: int64(s.JWT.TTL().Seconds()), User: u}, nil
}

// Refresh rotates a live session (old token dies atomically, new cookie + JWT mint).
func (s *Service) Refresh(ctx context.Context, sessionToken string) (*LoginResult, error) {
	sess, err := s.Sessions.Rotate(ctx, sessionToken, s.cfg.SessionTTL)
	if err != nil {
		return nil, ErrSessionExpired
	}
	u, err := s.Users.ByID(ctx, sess.UserID)
	if err != nil || u.DeactivatedAt != nil {
		// The row died between mint and lookup — kill the fresh session, fail closed.
		_ = s.Sessions.Revoke(ctx, sess.Token)
		return nil, ErrSessionExpired
	}
	access, err := s.mintJWT(u, sess.ID)
	if err != nil {
		_ = s.Sessions.Revoke(ctx, sess.Token)
		return nil, err
	}
	return &LoginResult{SessionToken: sess.Token, AccessToken: access, ExpiresIn: int64(s.JWT.TTL().Seconds()), User: u}, nil
}

// Logout revokes the session (idempotent: a stale cookie logs out cleanly).
func (s *Service) Logout(ctx context.Context, sessionToken string) error {
	return s.Sessions.Revoke(ctx, sessionToken)
}

// Me resolves a session token to its user (the /auth/me probe; 401-shape on any doubt).
func (s *Service) Me(ctx context.Context, sessionToken string) (*User, error) {
	userID, err := s.Sessions.Resolve(ctx, sessionToken)
	if err != nil {
		return nil, ErrSessionExpired
	}
	u, err := s.Users.ByID(ctx, userID)
	if err != nil || u.DeactivatedAt != nil {
		return nil, ErrSessionExpired
	}
	return u, nil
}

// mintJWT signs the internal token for an authenticated user+session pair.
func (s *Service) mintJWT(u *User, sessionID uuid.UUID) (string, error) {
	tok, err := s.JWT.Mint(Claims{
		Subject:   u.Principal,
		UserID:    u.ID.String(),
		TeamID:    u.TeamID.String(),
		Role:      u.GlobalRole,
		SessionID: sessionID.String(),
	})
	if err != nil {
		return "", fmt.Errorf("auth: mint jwt: %w", err)
	}
	return tok, nil
}
