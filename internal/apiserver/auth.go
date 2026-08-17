package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
)

// ============================================================================
// §13 / ADR-033 identity seam — the apiserver mints the internal identity from
// the forwarded session cookie, NEVER from a BFF-asserted principal header.
// ============================================================================
//
// Contract (console/lib/bff.ts, story 8.7d Dev Notes): the Next.js BFF is the ONE
// authorization choke point. It holds the HttpOnly `ksquad_session` cookie and forwards
// it — and ONLY it — upstream. It never asserts a principal header, so the apiserver
// must resolve the caller's identity+tenancy itself from the opaque session token. That
// resolution is the "internal JWT mint": the session token → server-derived AuthorContext
// (principal + Team scope) that every §7.5 write stamps its provenance from.
//
// The resolution BACKING (the auth/session store) is the auth `user` + `session` schema in
// db/migrations/0006_auth_schema.sql. This file ships the seam — the SessionResolver interface
// + the CookieAuthenticator that plugs it into the §13 discussion.BFFAuthz choke point — plus an
// in-memory resolver for dev/test. The production PostgresSessionResolver over that schema lives in
// session_resolver.go and is wired as the default in cmd/apiserver/main.go (ISI-2758).

// SessionCookieName is the HttpOnly session cookie the BFF forwards upstream (arch §12.3 /
// ADR-033). It mirrors console/lib/bff.ts `sessionCookieName()` default; a deployment that
// overrides KSQUAD_SESSION_COOKIE on the BFF must set the matching apiserver config.
const SessionCookieName = "ksquad_session"

// ErrNoSession is returned by a SessionResolver when the token is absent, expired, or unknown.
// It is deliberately opaque: the caller answers 401 the same way for every failure so a
// probing client cannot distinguish "no such session" from "expired" (fail-closed).
var ErrNoSession = errors.New("apiserver: no valid session")

// SessionResolver turns an opaque session token (the value of the forwarded ksquad_session
// cookie) into the server-derived AuthorContext — the sole identity+tenancy source the write
// path trusts. It is the ONE place a caller's identity enters the apiserver. A resolver MUST
// fail closed: any doubt returns ErrNoSession, never a partial/guessed principal.
type SessionResolver interface {
	Resolve(ctx context.Context, token string) (discussion.AuthorContext, error)
}

// CookieAuthenticator adapts a SessionResolver to the discussion.Authenticator seam consumed
// by discussion.BFFAuthz. It reads the forwarded ksquad_session cookie, resolves it, and hands
// the resulting AuthorContext to the choke point. A missing cookie, an empty token, or a
// resolver error all yield ok=false → the middleware answers 401 before any handler/store runs
// (deny-by-default). It NEVER reads a principal from a header or the body.
type CookieAuthenticator struct {
	Resolver   SessionResolver
	CookieName string // defaults to SessionCookieName when empty
}

// NewCookieAuthenticator builds the §13 authenticator over the given resolver.
func NewCookieAuthenticator(r SessionResolver) *CookieAuthenticator {
	return &CookieAuthenticator{Resolver: r, CookieName: SessionCookieName}
}

// Authenticate implements discussion.Authenticator. It is called by discussion.BFFAuthz for
// every gated request. ok=false ⇒ unauthenticated (401); the request never reaches a handler,
// so provenance-stamping fails closed.
func (a *CookieAuthenticator) Authenticate(r *http.Request) (discussion.AuthorContext, bool) {
	if a == nil || a.Resolver == nil {
		return discussion.AuthorContext{}, false // no resolver wired ⇒ fail closed
	}
	name := a.CookieName
	if name == "" {
		name = SessionCookieName
	}
	c, err := r.Cookie(name)
	if err != nil || c.Value == "" {
		return discussion.AuthorContext{}, false
	}
	auth, err := a.Resolver.Resolve(r.Context(), c.Value)
	if err != nil || auth.Principal == "" {
		return discussion.AuthorContext{}, false
	}
	return auth, true
}

// StaticSessionResolver is an in-memory SessionResolver keyed by opaque token. It backs local
// runs and tests so the host is exercisable end-to-end without the (not-yet-built) auth store.
// It is NOT for production: tokens are compared in plaintext and never expire. Production wires
// PostgresSessionResolver over the real auth.session table (session_resolver.go, ISI-2758).
type StaticSessionResolver struct {
	Sessions map[string]discussion.AuthorContext
}

// Resolve looks the token up in the static map, failing closed on a miss.
func (s *StaticSessionResolver) Resolve(_ context.Context, token string) (discussion.AuthorContext, error) {
	if s == nil {
		return discussion.AuthorContext{}, ErrNoSession
	}
	auth, ok := s.Sessions[token]
	if !ok || auth.Principal == "" {
		return discussion.AuthorContext{}, ErrNoSession
	}
	return auth, nil
}

// staticSession is the on-disk shape of a dev session (KSQUAD_DEV_SESSIONS file). It mirrors
// AuthorContext but keeps teamId as a string for hand-authoring.
type staticSession struct {
	Token     string  `json:"token"`
	Principal string  `json:"principal"`
	TeamID    string  `json:"teamId"`
	AgentID   *string `json:"agentId,omitempty"`
	RunID     *string `json:"runId,omitempty"`
	IsAdmin   bool    `json:"isAdmin,omitempty"`
}

// LoadStaticSessions reads a JSON array of dev sessions into a StaticSessionResolver. It exists
// ONLY to make the host runnable end-to-end locally before the Postgres session store lands; it
// must never be wired in production (there is no expiry, no signature). Callers should log a loud
// warning when using it.
func LoadStaticSessions(path string) (*StaticSessionResolver, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read dev sessions %s: %w", path, err)
	}
	var rows []staticSession
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("parse dev sessions %s: %w", path, err)
	}
	m := make(map[string]discussion.AuthorContext, len(rows))
	for i, row := range rows {
		if row.Token == "" || row.Principal == "" || row.TeamID == "" {
			return nil, fmt.Errorf("dev session %d: token, principal and teamId are required", i)
		}
		team, err := uuid.Parse(row.TeamID)
		if err != nil {
			return nil, fmt.Errorf("dev session %d: invalid teamId %q: %w", i, row.TeamID, err)
		}
		m[row.Token] = discussion.AuthorContext{
			Principal: row.Principal,
			TeamID:    team,
			AgentID:   row.AgentID,
			RunID:     row.RunID,
			IsAdmin:   row.IsAdmin,
		}
	}
	return &StaticSessionResolver{Sessions: m}, nil
}

// deniedResolver fails every resolution closed. Production now defaults to PostgresSessionResolver
// (session_resolver.go); this remains the explicit deny-all fallback — wire it to make the §13 choke
// point answer 401 for every gated request (deny-by-default) rather than trust an unauthenticated caller.
type deniedResolver struct{}

// Resolve always denies.
func (deniedResolver) Resolve(context.Context, string) (discussion.AuthorContext, error) {
	return discussion.AuthorContext{}, ErrNoSession
}

// DeniedResolver returns a resolver that denies all sessions (fail-closed default).
func DeniedResolver() SessionResolver { return deniedResolver{} }
