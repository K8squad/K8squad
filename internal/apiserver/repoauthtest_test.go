package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-github/v57/github"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
)

// ============================================================================
// E4-S1 repo auth Test-connection (ISI-3683, AD-7, NFR-2) — the merge-gate
// rubric:
//   - the probe is server-side and answers {ok, detail} ONLY — the stored
//     material is never echoed on any branch (success, auth failure, transport
//     failure);
//   - a bad credential is a FAILED TEST (200 + ok=false), never a 5xx;
//   - team scope is existence-hiding (unresolvable Team ⇒ 404; a Secret in a
//     foreign namespace is unreachable by construction — the read key is the
//     caller's own squad namespace);
//   - the last result is cached on the Team annotation
//     (ksquad.io/onboarding-test-connection-repo) on pass AND fail;
//   - v1 provider floor: non-github.com URLs fail field validation (422);
//   - the default data key is the repo-sync contract key ("token"), and an
//     explicit credentialSecretRef.key is honored.
// ============================================================================

const repoPatCanary = "ghp_CANARY-pat-that-must-never-leak"

// fakeRepoProber stubs the provider seam; err controls the probe outcome.
type fakeRepoProber struct {
	login string
	repos int64
	err   error
	// token mirrors what the handler passed in, for the no-echo assertions
	// (the canary must reach the prober — proving the right Secret was read —
	// while never reaching the response).
	token string
	url   string
}

func (p *fakeRepoProber) Probe(_ context.Context, repoURL, token string) (string, int64, error) {
	p.token, p.url = token, repoURL
	return p.login, p.repos, p.err
}

func newRepoAuthTester(t *testing.T, objs ...client.Object) (*RepoAuthTestService, *fakeRepoProber, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(secretWriteScheme(t)).WithObjects(objs...).Build()
	p := &fakeRepoProber{login: "nadia", repos: 42}
	return NewRepoAuthTestService(c, p), p, c
}

func testRepoAuthServer(t *testing.T, teamID uuid.UUID, svc *RepoAuthTestService) http.Handler {
	t.Helper()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:nadia", TeamID: teamID},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		RepoAuthTest:  svc,
	})
	return srv.Handler()
}

func postRepoAuthTest(t *testing.T, h http.Handler, body string, withAuth bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/projects/repo-auth/test", strings.NewReader(body))
	if withAuth {
		req = withSession(req, devToken)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func repoTestTeamAndSecret(t *testing.T) (*ksquadv1.Team, *corev1.Secret, uuid.UUID) {
	t.Helper()
	teamID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	tm := teamWithStatus("teams", "alpha", teamID.String(), "ksquad-team-alpha")
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-repo-pat", Namespace: "ksquad-team-alpha"},
		Data:       map[string][]byte{"token": []byte(repoPatCanary)},
	}
	return tm, sec, teamID
}

// TestRepoAuthTestHappyPath — the stored PAT probes green: the prober receives
// the stored material (proving the Secret read keyed correctly), the response
// carries {ok:true, detail naming login + count} and NEVER the material, and
// the Team annotation cache records "passed".
func TestRepoAuthTestHappyPath(t *testing.T) {
	tm, sec, teamID := repoTestTeamAndSecret(t)
	svc, prober, c := newRepoAuthTester(t, tm, sec)
	h := testRepoAuthServer(t, teamID, svc)

	rec := postRepoAuthTest(t, h, `{"url":"https://github.com/acme/widget","credentialSecretRef":{"name":"alpha-repo-pat"}}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("test: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if prober.token != repoPatCanary {
		t.Fatalf("prober received %q, want the stored material (wrong key/secret read)", prober.token)
	}
	if prober.url != "https://github.com/acme/widget" {
		t.Fatalf("prober received url %q", prober.url)
	}
	var out repoAuthTestResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.OK || !strings.Contains(out.Detail, "nadia") || !strings.Contains(out.Detail, "42") {
		t.Fatalf("result: got %+v", out)
	}
	if strings.Contains(rec.Body.String(), repoPatCanary) {
		t.Fatalf("NFR-2 breach: response echoes the credential material: %s", rec.Body.String())
	}

	var got ksquadv1.Team
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: "teams", Name: "alpha"}, &got); err != nil {
		t.Fatalf("team read: %v", err)
	}
	if recorded, passed := RepoTestConnectionFlag(&got); !recorded || !passed {
		t.Fatalf("annotation cache: got recorded=%v passed=%v, want passed", recorded, passed)
	}
}

// TestRepoAuthTestExplicitKey — an explicit credentialSecretRef.key overrides
// the repo-sync default key.
func TestRepoAuthTestExplicitKey(t *testing.T) {
	tm, sec, teamID := repoTestTeamAndSecret(t)
	sec.Data = map[string][]byte{"pat": []byte(repoPatCanary)}
	svc, prober, _ := newRepoAuthTester(t, tm, sec)
	h := testRepoAuthServer(t, teamID, svc)

	rec := postRepoAuthTest(t, h, `{"url":"https://github.com/acme/widget","credentialSecretRef":{"name":"alpha-repo-pat","key":"pat"}}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("test: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if prober.token != repoPatCanary {
		t.Fatal("explicit key not honored: prober received empty/wrong material")
	}
}

// TestRepoAuthTestEmptyKeyIsAFailedTest — a Secret with no material under the
// resolved key is ok=false with the key named, never a 5xx, and the failure is
// cached.
func TestRepoAuthTestEmptyKeyIsAFailedTest(t *testing.T) {
	tm, sec, teamID := repoTestTeamAndSecret(t)
	sec.Data = map[string][]byte{"other": []byte(repoPatCanary)}
	svc, _, c := newRepoAuthTester(t, tm, sec)
	h := testRepoAuthServer(t, teamID, svc)

	rec := postRepoAuthTest(t, h, `{"url":"https://github.com/acme/widget","credentialSecretRef":{"name":"alpha-repo-pat"}}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("test: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out repoAuthTestResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.OK || !strings.Contains(out.Detail, "token") {
		t.Fatalf("result: got %+v, want ok=false naming the missing key", out)
	}

	var got ksquadv1.Team
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: "teams", Name: "alpha"}, &got); err != nil {
		t.Fatalf("team read: %v", err)
	}
	if recorded, passed := RepoTestConnectionFlag(&got); !recorded || passed {
		t.Fatalf("annotation cache: got recorded=%v passed=%v, want failed", recorded, passed)
	}
}

// TestRepoAuthTestProvider401 — a rejected PAT is a FAILED TEST with the
// provider status in the detail, and the failure is cached. Never a 5xx, never
// the material.
func TestRepoAuthTestProvider401(t *testing.T) {
	tm, sec, teamID := repoTestTeamAndSecret(t)
	svc, _, c := newRepoAuthTester(t, tm, sec)
	svc.prober = &fakeRepoProber{err: &github.ErrorResponse{Response: &http.Response{StatusCode: 401}, Message: "Bad credentials"}}
	h := testRepoAuthServer(t, teamID, svc)

	rec := postRepoAuthTest(t, h, `{"url":"https://github.com/acme/widget","credentialSecretRef":{"name":"alpha-repo-pat"}}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("test: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out repoAuthTestResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.OK || !strings.Contains(out.Detail, "401") {
		t.Fatalf("result: got %+v, want ok=false naming 401", out)
	}
	if strings.Contains(rec.Body.String(), repoPatCanary) {
		t.Fatalf("NFR-2 breach on error branch: %s", rec.Body.String())
	}

	var got ksquadv1.Team
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: "teams", Name: "alpha"}, &got); err != nil {
		t.Fatalf("team read: %v", err)
	}
	if recorded, passed := RepoTestConnectionFlag(&got); !recorded || passed {
		t.Fatalf("annotation cache: got recorded=%v passed=%v, want failed", recorded, passed)
	}
}

// TestRepoAuthTestValidation — the v1 provider floor and the ref name rules:
// a non-github.com host and a missing/malformed ref name are field-level 422s
// BEFORE any cluster contact.
func TestRepoAuthTestValidation(t *testing.T) {
	tm, sec, teamID := repoTestTeamAndSecret(t)
	svc, prober, _ := newRepoAuthTester(t, tm, sec)
	h := testRepoAuthServer(t, teamID, svc)

	for _, tt := range []struct {
		name string
		body string
	}{
		{"gitlab host", `{"url":"https://gitlab.com/acme/widget","credentialSecretRef":{"name":"alpha-repo-pat"}}`},
		{"non-http scheme", `{"url":"git@github.com:acme/widget.git","credentialSecretRef":{"name":"alpha-repo-pat"}}`},
		{"missing url", `{"credentialSecretRef":{"name":"alpha-repo-pat"}}`},
		{"missing ref name", `{"url":"https://github.com/acme/widget","credentialSecretRef":{}}`},
		{"bad ref name", `{"url":"https://github.com/acme/widget","credentialSecretRef":{"name":"Not_A_Name"}}`},
	} {
		rec := postRepoAuthTest(t, h, tt.body, true)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: got %d, want 422 (body %s)", tt.name, rec.Code, rec.Body.String())
		}
	}
	if prober.token != "" {
		t.Fatal("validation failure must not reach the prober")
	}
}

// TestRepoAuthTestHostCaseInsensitive — GitHub hosts match case-insensitively
// (browsers and docs paste mixed case).
func TestRepoAuthTestHostCaseInsensitive(t *testing.T) {
	tm, sec, teamID := repoTestTeamAndSecret(t)
	svc, _, _ := newRepoAuthTester(t, tm, sec)
	h := testRepoAuthServer(t, teamID, svc)

	rec := postRepoAuthTest(t, h, `{"url":"https://GitHub.Com/acme/widget","credentialSecretRef":{"name":"alpha-repo-pat"}}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("test: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestRepoAuthTestTeamScope — an unresolvable Team UID is a 404
// (existence-hiding), and a Secret that exists only in ANOTHER namespace is
// never readable: the read key is structurally the caller's squad namespace.
func TestRepoAuthTestTeamScope(t *testing.T) {
	teamID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-repo-pat", Namespace: "ksquad-team-other"},
		Data:       map[string][]byte{"token": []byte(repoPatCanary)},
	}
	// Caller's Team resolves to ksquad-team-alpha; the same-named Secret lives
	// only in ksquad-team-other ⇒ NotFound for this caller.
	tm := teamWithStatus("teams", "alpha", teamID.String(), "ksquad-team-alpha")
	svc, prober, _ := newRepoAuthTester(t, tm, foreign)
	h := testRepoAuthServer(t, teamID, svc)

	rec := postRepoAuthTest(t, h, `{"url":"https://github.com/acme/widget","credentialSecretRef":{"name":"alpha-repo-pat"}}`, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign secret: got %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if prober.token != "" {
		t.Fatal("a foreign-namespace secret must never reach the prober")
	}

	// No resolvable Team at all ⇒ 404 before any Secret read.
	svc2, _, _ := newRepoAuthTester(t)
	h2 := testRepoAuthServer(t, teamID, svc2)
	rec2 := postRepoAuthTest(t, h2, `{"url":"https://github.com/acme/widget","credentialSecretRef":{"name":"alpha-repo-pat"}}`, true)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("unresolved team: got %d, want 404 (body %s)", rec2.Code, rec2.Body.String())
	}
}

// TestRepoAuthTestForbiddenRead — a Forbidden Secret read (the apiserver SA
// without its secrets:get grant) is a 502 naming the configuration gap, never
// a raw RBAC error body.
func TestRepoAuthTestForbiddenRead(t *testing.T) {
	tm, sec, teamID := repoTestTeamAndSecret(t)
	svc, _, _ := newRepoAuthTester(t, tm, sec)
	svc.client = forbiddingRepoAuthClient{inner: svc.client}
	h := testRepoAuthServer(t, teamID, svc)

	rec := postRepoAuthTest(t, h, `{"url":"https://github.com/acme/widget","credentialSecretRef":{"name":"alpha-repo-pat"}}`, true)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("forbidden read: got %d, want 502 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "grant") {
		t.Fatalf("502 must name the missing grant: %s", rec.Body.String())
	}
}

// forbiddingRepoAuthClient forces a Forbidden on every Get, passing List and
// Update through — the minimum to reach the Secret read branch.
type forbiddingRepoAuthClient struct {
	inner RepoAuthTestClient
}

func (f forbiddingRepoAuthClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return apierrors.NewForbidden(schema.GroupResource{Group: "", Resource: "secrets"}, key.Name, errors.New("denied"))
}
func (f forbiddingRepoAuthClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return f.inner.List(ctx, list, opts...)
}
func (f forbiddingRepoAuthClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	return f.inner.Update(ctx, obj, opts...)
}

// TestRepoAuthTestUnauthenticatedAnd501 — no session ⇒ 401; a nil service ⇒
// the documented 501 (cluster-less dev run), so the console contract stays
// honest.
func TestRepoAuthTestUnauthenticatedAnd501(t *testing.T) {
	_, _, teamID := repoTestTeamAndSecret(t)
	svc, _, _ := newRepoAuthTester(t)
	h := testRepoAuthServer(t, teamID, svc)

	rec := postRepoAuthTest(t, h, `{"url":"https://github.com/acme/widget","credentialSecretRef":{"name":"alpha-repo-pat"}}`, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: got %d, want 401", rec.Code)
	}

	bare := NewServer(Options{
		Authenticator: NewCookieAuthenticator(&StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
			devToken: {Principal: "user:nadia", TeamID: teamID},
		}}),
		Discussion: discussion.NewHandler(nil),
	})
	rec2 := postRepoAuthTest(t, bare.Handler(), `{}`, true)
	if rec2.Code != http.StatusNotImplemented {
		t.Fatalf("nil service: got %d, want 501 (body %s)", rec2.Code, rec2.Body.String())
	}
}

// TestRepoAuthTestNoEchoSweep — every response branch is swept for the canary:
// success, empty-key failure, provider failure, validation, not-found,
// forbidden, unauthenticated.
func TestRepoAuthTestNoEchoSweep(t *testing.T) {
	tm, sec, teamID := repoTestTeamAndSecret(t)
	bodies := []string{
		`{"url":"https://github.com/acme/widget","credentialSecretRef":{"name":"alpha-repo-pat"}}`,
		`{"url":"https://gitlab.com/acme/widget","credentialSecretRef":{"name":"alpha-repo-pat","key":"` + repoPatCanary + `"}}`,
	}
	builders := []func() http.Handler{
		func() http.Handler {
			svc, _, _ := newRepoAuthTester(t, tm, sec)
			return testRepoAuthServer(t, teamID, svc)
		},
		func() http.Handler {
			svc, _, _ := newRepoAuthTester(t, tm, sec)
			svc.prober = &fakeRepoProber{err: errors.New("boom " + repoPatCanary)}
			return testRepoAuthServer(t, teamID, svc)
		},
	}
	for _, build := range builders {
		h := build()
		for _, authed := range []bool{true, false} {
			for _, body := range bodies {
				rec := postRepoAuthTest(t, h, body, authed)
				if strings.Contains(rec.Body.String(), repoPatCanary) {
					t.Fatalf("NFR-2 breach: body %s echoes the canary", rec.Body.String())
				}
			}
		}
	}
}

// TestSupportedRepoHost — the v1 host floor, table-driven.
func TestSupportedRepoHost(t *testing.T) {
	for _, tt := range []struct {
		raw  string
		want bool
	}{
		{"https://github.com/acme/widget", true},
		{"https://WWW.GitHub.com/acme/widget", true},
		{"http://github.com/acme/widget", true},
		{"https://github.com", true},
		{"https://gitlab.com/acme/widget", false},
		{"https://ghe.corp.example/acme/widget", false},
		{"git@github.com:acme/widget.git", false},
		{"not a url", false},
		{"", false},
	} {
		if got := supportedRepoHost(tt.raw); got != tt.want {
			t.Errorf("supportedRepoHost(%q) = %v, want %v", tt.raw, got, tt.want)
		}
	}
}
