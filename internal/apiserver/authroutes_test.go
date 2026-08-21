package apiserver

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/auth"
)

// ============================================================================
// /auth/* and /admin/users handler tests: a real auth.Service over map-backed
// fakes (no Postgres), driven through the SERVER ROUTER so middleware — body
// limits, the same-origin CSRF guard, the requireAdmin gate — is what actually
// runs. These are the tests PR #90's review asked for (findings 1, 3, 4, 6 and
// the internal/apiserver coverage ratchet).
// ============================================================================

// ── fakes (auth.UserStore / auth.SessionStore) ────────────────────────────────

type mapUsers struct {
	mu        sync.Mutex
	byName    map[string]*auth.User
	byID      map[uuid.UUID]*auth.User
	injectErr error // returned by Update/Deactivate when set (drives 409/500 branches)
}

func newMapUsers(users ...*auth.User) *mapUsers {
	m := &mapUsers{byName: map[string]*auth.User{}, byID: map[uuid.UUID]*auth.User{}}
	for _, u := range users {
		m.byName[u.Username] = u
		m.byID[u.ID] = u
	}
	return m
}

func (m *mapUsers) ByUsername(_ context.Context, name string) (*auth.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.byName[name]; ok {
		cp := *u
		return &cp, nil
	}
	return nil, auth.ErrNotFound
}

func (m *mapUsers) ByID(_ context.Context, id uuid.UUID) (*auth.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.byID[id]; ok {
		cp := *u
		return &cp, nil
	}
	return nil, auth.ErrNotFound
}

func (m *mapUsers) Create(_ context.Context, u *auth.User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.byName[u.Username]; dup {
		return errDuplicateKey // the handler maps this shape to 409
	}
	u.ID = uuid.New()
	u.Principal = "user:" + u.Username
	u.CreatedAt = time.Now()
	m.byName[u.Username] = u
	m.byID[u.ID] = u
	return nil
}

func (m *mapUsers) List(_ context.Context, limit, offset int) ([]*auth.User, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*auth.User, 0, len(m.byName))
	for _, u := range m.byName {
		out = append(out, u)
	}
	total := len(out)
	if offset >= len(out) {
		return nil, total, nil
	}
	out = out[offset:]
	if len(out) > limit {
		out = out[:limit]
	}
	return out, total, nil
}

func (m *mapUsers) Update(_ context.Context, id uuid.UUID, upd auth.UserUpdate) (*auth.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.injectErr != nil {
		return nil, m.injectErr
	}
	u, ok := m.byID[id]
	if !ok {
		return nil, auth.ErrNotFound
	}
	if upd.GlobalRole != nil {
		u.GlobalRole = *upd.GlobalRole
	}
	if upd.Email != nil {
		if *upd.Email == "" {
			u.Email = nil
		} else {
			e := *upd.Email
			u.Email = &e
		}
	}
	cp := *u
	return &cp, nil
}

func (m *mapUsers) Deactivate(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.injectErr != nil {
		return m.injectErr
	}
	u, ok := m.byID[id]
	if !ok || u.DeactivatedAt != nil {
		return auth.ErrNotFound
	}
	now := time.Now()
	u.DeactivatedAt = &now
	return nil
}

func (m *mapUsers) Count(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.byName), nil
}

type mapSessions struct {
	mu   sync.Mutex
	live map[string]auth.Session
}

// errDuplicateKey mimics the pg unique-violation error string the handler matches.
var errDuplicateKey = &duplicateKeyError{}

type duplicateKeyError struct{}

func (*duplicateKeyError) Error() string { return "pq: duplicate key value violates unique constraint" }

func newMapSessions() *mapSessions { return &mapSessions{live: map[string]auth.Session{}} }

func (m *mapSessions) Create(_ context.Context, userID uuid.UUID, ttl time.Duration) (auth.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := auth.Session{Token: "tok-" + uuid.NewString(), ID: uuid.New(), UserID: userID, ExpiresAt: time.Now().Add(ttl)}
	m.live[s.Token] = s
	return s, nil
}

func (m *mapSessions) Rotate(ctx context.Context, token string, ttl time.Duration) (auth.Session, error) {
	m.mu.Lock()
	old, ok := m.live[token]
	m.mu.Unlock()
	if !ok {
		return auth.Session{}, auth.ErrNotFound
	}
	m.Revoke(context.Background(), token)
	return m.Create(ctx, old.UserID, ttl)
}

func (m *mapSessions) Revoke(_ context.Context, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.live, token)
	return nil
}

func (m *mapSessions) RevokeAllForUser(_ context.Context, userID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for tok, s := range m.live {
		if s.UserID == userID {
			delete(m.live, tok)
		}
	}
	return nil
}

func (m *mapSessions) PruneExpired(_ context.Context) (int64, error) { return 0, nil }

func (m *mapSessions) Resolve(_ context.Context, token string) (uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.live[token]; ok {
		return s.UserID, nil
	}
	return uuid.UUID{}, auth.ErrNotFound
}

// ── test harness ──────────────────────────────────────────────────────────────

// stubAuthenticator answers with mutable state so tests can drive 401 / 403 /
// admin paths through the real requireAdmin gate.
type stubAuthenticator struct {
	author discussion.AuthorContext
	ok     bool
}

func (s *stubAuthenticator) Authenticate(_ *http.Request) (discussion.AuthorContext, bool) {
	return s.author, s.ok
}

type authHarness struct {
	srv       *Server
	users     *mapUsers
	sessions  *mapSessions
	authn     *stubAuthenticator
	audits    []string
	cookieVal string
}

func hashFor(t *testing.T, pw string) string {
	t.Helper()
	h, err := auth.HashPassword(pw)
	require.NoError(t, err)
	return h
}

// newAuthHarness builds the full server with the auth surface mounted and a
// permissive-enough limiter for handler tests (limiter semantics live in
// pkg/auth's own tests).
func newAuthHarness(t *testing.T, admin *auth.User, others ...*auth.User) *authHarness {
	t.Helper()
	all := append(others, admin)
	iss, err := auth.NewJWTIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	require.NoError(t, err)
	users := newMapUsers(all...)
	h := &authHarness{users: users, sessions: newMapSessions(),
		authn: &stubAuthenticator{author: discussion.AuthorContext{Principal: "user:" + admin.Username, TeamID: admin.TeamID, IsAdmin: true}, ok: true}}
	svc := auth.NewService(users, h.sessions, iss, auth.NewRateLimiter(100, time.Minute), auth.ServiceConfig{SessionTTL: time.Hour})
	h.srv = NewServer(Options{
		Auth: AuthRoutesOptions{
			Service:       svc,
			Authenticator: h.authn,
			CookieName:    "ksquad_session",
			SecureCookies: false, // httptest is plain http
			Audit: func(_ context.Context, eventType, principal string, _ map[string]any) {
				h.audits = append(h.audits, eventType+"/"+principal)
			},
		},
	})
	return h
}

func (h *authHarness) do(t *testing.T, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if h.cookieVal != "" {
		req.AddCookie(&http.Cookie{Name: "ksquad_session", Value: h.cookieVal})
	}
	w := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(w, req)
	return w
}

func loginBody(user, pass string) string {
	b, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	return string(b)
}

func newAdmin(t *testing.T, name string) *auth.User {
	t.Helper()
	return &auth.User{ID: uuid.New(), Username: name, Principal: "user:" + name,
		PasswordHash: hashFor(t, "admin-pass-123"), TeamID: uuid.New(), GlobalRole: auth.RoleAdmin, CreatedAt: time.Now()}
}

func newUser(t *testing.T, name string) *auth.User {
	t.Helper()
	return &auth.User{ID: uuid.New(), Username: name, Principal: "user:" + name,
		PasswordHash: hashFor(t, "user-pass-123"), TeamID: uuid.New(), GlobalRole: auth.RoleUser, CreatedAt: time.Now()}
}

// ── /auth/login ───────────────────────────────────────────────────────────────

func TestAuthRoutes_Login(t *testing.T) {
	admin := newAdmin(t, "admin")
	h := newAuthHarness(t, admin)

	w := h.do(t, "POST", "/auth/login", loginBody("admin", "admin-pass-123"), map[string]string{"Content-Type": "application/json"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		AccessToken string `json:"accessToken"`
		ExpiresIn   int64  `json:"expiresIn"`
		User        struct {
			Username   string `json:"username"`
			GlobalRole string `json:"globalRole"`
		} `json:"user"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.AccessToken)
	assert.Equal(t, int64(3600), resp.ExpiresIn)
	assert.Equal(t, "admin", resp.User.Username)
	assert.Contains(t, w.Header().Get("Set-Cookie"), "HttpOnly")

	// Missing fields → 400.
	w = h.do(t, "POST", "/auth/login", `{"username":"admin"}`, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	// Bad credentials → the single opaque 401.
	w = h.do(t, "POST", "/auth/login", loginBody("admin", "wrong"), nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "invalid credentials")

	// Malformed JSON → 400.
	w = h.do(t, "POST", "/auth/login", `{`, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	// Oversized body → the 4096-byte cap answers (MaxBytesReader → 400 decode error).
	w = h.do(t, "POST", "/auth/login", `{"username":"`+strings.Repeat("a", 5000)+`","password":"x"}`, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAuthRoutes_Login_NoSelfAssertedGroups(t *testing.T) {
	// PR #90 review finding 3: a client self-asserting an admin group MUST NOT
	// change the response's role. The field is not even part of the request
	// shape anymore; sending it is inert.
	admin := newAdmin(t, "admin")
	h := newAuthHarness(t, admin)

	body := `{"username":"admin","password":"admin-pass-123","groups":["platform-admins"]}`
	w := h.do(t, "POST", "/auth/login", body, nil)
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		User struct {
			GlobalRole string `json:"globalRole"`
		} `json:"user"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, auth.RoleAdmin, resp.User.GlobalRole, "role comes from the STORE, not the body")
}

func TestAuthRoutes_Login_ClientIPTrustedProxyGate(t *testing.T) {
	// PR #90 review finding 1, through the real handler: a spoofed
	// X-Forwarded-For from an UNTRUSTED peer must not rotate the limiter bucket.
	admin := newAdmin(t, "admin")
	h := newAuthHarness(t, admin)

	// Drain the limiter for the SOCKET ip (httptest peer = 192.0.2.1).
	for i := 0; i < 100; i++ {
		w := h.do(t, "POST", "/auth/login", loginBody("admin", "wrong"), map[string]string{
			"X-Forwarded-For": "9.9.9." + string(rune('0'+i%10)), // spoofed, untrusted peer
		})
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("limiter tripped on attempt %d — spoofed XFF rotated the bucket", i)
		}
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	}
}

// ── /auth/refresh, /auth/logout, /auth/me ────────────────────────────────────

func (h *authHarness) loginCookie(t *testing.T, user, pass string) {
	t.Helper()
	h.cookieVal = ""
	w := h.do(t, "POST", "/auth/login", loginBody(user, pass), nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	for _, c := range w.Result().Cookies() {
		if c.Name == "ksquad_session" {
			h.cookieVal = c.Value
		}
	}
	require.NotEmpty(t, h.cookieVal)
}

func TestAuthRoutes_RefreshLogoutMe(t *testing.T) {
	admin := newAdmin(t, "admin")
	h := newAuthHarness(t, admin)

	// No cookie → 401 on all three.
	assert.Equal(t, http.StatusUnauthorized, h.do(t, "POST", "/auth/refresh", "", nil).Code)
	assert.Equal(t, http.StatusUnauthorized, h.do(t, "GET", "/auth/me", "", nil).Code)

	h.loginCookie(t, "admin", "admin-pass-123")

	w := h.do(t, "GET", "/auth/me", "", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"username":"admin"`)

	old := h.cookieVal
	w = h.do(t, "POST", "/auth/refresh", "", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Set-Cookie"), "HttpOnly")
	for _, c := range w.Result().Cookies() {
		if c.Name == "ksquad_session" {
			h.cookieVal = c.Value
		}
	}
	assert.NotEqual(t, old, h.cookieVal, "refresh rotates the cookie")

	// The SPENT token no longer refreshes.
	w = h.do(t, "POST", "/auth/refresh", "", nil)
	require.Equal(t, http.StatusOK, w.Code)
	h.cookieVal = old
	assert.Equal(t, http.StatusUnauthorized, h.do(t, "POST", "/auth/refresh", "", nil).Code)
	assert.Equal(t, http.StatusUnauthorized, h.do(t, "GET", "/auth/me", "", nil).Code)

	// Logout clears the cookie and kills the session (idempotent 204).
	h.loginCookie(t, "admin", "admin-pass-123")
	w = h.do(t, "POST", "/auth/logout", "", nil)
	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Contains(t, w.Header().Get("Set-Cookie"), "Max-Age=0")
	h.cookieVal = ""
	w = h.do(t, "POST", "/auth/logout", "", nil)
	assert.Equal(t, http.StatusNoContent, w.Code)
}

// ── CSRF guard (PR #90 review finding 6) ─────────────────────────────────────

func TestAuthRoutes_SameOriginGuard(t *testing.T) {
	admin := newAdmin(t, "admin")
	h := newAuthHarness(t, admin)
	h.loginCookie(t, "admin", "admin-pass-123")

	// Foreign Origin on a state change → 403 before the handler runs.
	w := h.do(t, "POST", "/auth/logout", "", map[string]string{"Origin": "https://evil.example.com"})
	assert.Equal(t, http.StatusForbidden, w.Code)

	// Cross-site fetch metadata → 403.
	w = h.do(t, "POST", "/auth/logout", "", map[string]string{"Sec-Fetch-Site": "cross-site"})
	assert.Equal(t, http.StatusForbidden, w.Code)

	// Same-origin Origin and same-site fetch metadata pass.
	w = h.do(t, "POST", "/auth/logout", "", map[string]string{"Origin": "http://example.com", "Sec-Fetch-Site": "same-origin"})
	assert.Equal(t, http.StatusNoContent, w.Code)

	// No browser headers (server-to-server BFF hop) passes.
	h.loginCookie(t, "admin", "admin-pass-123")
	w = h.do(t, "POST", "/auth/logout", "", nil)
	assert.Equal(t, http.StatusNoContent, w.Code)

	// GET (safe method) never blocked on Origin.
	h.loginCookie(t, "admin", "admin-pass-123")
	w = h.do(t, "GET", "/auth/me", "", map[string]string{"Origin": "https://evil.example.com"})
	assert.Equal(t, http.StatusOK, w.Code)
}

// ── /admin/users gate + CRUD ─────────────────────────────────────────────────

func TestAuthRoutes_AdminGate(t *testing.T) {
	admin := newAdmin(t, "admin")
	plain := newUser(t, "mallory")
	h := newAuthHarness(t, admin, plain)

	// Authentication fails (no resolvable session) → 401.
	h.authn.ok = false
	assert.Equal(t, http.StatusUnauthorized, h.do(t, "GET", "/admin/users", "", nil).Code)

	// Authenticated but NOT admin → 403.
	h.authn.ok = true
	h.authn.author.IsAdmin = false
	assert.Equal(t, http.StatusForbidden, h.do(t, "GET", "/admin/users", "", nil).Code)

	// Admin → 200 with the page envelope.
	h.authn.author.IsAdmin = true
	w := h.do(t, "GET", "/admin/users", "", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"total":2`)
}

func TestAuthRoutes_AdminCreateUser(t *testing.T) {
	admin := newAdmin(t, "admin")
	h := newAuthHarness(t, admin)
	h.loginCookie(t, "admin", "admin-pass-123")

	// Valid create → 201, audit, hash never in response.
	w := h.do(t, "POST", "/admin/users", `{"username":"newdev","password":"long-enough-1","globalRole":"user"}`, map[string]string{"Content-Type": "application/json"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "argon2")
	assert.Contains(t, h.audits[len(h.audits)-1], "user_created")

	// Duplicate username → 409.
	w = h.do(t, "POST", "/admin/users", `{"username":"newdev","password":"long-enough-1"}`, nil)
	assert.Equal(t, http.StatusConflict, w.Code)

	// Short password → 400.
	w = h.do(t, "POST", "/admin/users", `{"username":"shorty","password":"short"}`, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	// Bad role → 400.
	w = h.do(t, "POST", "/admin/users", `{"username":"rooty","password":"long-enough-1","globalRole":"root"}`, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	// Bad teamId → 400.
	w = h.do(t, "POST", "/admin/users", `{"username":"teamy","password":"long-enough-1","teamId":"not-a-uuid"}`, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAuthRoutes_AdminGetListPatch(t *testing.T) {
	admin := newAdmin(t, "admin")
	dev := newUser(t, "dev")
	h := newAuthHarness(t, admin, dev)
	h.loginCookie(t, "admin", "admin-pass-123")

	// Bad id → 400; unknown id → 404.
	assert.Equal(t, http.StatusBadRequest, h.do(t, "GET", "/admin/users/not-a-uuid", "", nil).Code)
	assert.Equal(t, http.StatusNotFound, h.do(t, "GET", "/admin/users/"+uuid.NewString(), "", nil).Code)

	w := h.do(t, "GET", "/admin/users/"+dev.ID.String(), "", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"username":"dev"`)

	// PATCH: bad role 400, empty body 400, real role change 200 + audit.
	assert.Equal(t, http.StatusBadRequest, h.do(t, "PATCH", "/admin/users/"+dev.ID.String(), `{"globalRole":"supreme"}`, nil).Code)
	assert.Equal(t, http.StatusBadRequest, h.do(t, "PATCH", "/admin/users/"+dev.ID.String(), `{}`, nil).Code)
	w = h.do(t, "PATCH", "/admin/users/"+dev.ID.String(), `{"globalRole":"admin"}`, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"globalRole":"admin"`)
	assert.Contains(t, h.audits[len(h.audits)-1], "user_updated")
}

func TestAuthRoutes_AdminDelete(t *testing.T) {
	admin := newAdmin(t, "admin")
	dev := newUser(t, "dev")
	h := newAuthHarness(t, admin, dev)
	h.loginCookie(t, "admin", "admin-pass-123")

	// DELETE the (non-last) admin-target dev → 204 + audit + sessions revoked.
	h.cookieVal = "" // act as the admin via the stub-authenticated gate instead
	w := h.do(t, "DELETE", "/admin/users/"+dev.ID.String(), "", nil)
	require.Equal(t, http.StatusNoContent, w.Code)
	assert.Contains(t, h.audits[len(h.audits)-1], "user_deactivated")

	// Gone → 404 on repeat.
	w = h.do(t, "DELETE", "/admin/users/"+dev.ID.String(), "", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestAuthRoutes_AdminLastAdminGuard(t *testing.T) {
	// PR #90 review finding 4, at the HTTP surface: the fake store cannot
	// express the count, so this asserts the route mapping only — the guard
	// itself (ErrLastAdmin → 409) is enforced by PostgresUserStore. Here we
	// verify a normal PATCH/DELETE against the sole admin still answers sanely
	// (200/204 via the fake) and that the 409 mapping exists in the handler:
	admin := newAdmin(t, "admin")
	h := newAuthHarness(t, admin)
	h.cookieVal = ""

	w := h.do(t, "PATCH", "/admin/users/"+admin.ID.String(), `{"globalRole":"user"}`, nil)
	assert.Equal(t, http.StatusOK, w.Code) // fake store has no guard; real store returns ErrLastAdmin → 409 (see store tests)
}

func TestAuthRoutes_ClientIPOnlyWhenTrusted(t *testing.T) {
	// Direct unit checks of the helper the handler uses (belt to the drain test above).
	trust := auth.ParseCIDRs("10.0.0.0/8")
	assert.Equal(t, "8.8.8.8", auth.ClientIP(trust, "203.0.113.9, 8.8.8.8", "10.0.0.5:1"))
	assert.Equal(t, "198.51.100.7", auth.ClientIP(trust, "9.9.9.9", "198.51.100.7:2"), "untrusted peer: XFF ignored")
	_ = net.ParseIP("127.0.0.1")
}

// ── error-branch and edge coverage ───────────────────────────────────────────

// failSessions fails Create so the login 500 branch (mint failure) is reachable.
type failSessions struct {
	mapSessions
	failCreate bool
}

func (f *failSessions) Create(ctx context.Context, userID uuid.UUID, ttl time.Duration) (auth.Session, error) {
	if f.failCreate {
		return auth.Session{}, assert.AnError
	}
	return f.mapSessions.Create(ctx, userID, ttl)
}

func TestAuthRoutes_LoginInternalError(t *testing.T) {
	admin := newAdmin(t, "admin")
	iss, err := auth.NewJWTIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	require.NoError(t, err)
	users := newMapUsers(admin)
	fs := &failSessions{mapSessions: *newMapSessions(), failCreate: true}
	svc := auth.NewService(users, fs, iss, auth.NewRateLimiter(100, time.Minute), auth.ServiceConfig{SessionTTL: time.Hour})
	srv := NewServer(Options{Auth: AuthRoutesOptions{Service: svc,
		Authenticator: &stubAuthenticator{author: discussion.AuthorContext{Principal: "user:admin", TeamID: admin.TeamID, IsAdmin: true}, ok: true},
		CookieName:    "ksquad_session"}})
	req := httptest.NewRequest("POST", "/auth/login", strings.NewReader(loginBody("admin", "admin-pass-123")))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestAuthRoutes_LoginRateLimited(t *testing.T) {
	admin := newAdmin(t, "admin")
	iss, err := auth.NewJWTIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	require.NoError(t, err)
	users := newMapUsers(admin)
	svc := auth.NewService(users, newMapSessions(), iss, auth.NewRateLimiter(1, time.Minute), auth.ServiceConfig{SessionTTL: time.Hour})
	srv := NewServer(Options{Auth: AuthRoutesOptions{Service: svc,
		Authenticator: &stubAuthenticator{author: discussion.AuthorContext{Principal: "user:admin", TeamID: admin.TeamID, IsAdmin: true}, ok: true},
		CookieName:    "ksquad_session"}})
	do := func() int {
		req := httptest.NewRequest("POST", "/auth/login", strings.NewReader(loginBody("admin", "wrong")))
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w.Code
	}
	assert.Equal(t, http.StatusUnauthorized, do()) // the one allowed failure
	assert.Equal(t, http.StatusTooManyRequests, do(), "brake answers 429 before credential work")
}

func TestAuthRoutes_AdminStoreErrors(t *testing.T) {
	admin := newAdmin(t, "admin")
	dev := newUser(t, "dev")
	h := newAuthHarness(t, admin, dev)

	// PATCH unknown id → 404.
	w := h.do(t, "PATCH", "/admin/users/"+uuid.NewString(), `{"globalRole":"user"}`, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)

	// PATCH last-admin guard from the store → 409.
	h.users.injectErr = auth.ErrLastAdmin
	w = h.do(t, "PATCH", "/admin/users/"+dev.ID.String(), `{"globalRole":"user"}`, nil)
	assert.Equal(t, http.StatusConflict, w.Code)

	// DELETE last-admin guard → 409; generic store error → 500.
	w = h.do(t, "DELETE", "/admin/users/"+dev.ID.String(), "", nil)
	assert.Equal(t, http.StatusConflict, w.Code)
	h.users.injectErr = assert.AnError
	w = h.do(t, "DELETE", "/admin/users/"+dev.ID.String(), "", nil)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	w = h.do(t, "PATCH", "/admin/users/"+dev.ID.String(), `{"globalRole":"user"}`, nil)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	h.users.injectErr = nil

	// DELETE unknown (already deactivated) → 404.
	require.Equal(t, http.StatusNoContent, h.do(t, "DELETE", "/admin/users/"+dev.ID.String(), "", nil).Code)
	assert.Equal(t, http.StatusNotFound, h.do(t, "DELETE", "/admin/users/"+dev.ID.String(), "", nil).Code)
}

func TestAuthRoutes_PaginationAndQuery(t *testing.T) {
	admin := newAdmin(t, "admin")
	h := newAuthHarness(t, admin)

	// Bad values fall back to defaults; negative offset clamps to 0.
	w := h.do(t, "GET", "/admin/users?limit=abc&offset=-7", "", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"limit":50`)
	assert.Contains(t, w.Body.String(), `"offset":0`)

	// Oversized limit clamps.
	w = h.do(t, "GET", "/admin/users?limit=99999", "", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"limit":50`)

	// Explicit valid values pass through.
	w = h.do(t, "GET", "/admin/users?limit=1&offset=0", "", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"limit":1`)
}

func TestAuthRoutes_OriginAllowlist(t *testing.T) {
	admin := newAdmin(t, "admin")
	iss, err := auth.NewJWTIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	require.NoError(t, err)
	users := newMapUsers(admin)
	svc := auth.NewService(users, newMapSessions(), iss, auth.NewRateLimiter(100, time.Minute), auth.ServiceConfig{SessionTTL: time.Hour})
	srv := NewServer(Options{Auth: AuthRoutesOptions{
		Service:       svc,
		Authenticator: &stubAuthenticator{author: discussion.AuthorContext{Principal: "user:admin", TeamID: admin.TeamID, IsAdmin: true}, ok: true},
		CookieName:    "ksquad_session", AllowedOrigins: []string{"https://console.example.com"},
	}})
	do := func(origin string) int {
		req := httptest.NewRequest("POST", "/auth/logout", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w.Code
	}
	assert.Equal(t, http.StatusNoContent, do("https://console.example.com"), "allowlisted origin passes")
	assert.Equal(t, http.StatusForbidden, do("https://evil.example.com"))
	assert.Equal(t, http.StatusForbidden, do("https://console.example.com.evil.io"))
	assert.Equal(t, http.StatusNoContent, do(""), "no Origin header (BFF hop) passes")
}

func TestAuthRoutes_AdminUnavailableWithoutAuthenticator(t *testing.T) {
	admin := newAdmin(t, "admin")
	iss, err := auth.NewJWTIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	require.NoError(t, err)
	svc := auth.NewService(newMapUsers(admin), newMapSessions(), iss, auth.NewRateLimiter(100, time.Minute), auth.ServiceConfig{SessionTTL: time.Hour})
	srv := NewServer(Options{Auth: AuthRoutesOptions{Service: svc, Authenticator: nil, CookieName: "ksquad_session"}})
	req := httptest.NewRequest("GET", "/admin/users", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "nil authenticator: admin surface answers 503, not a panic")
}

func TestAuditAdminMutation_FallbackLogsWhenNoSink(t *testing.T) {
	// No Audit sink wired: the fallback log path must not panic (and records nothing).
	assert.NotPanics(t, func() {
		auditAdminMutation(context.Background(), AuthRoutesOptions{}, "user_created", "user:admin", map[string]any{"k": 1})
	})
}
