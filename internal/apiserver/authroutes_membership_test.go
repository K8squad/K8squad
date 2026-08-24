package apiserver

import (
	"context"
	"encoding/json"
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
// /admin/users/{id}/memberships handler tests (8.15 per-Project roles, ISI-2911).
//
// The admin Users & Roles screen reads and edits auth.project_membership grants.
// These tests drive the routes through the SERVER ROUTER so the real requireAdmin
// gate + same-origin guard run, over a map-backed membership fake (no Postgres) —
// the same style as authroutes_test.go.
// ============================================================================

// mapMemberships is a map-backed auth.MembershipStore fake keyed by (userID, project).
type mapMemberships struct {
	mu        sync.Mutex
	byUser    map[uuid.UUID]map[string]string // userID -> project -> role
	injectErr error
}

func newMapMemberships() *mapMemberships {
	return &mapMemberships{byUser: map[uuid.UUID]map[string]string{}}
}

func (m *mapMemberships) RoleForPrincipal(_ context.Context, _, _ string) (string, error) {
	return "", auth.ErrNoMembership // unused on the admin surface
}

func (m *mapMemberships) ListForUser(_ context.Context, userID uuid.UUID) ([]auth.ProjectMembership, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.injectErr != nil {
		return nil, m.injectErr
	}
	var out []auth.ProjectMembership
	for project, role := range m.byUser[userID] {
		out = append(out, auth.ProjectMembership{Project: project, Role: role})
	}
	return out, nil
}

func (m *mapMemberships) Grant(_ context.Context, userID uuid.UUID, project, role, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.injectErr != nil {
		return m.injectErr
	}
	if m.byUser[userID] == nil {
		m.byUser[userID] = map[string]string{}
	}
	m.byUser[userID][project] = role
	return nil
}

func (m *mapMemberships) Revoke(_ context.Context, userID uuid.UUID, project string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.injectErr != nil {
		return m.injectErr
	}
	delete(m.byUser[userID], project)
	return nil
}

// membershipHarness builds a server with the admin surface + a membership store wired. isAdmin
// toggles the acting caller's global role so the requireAdmin gate can be exercised. A nil store
// exercises the documented-501 fallback.
type membershipHarness struct {
	srv   *Server
	store *mapMemberships
}

func newMembershipHarness(t *testing.T, isAdmin bool, store auth.MembershipStore, users ...*auth.User) *membershipHarness {
	t.Helper()
	iss, err := auth.NewJWTIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	require.NoError(t, err)
	mu := newMapUsers(users...)
	svc := auth.NewService(mu, newMapSessions(), iss, auth.NewRateLimiter(100, time.Minute), auth.ServiceConfig{SessionTTL: time.Hour})
	authn := &stubAuthenticator{
		author: discussion.AuthorContext{Principal: "user:admin", TeamID: uuid.New(), IsAdmin: isAdmin},
		ok:     true,
	}
	srv := NewServer(Options{
		Auth: AuthRoutesOptions{
			Service:       svc,
			Authenticator: authn,
			CookieName:    "ksquad_session",
			SecureCookies: false,
			Memberships:   store,
		},
	})
	h := &membershipHarness{srv: srv}
	if s, ok := store.(*mapMemberships); ok {
		h.store = s
	}
	return h
}

func (h *membershipHarness) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(&http.Cookie{Name: "ksquad_session", Value: "sess"})
	w := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(w, req)
	return w
}

func TestAdminMemberships_GrantListRevoke(t *testing.T) {
	u := newUser(t, "alice")
	h := newMembershipHarness(t, true, newMapMemberships(), u)
	base := "/admin/users/" + u.ID.String() + "/memberships"

	// Empty to start.
	w := h.do(t, "GET", base, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var list struct {
		UserID string                   `json:"userId"`
		Items  []auth.ProjectMembership `json:"items"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	assert.Equal(t, u.ID.String(), list.UserID)
	assert.Empty(t, list.Items)

	// Grant maintainer on "acme".
	w = h.do(t, "PUT", base, `{"project":"acme","role":"maintainer"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var grant auth.ProjectMembership
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &grant))
	assert.Equal(t, "acme", grant.Project)
	assert.Equal(t, "maintainer", grant.Role)

	// Now it lists.
	w = h.do(t, "GET", base, "")
	require.Equal(t, http.StatusOK, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	require.Len(t, list.Items, 1)
	assert.Equal(t, "acme", list.Items[0].Project)

	// Revoke it (idempotent: 204).
	w = h.do(t, "DELETE", base+"?project=acme", "")
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())

	w = h.do(t, "GET", base, "")
	require.Equal(t, http.StatusOK, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	assert.Empty(t, list.Items)
}

func TestAdminMemberships_Validation(t *testing.T) {
	u := newUser(t, "bob")
	h := newMembershipHarness(t, true, newMapMemberships(), u)
	base := "/admin/users/" + u.ID.String() + "/memberships"

	// Bad role -> 400.
	w := h.do(t, "PUT", base, `{"project":"acme","role":"superuser"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	// Missing project -> 400.
	w = h.do(t, "PUT", base, `{"role":"viewer"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	// Revoke without ?project -> 400.
	w = h.do(t, "DELETE", base, "")
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAdminMemberships_UnknownUser404(t *testing.T) {
	h := newMembershipHarness(t, true, newMapMemberships()) // no users
	base := "/admin/users/" + uuid.New().String() + "/memberships"

	assert.Equal(t, http.StatusNotFound, h.do(t, "GET", base, "").Code)
	assert.Equal(t, http.StatusNotFound, h.do(t, "PUT", base, `{"project":"acme","role":"viewer"}`).Code)
}

func TestAdminMemberships_NonAdminForbidden(t *testing.T) {
	u := newUser(t, "carol")
	h := newMembershipHarness(t, false, newMapMemberships(), u) // caller is not admin
	base := "/admin/users/" + u.ID.String() + "/memberships"

	assert.Equal(t, http.StatusForbidden, h.do(t, "GET", base, "").Code)
}

func TestAdminMemberships_NoStore501(t *testing.T) {
	u := newUser(t, "dave")
	h := newMembershipHarness(t, true, nil, u) // Memberships store not wired
	base := "/admin/users/" + u.ID.String() + "/memberships"

	assert.Equal(t, http.StatusNotImplemented, h.do(t, "GET", base, "").Code)
}
