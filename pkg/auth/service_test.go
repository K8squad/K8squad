package auth

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Service tests over fakes: the JWT issuer and rate limiter are the REAL
// in-memory implementations (they have no dependencies); the stores are small
// map-backed fakes. Nothing here touches Postgres.
// ============================================================================

// fakeUsers is a map-backed UserStore.
type fakeUsers struct {
	mu    sync.Mutex
	byID  map[uuid.UUID]*User
	users map[string]*User // username → user
}

func newFakeUsers(users ...*User) *fakeUsers {
	f := &fakeUsers{byID: map[uuid.UUID]*User{}, users: map[string]*User{}}
	for _, u := range users {
		f.byID[u.ID] = u
		f.users[u.Username] = u
	}
	return f
}

func (f *fakeUsers) ByUsername(_ context.Context, username string) (*User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, ok := f.users[username]; ok {
		cp := *u
		return &cp, nil
	}
	return nil, ErrNotFound
}

func (f *fakeUsers) ByID(_ context.Context, id uuid.UUID) (*User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, ok := f.byID[id]; ok {
		cp := *u
		return &cp, nil
	}
	return nil, ErrNotFound
}

func (f *fakeUsers) Create(_ context.Context, u *User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u.ID = uuid.New()
	u.Principal = "user:" + u.Username
	u.CreatedAt = time.Now()
	f.byID[u.ID] = u
	f.users[u.Username] = u
	return nil
}

func (f *fakeUsers) List(_ context.Context, limit, offset int) ([]*User, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*User, 0, len(f.users))
	for _, u := range f.users {
		out = append(out, u)
	}
	if offset < len(out) {
		out = out[offset:]
	} else {
		out = nil
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, len(f.users), nil
}

func (f *fakeUsers) Update(_ context.Context, id uuid.UUID, upd UserUpdate) (*User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.byID[id]
	if !ok {
		return nil, ErrNotFound
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

func (f *fakeUsers) Deactivate(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.byID[id]
	if !ok || u.DeactivatedAt != nil {
		return ErrNotFound
	}
	now := time.Now()
	u.DeactivatedAt = &now
	return nil
}

func (f *fakeUsers) Count(_ context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.users), nil
}

// fakeSessions is a map-backed SessionStore mirroring the Postgres semantics
// (only sha256-keyed live rows resolve; rotate revokes old + mints new).
type fakeSessions struct {
	mu       sync.Mutex
	live     map[string]Session // token → session (live only)
	revoked  int
	minted   int
	lastTTLs []time.Duration
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{live: map[string]Session{}}
}

func (f *fakeSessions) Create(_ context.Context, userID uuid.UUID, ttl time.Duration) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.minted++
	f.lastTTLs = append(f.lastTTLs, ttl)
	tok := "tok-" + uuid.NewString()
	sess := Session{Token: tok, ID: uuid.New(), UserID: userID, ExpiresAt: time.Now().Add(ttl)}
	f.live[tok] = sess
	return sess, nil
}

func (f *fakeSessions) Rotate(ctx context.Context, token string, ttl time.Duration) (Session, error) {
	old, ok := f.live[token]
	if !ok {
		return Session{}, ErrNotFound
	}
	delete(f.live, token)
	f.revoked++
	return f.Create(ctx, old.UserID, ttl)
}

func (f *fakeSessions) Revoke(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.live[token]; ok {
		delete(f.live, token)
		f.revoked++
	}
	return nil // idempotent, like the Postgres store
}

func (f *fakeSessions) RevokeAllForUser(_ context.Context, userID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for tok, s := range f.live {
		if s.UserID == userID {
			delete(f.live, tok)
			f.revoked++
		}
	}
	return nil
}

func (f *fakeSessions) PruneExpired(_ context.Context) (int64, error) { return 0, nil }

func (f *fakeSessions) Resolve(_ context.Context, token string) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.live[token]; ok {
		return s.UserID, nil
	}
	return uuid.UUID{}, ErrNotFound
}

// newTestService assembles a Service over fakes: real issuer (32-byte test key),
// real limiter at limit/window, and the given users.
func newTestService(t *testing.T, limit int, users ...*User) (*Service, *fakeUsers, *fakeSessions, *JWTIssuer) {
	t.Helper()
	iss, err := NewJWTIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	require.NoError(t, err)
	fu := newFakeUsers(users...)
	fs := newFakeSessions()
	lim := NewRateLimiter(limit, time.Minute)
	return NewService(fu, fs, iss, lim, ServiceConfig{SessionTTL: time.Hour}), fu, fs, iss
}

func testUser(t *testing.T, username, password string) *User {
	t.Helper()
	hash, err := HashPassword(password)
	require.NoError(t, err)
	return &User{ID: uuid.New(), Username: username, Principal: "user:" + username, PasswordHash: hash, TeamID: uuid.New(), GlobalRole: RoleUser, CreatedAt: time.Now()}
}

func TestService_Login_Success(t *testing.T) {
	u := testUser(t, "amelia", "correct-horse")
	svc, _, fs, iss := newTestService(t, 5, u)

	res, err := svc.Login(context.Background(), "amelia", "correct-horse", "10.0.0.1")
	require.NoError(t, err)
	assert.NotEmpty(t, res.SessionToken)
	assert.Equal(t, int64(3600), res.ExpiresIn)
	assert.Equal(t, "amelia", res.User.Username)

	claims, err := iss.Verify(res.AccessToken)
	require.NoError(t, err)
	assert.Equal(t, "user:amelia", claims.Subject)
	assert.Equal(t, u.ID.String(), claims.UserID)
	assert.NotEmpty(t, claims.SessionID, "JWT binds to the minted session")

	_, err = fs.Resolve(context.Background(), res.SessionToken)
	assert.NoError(t, err, "session token resolves live")
}

func TestService_Login_OpaqueFailures(t *testing.T) {
	u := testUser(t, "amelia", "correct-horse")
	deactivated := testUser(t, "ghost", "correct-horse")
	now := time.Now()
	deactivated.DeactivatedAt = &now
	svc, _, _, _ := newTestService(t, 10, u, deactivated)

	for name, args := range map[string]struct {
		user, pass, ip string
	}{
		"unknown user":   {"nobody", "whatever", "10.0.0.1"},
		"wrong password": {"amelia", "wrong", "10.0.0.1"},
		"deactivated":    {"ghost", "correct-horse", "10.0.0.1"},
	} {
		res, err := svc.Login(context.Background(), args.user, args.pass, args.ip)
		assert.ErrorIs(t, err, ErrInvalidCredentials, name)
		assert.Nil(t, res, name)
	}
}

func TestService_Login_FailuresConsumeLimiter_SuccessResets(t *testing.T) {
	u := testUser(t, "amelia", "correct-horse")
	svc, _, _, _ := newTestService(t, 3, u) // 3 failures per window
	ip := "10.0.0.9"

	// Two failures, then an authentic login: the window must CLEAR (finding 5),
	// so two more failures still fit inside the limit of 3.
	for i := 0; i < 2; i++ {
		_, err := svc.Login(context.Background(), "amelia", "wrong", ip)
		assert.ErrorIs(t, err, ErrInvalidCredentials)
	}
	_, err := svc.Login(context.Background(), "amelia", "correct-horse", ip)
	require.NoError(t, err, "successful login within budget")
	for i := 0; i < 2; i++ {
		_, err := svc.Login(context.Background(), "amelia", "wrong", ip)
		assert.ErrorIs(t, err, ErrInvalidCredentials, "window was reset by the success")
	}

	// Third failure trips the brake; the answer is ErrRateLimited BEFORE any
	// credential work (no user enumeration on a throttled IP).
	_, err = svc.Login(context.Background(), "amelia", "wrong", ip)
	assert.ErrorIs(t, err, ErrInvalidCredentials)
	_, err = svc.Login(context.Background(), "amelia", "wrong", ip)
	assert.ErrorIs(t, err, ErrRateLimited)
	_, err = svc.Login(context.Background(), "totally-unknown-user", "x", ip)
	assert.ErrorIs(t, err, ErrRateLimited, "throttled IP is throttled for unknown users too")

	// Other IPs are independent buckets.
	_, err = svc.Login(context.Background(), "amelia", "correct-horse", "10.0.0.10")
	assert.NoError(t, err)
}

func TestService_Refresh_RotatesAndFailsClosed(t *testing.T) {
	u := testUser(t, "amelia", "correct-horse")
	svc, _, fs, _ := newTestService(t, 5, u)

	login, err := svc.Login(context.Background(), "amelia", "correct-horse", "10.0.0.1")
	require.NoError(t, err)

	refreshed, err := svc.Refresh(context.Background(), login.SessionToken)
	require.NoError(t, err)
	assert.NotEqual(t, login.SessionToken, refreshed.SessionToken, "rotation mints a new token")

	_, err = fs.Resolve(context.Background(), login.SessionToken)
	assert.Error(t, err, "old token died in the rotation")

	// Rotating again with the SPENT token fails closed.
	_, err = svc.Refresh(context.Background(), login.SessionToken)
	assert.ErrorIs(t, err, ErrSessionExpired)

	// A user deactivated mid-session: refresh kills the fresh session too.
	now := time.Now()
	require.NoError(t, svc.Users.Deactivate(context.Background(), u.ID))
	_ = now
	_, err = svc.Refresh(context.Background(), refreshed.SessionToken)
	assert.ErrorIs(t, err, ErrSessionExpired)
	_, err = fs.Resolve(context.Background(), refreshed.SessionToken)
	assert.Error(t, err, "mid-session deactivation revokes the rotated session")
}

func TestService_Logout_Idempotent(t *testing.T) {
	u := testUser(t, "amelia", "correct-horse")
	svc, _, fs, _ := newTestService(t, 5, u)
	login, err := svc.Login(context.Background(), "amelia", "correct-horse", "10.0.0.1")
	require.NoError(t, err)

	require.NoError(t, svc.Logout(context.Background(), login.SessionToken))
	_, err = fs.Resolve(context.Background(), login.SessionToken)
	assert.Error(t, err)

	assert.NoError(t, svc.Logout(context.Background(), login.SessionToken), "stale cookie logs out cleanly")
	assert.NoError(t, svc.Logout(context.Background(), "never-existed"), "unknown token is a no-op")
}

func TestService_Me_FailClosed(t *testing.T) {
	u := testUser(t, "amelia", "correct-horse")
	svc, _, _, _ := newTestService(t, 5, u)
	login, err := svc.Login(context.Background(), "amelia", "correct-horse", "10.0.0.1")
	require.NoError(t, err)

	got, err := svc.Me(context.Background(), login.SessionToken)
	require.NoError(t, err)
	assert.Equal(t, "amelia", got.Username)

	_, err = svc.Me(context.Background(), "garbage-token")
	assert.ErrorIs(t, err, ErrSessionExpired)

	require.NoError(t, svc.Logout(context.Background(), login.SessionToken))
	_, err = svc.Me(context.Background(), login.SessionToken)
	assert.ErrorIs(t, err, ErrSessionExpired, "revoked session resolves to nothing")
}

func TestService_SessionTTL_Defaults(t *testing.T) {
	assert.Equal(t, 24*time.Hour, NewService(nil, nil, nil, nil, ServiceConfig{}).SessionTTL())
	assert.Equal(t, 12*time.Hour, NewService(nil, nil, nil, nil, ServiceConfig{SessionTTL: 12 * time.Hour}).SessionTTL())
	assert.Equal(t, 24*time.Hour, NewService(nil, nil, nil, nil, ServiceConfig{SessionTTL: -time.Hour}).SessionTTL())
}
