package apiserver

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
)

// TestCookieAuthenticatorExtractsSession — a valid ksquad_session cookie resolves to the
// AuthorContext; a missing/empty cookie or a nil resolver fails closed.
func TestCookieAuthenticatorExtractsSession(t *testing.T) {
	team := uuid.New()
	a := NewCookieAuthenticator(&StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		"tok": {Principal: "user:bob", TeamID: team},
	}})

	// valid cookie
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "tok"})
	got, ok := a.Authenticate(req)
	if !ok || got.Principal != "user:bob" || got.TeamID != team {
		t.Fatalf("valid cookie: ok=%v principal=%q team=%v", ok, got.Principal, got.TeamID)
	}

	// missing cookie
	if _, ok := a.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil)); ok {
		t.Errorf("missing cookie: want ok=false")
	}

	// empty value
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: ""})
	if _, ok := a.Authenticate(req); ok {
		t.Errorf("empty cookie: want ok=false")
	}

	// nil resolver fails closed
	if _, ok := (&CookieAuthenticator{}).Authenticate(req); ok {
		t.Errorf("nil resolver: want ok=false")
	}
}

// TestDeniedResolver — the production default denies every token (deny-by-default).
func TestDeniedResolver(t *testing.T) {
	if _, err := DeniedResolver().Resolve(t.Context(), "anything"); err == nil {
		t.Fatalf("DeniedResolver.Resolve: want error, got nil")
	}
}

// TestLoadStaticSessions — round-trips a dev sessions file and rejects malformed rows.
func TestLoadStaticSessions(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "sessions.json")
	team := uuid.NewString()
	if err := os.WriteFile(good, []byte(`[{"token":"t1","principal":"user:carol","teamId":"`+team+`"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := LoadStaticSessions(good)
	if err != nil {
		t.Fatalf("load good: %v", err)
	}
	auth, rerr := r.Resolve(t.Context(), "t1")
	if rerr != nil || auth.Principal != "user:carol" {
		t.Fatalf("resolve t1: err=%v principal=%q", rerr, auth.Principal)
	}

	// missing teamId is rejected
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`[{"token":"t","principal":"p"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadStaticSessions(bad); err == nil {
		t.Errorf("load bad: want error for missing teamId")
	}
}
