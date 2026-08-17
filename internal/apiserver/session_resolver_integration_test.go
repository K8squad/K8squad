//go:build auth_integration

// Apiserver integration test for the §13 session-resolution backing against a REAL Postgres (ISI-2758,
// ADR-033 / §12.3). Build-tag gated so it never runs in the default unit lane; CI provisions Postgres
// and runs
//
//	go test -tags=auth_integration ./internal/apiserver/...
//
// It applies the SHIPPED migration (db/migrations/0006_auth_schema.sql) — not inline DDL — so any drift
// between the migration and PostgresSessionResolver's query goes RED here. When DATABASE_URL is unset the
// test SKIPS (mirrors the discussion integration test), so a developer without Postgres is not blocked.
//
// It proves the fail-closed resolution contract end to end: a live session resolves to the joined
// identity + Team scope; a revoked, expired, or unknown token resolves to ErrNoSession with no leaked
// principal; and — the whole point of this seam — the CookieAuthenticator over this resolver turns a
// real HTTP request's ksquad_session cookie into an authenticated AuthorContext.
package apiserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"
)

func openAuthTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL unset — skipping the apiserver auth-session integration test (needs real Postgres)")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping db: %v", err)
	}
	return db
}

// applyAuthMigration applies the SHIPPED 0006 migration into a clean `auth` schema.
func applyAuthMigration(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS auth CASCADE`); err != nil {
		t.Fatalf("reset auth schema: %v", err)
	}
	candidates := []string{
		filepath.Join("..", "..", "db", "migrations", "0006_auth_schema.sql"),
		filepath.Join("db", "migrations", "0006_auth_schema.sql"),
	}
	if d := os.Getenv("AUTH_MIGRATIONS_DIR"); d != "" {
		candidates = append([]string{filepath.Join(d, "0006_auth_schema.sql")}, candidates...)
	}
	var sqlBytes []byte
	var err error
	for _, c := range candidates {
		if sqlBytes, err = os.ReadFile(c); err == nil {
			break
		}
	}
	if sqlBytes == nil {
		t.Fatalf("could not locate 0006_auth_schema.sql (tried %v); set AUTH_MIGRATIONS_DIR", candidates)
	}
	if _, err := db.ExecContext(ctx, string(sqlBytes)); err != nil {
		t.Fatalf("the shipped auth migration failed to apply against real Postgres: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		_, _ = db.ExecContext(cctx, `DROP SCHEMA IF EXISTS auth CASCADE`)
	})
}

// seedUser inserts a local-cred principal (as the Epic-15 login path will) and returns its user id.
func seedUser(t *testing.T, db *sql.DB, principal string, team uuid.UUID, isAdmin bool) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var id uuid.UUID
	err := db.QueryRowContext(ctx, `
		INSERT INTO auth.user (username, principal, password_hash, team_id, is_admin)
		VALUES ($1, $1, $2, $3, $4) RETURNING id`,
		principal,
		// A syntactically-valid argon2id PHC placeholder — the resolver never reads it; login/reset (Epic 15) does.
		"$argon2id$v=19$m=65536,t=3,p=4$c29tZXNhbHQ$0123456789abcdef0123456789abcdef",
		team, isAdmin,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed user %s: %v", principal, err)
	}
	return id
}

// seedSession mints a session bound to user with the given lifetime and revocation, storing sha256(token).
func seedSession(t *testing.T, db *sql.DB, userID uuid.UUID, token string, expires time.Time, revoked bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sum := sha256.Sum256([]byte(token))
	var revokedAt any
	if revoked {
		revokedAt = time.Now()
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO auth.session (token_hash, user_id, expires_at, revoked_at)
		VALUES ($1, $2, $3, $4)`, sum[:], userID, expires, revokedAt); err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

func TestPostgresSessionResolverResolvesLiveSession(t *testing.T) {
	db := openAuthTestDB(t)
	defer db.Close()
	applyAuthMigration(t, db)

	team := uuid.New()
	seedUser(t, db, "user:amelia", team, true)
	seedSession(t, db, mustUserID(t, db, "user:amelia"), "live-token", time.Now().Add(time.Hour), false)

	r := NewPostgresSessionResolver(db)
	auth, err := r.Resolve(context.Background(), "live-token")
	if err != nil {
		t.Fatalf("Resolve(live) unexpected err: %v", err)
	}
	if auth.Principal != "user:amelia" {
		t.Errorf("principal = %q, want user:amelia", auth.Principal)
	}
	if auth.TeamID != team {
		t.Errorf("teamID = %v, want %v", auth.TeamID, team)
	}
	if !auth.IsAdmin {
		t.Error("isAdmin = false, want true (seeded admin)")
	}
	// A browser session is a human post: agent/run linkage must stay nil (§7.5, agent-vs-human is derived).
	if auth.AgentID != nil || auth.RunID != nil {
		t.Errorf("a console session must be human-authored: agentID=%v runID=%v", auth.AgentID, auth.RunID)
	}
}

func TestPostgresSessionResolverFailsClosedDB(t *testing.T) {
	db := openAuthTestDB(t)
	defer db.Close()
	applyAuthMigration(t, db)

	team := uuid.New()
	seedUser(t, db, "user:bob", team, false)
	uid := mustUserID(t, db, "user:bob")
	seedSession(t, db, uid, "expired-token", time.Now().Add(-time.Second), false)
	seedSession(t, db, uid, "revoked-token", time.Now().Add(time.Hour), true)

	r := NewPostgresSessionResolver(db)
	for _, token := range []string{"expired-token", "revoked-token", "unknown-token"} {
		auth, err := r.Resolve(context.Background(), token)
		if err != ErrNoSession {
			t.Errorf("Resolve(%q) err = %v, want ErrNoSession (fail-closed)", token, err)
		}
		if auth.Principal != "" {
			t.Errorf("Resolve(%q) leaked principal %q on a fail-closed path", token, auth.Principal)
		}
	}
}

// The whole point of the seam: a real HTTP request carrying the ksquad_session cookie authenticates
// through the CookieAuthenticator over the production resolver.
func TestCookieAuthenticatorOverPostgresResolver(t *testing.T) {
	db := openAuthTestDB(t)
	defer db.Close()
	applyAuthMigration(t, db)

	team := uuid.New()
	seedUser(t, db, "user:carol", team, false)
	seedSession(t, db, mustUserID(t, db, "user:carol"), "carol-cookie", time.Now().Add(time.Hour), false)

	authn := NewCookieAuthenticator(NewPostgresSessionResolver(db))

	// A request with the live cookie authenticates.
	req := &http.Request{Header: http.Header{}}
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "carol-cookie"})
	if auth, ok := authn.Authenticate(req.WithContext(context.Background())); !ok || auth.Principal != "user:carol" {
		t.Fatalf("Authenticate(live cookie) = %+v,%v; want user:carol,true", auth, ok)
	}

	// No cookie ⇒ unauthenticated.
	if _, ok := authn.Authenticate((&http.Request{Header: http.Header{}}).WithContext(context.Background())); ok {
		t.Fatal("Authenticate(no cookie): expected !ok")
	}

	// A bogus cookie value ⇒ unauthenticated (deny-by-default).
	bad := &http.Request{Header: http.Header{}}
	bad.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "not-a-real-session"})
	if _, ok := authn.Authenticate(bad.WithContext(context.Background())); ok {
		t.Fatal("Authenticate(bogus cookie): expected !ok")
	}
}

func mustUserID(t *testing.T, db *sql.DB, principal string) (id uuid.UUID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.QueryRowContext(ctx, `SELECT id FROM auth.user WHERE principal = $1`, principal).Scan(&id); err != nil {
		t.Fatalf("lookup user %s: %v", principal, err)
	}
	return id
}
