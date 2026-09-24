package apiserver

// reviewautomation_test.go — E1 (ISI-4763 / ISI-4750): the PR-review-automation
// config sub-resource behind GET/PUT/PATCH
// /api/projects/{projectId}/repo/review-automation.
//
// Covers the ACs:
//   AC1  nil spec ⇒ explicit "off" view with enum defaults;
//   AC2  read = member+ (viewer reads);
//   AC3  contributor write persists + read-back is exact;
//   AC4  enabledBy server-stamped from the caller, body value ignored, cleared on disable;
//   AC5  RBAC: unauth 401, below-contributor write 403, foreign/unknown 404;
//   AC6  D5 eligibility: eligible ok, not-team-agent 422, no-capability 422, empty reviewer 422;
//   AC7  enum validation 422;
//   AC9  a write authors no work item / mutates only spec.repo.reviewAutomation.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
)

// --- fixtures -----------------------------------------------------------------------------------

const (
	raContributorToken = "ra-contributor"
	raViewerToken      = "ra-viewer"
	raStrangerToken    = "ra-stranger" // authenticated but holds NO project membership
)

var raTeamID = uuid.MustParse("22222222-2222-2222-2222-222222222222")

// raAgent builds an Agent CR referencing roleName in the team namespace.
func raAgent(name, roleName string) *ksquadv1.Agent {
	return &ksquadv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "squad-a", Name: name},
		Spec: ksquadv1.AgentSpec{
			RuntimeRef: ksquadv1.ObjectRef{Name: "rt"},
			RoleRef:    ksquadv1.ObjectRef{Name: roleName},
		},
	}
}

func raRole(name string, phases ...string) *ksquadv1.Role {
	return &ksquadv1.Role{
		ObjectMeta: metav1.ObjectMeta{Namespace: "squad-a", Name: name},
		Spec:       ksquadv1.RoleSpec{ActivePhases: phases},
	}
}

// raTeam builds the owning Team with the given composition refs.
func raTeam(agents ...ksquadv1.ObjectRef) *ksquadv1.Team {
	tm := team("squad-a", "alpha", raTeamID.String())
	tm.Spec.Agents = agents
	return tm
}

// raFixtures wires the standard object set: a Team composed of a code_review-capable
// agent "rev" and a non-capable agent "writer", plus a Project "web".
func raFixtures() []client.Object {
	return []client.Object{
		raTeam(ksquadv1.ObjectRef{Name: "rev"}, ksquadv1.ObjectRef{Name: "writer"}),
		raAgent("rev", "reviewer-role"),
		raAgent("writer", "writer-role"),
		raRole("reviewer-role", "implementation", "code_review"),
		raRole("writer-role", "implementation"),
		project("squad-a", "web", "https://github.com/acme/web"),
	}
}

// newRAClient builds a fake client usable as BOTH the read cache and the write
// applier (the controller-runtime fake implements client.Client).
func newRAClient(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(objs...).Build()
}

// testReviewAutoServer mounts the review-automation route with the given client as
// both reader and applier, a membership store granting carol=contributor and
// vic=viewer, and sessions for both. roles is wired to both the service (canEdit /
// write-tier) and the route gate (requireProjectRole).
func testReviewAutoServer(t *testing.T, cl client.WithWatch) http.Handler {
	t.Helper()
	roles := &fakeMembershipStore{roles: map[string]string{
		"user:carol": "contributor",
		"user:vic":   "viewer",
	}}
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		raContributorToken: {Principal: "user:carol", TeamID: raTeamID},
		raViewerToken:      {Principal: "user:vic", TeamID: raTeamID},
		raStrangerToken:    {Principal: "user:mallory", TeamID: raTeamID},
	}}
	srv := NewServer(Options{
		Authenticator:    NewCookieAuthenticator(resolver),
		Discussion:       discussion.NewHandler(nil),
		ReviewAutomation: NewReviewAutomationService(cl, cl, roles),
		ProjectRoles:     roles,
	})
	return srv.Handler()
}

func raGet(t *testing.T, h http.Handler, token, projectID string) (*httptest.ResponseRecorder, *ReviewAutomationView) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/repo/review-automation", nil), token))
	if rec.Code != http.StatusOK {
		return rec, nil
	}
	var out ReviewAutomationView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode view: %v (body %s)", err, rec.Body.String())
	}
	return rec, &out
}

func raWrite(t *testing.T, h http.Handler, token, projectID, method string, payload any) (*httptest.ResponseRecorder, *ReviewAutomationView) {
	t.Helper()
	buf, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	req := withSession(httptest.NewRequest(method, "/api/projects/"+projectID+"/repo/review-automation", bytes.NewReader(buf)), token)
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return rec, nil
	}
	var out ReviewAutomationView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode view: %v (body %s)", err, rec.Body.String())
	}
	return rec, &out
}

// --- AC1: nil spec ⇒ off view with defaults ------------------------------------------------------

func TestReviewAutoReadNilDefaults(t *testing.T) {
	cl := newRAClient(t, raFixtures()...)
	h := testReviewAutoServer(t, cl)

	_, v := raGet(t, h, raViewerToken, "web")
	if v == nil {
		t.Fatal("want 200 off-view")
	}
	if v.Enabled {
		t.Fatalf("nil spec must be off: %+v", v)
	}
	if v.Scope != ksquadv1.ReviewScopeTeamAuthored || v.Trigger != ksquadv1.ReviewTriggerOnNewCommits {
		t.Fatalf("defaults not applied: scope=%q trigger=%q", v.Scope, v.Trigger)
	}
	if v.EnabledBy != "" || v.ReviewerAgentID != "" {
		t.Fatalf("off-view leaks provenance: %+v", v)
	}
	// AC2: a viewer can read (member+); canEdit is false for a viewer.
	if v.CanEdit {
		t.Fatal("viewer must not have canEdit")
	}
}

// --- AC3 + AC4: contributor write persists; enabledBy stamped, read-back exact --------------------

func TestReviewAutoWritePersistsAndStamps(t *testing.T) {
	cl := newRAClient(t, raFixtures()...)
	h := testReviewAutoServer(t, cl)

	// Body carries a bogus enabledBy — it MUST be ignored (AC4).
	rec, v := raWrite(t, h, raContributorToken, "web", http.MethodPut, map[string]any{
		"enabled":         true,
		"reviewerAgentId": "rev",
		"scope":           "all",
		"trigger":         "on_open",
		"enabledBy":       "user:impersonated",
	})
	if v == nil {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !v.Enabled || v.ReviewerAgentID != "rev" || v.Scope != "all" || v.Trigger != "on_open" {
		t.Fatalf("write view: %+v", v)
	}
	if v.EnabledBy != "user:carol" {
		t.Fatalf("enabledBy must be server-stamped to the caller, got %q", v.EnabledBy)
	}
	if !v.CanEdit {
		t.Fatal("contributor must have canEdit")
	}

	// AC3: read-back returns exactly what was written.
	_, rv := raGet(t, h, raContributorToken, "web")
	if rv == nil || !rv.Enabled || rv.ReviewerAgentID != "rev" || rv.Scope != "all" || rv.Trigger != "on_open" || rv.EnabledBy != "user:carol" {
		t.Fatalf("read-back mismatch: %+v", rv)
	}

	// AC9: only spec.repo.reviewAutomation changed — URL is intact, no other spec churn.
	var p ksquadv1.Project
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "squad-a", Name: "web"}, &p); err != nil {
		t.Fatalf("get project: %v", err)
	}
	if p.Spec.Repo.URL != "https://github.com/acme/web" {
		t.Fatalf("write clobbered unrelated repo config: %+v", p.Spec.Repo)
	}
	if p.Spec.Repo.ReviewAutomation == nil || p.Spec.Repo.ReviewAutomation.EnabledBy != "user:carol" {
		t.Fatalf("persisted spec: %+v", p.Spec.Repo.ReviewAutomation)
	}
}

// AC4: disabling clears enabledBy, and a body enabledBy on a disable is dropped.
func TestReviewAutoDisableClearsEnabledBy(t *testing.T) {
	cl := newRAClient(t, raFixtures()...)
	h := testReviewAutoServer(t, cl)

	// Enable first.
	if _, v := raWrite(t, h, raContributorToken, "web", http.MethodPut, map[string]any{
		"enabled": true, "reviewerAgentId": "rev",
	}); v == nil || v.EnabledBy != "user:carol" {
		t.Fatalf("enable failed: %+v", v)
	}
	// Disable — enabledBy must clear, and a body value must not stick.
	_, v := raWrite(t, h, raContributorToken, "web", http.MethodPatch, map[string]any{
		"enabled": false, "enabledBy": "user:ghost",
	})
	if v == nil {
		t.Fatal("disable want 200")
	}
	if v.Enabled || v.EnabledBy != "" {
		t.Fatalf("disable must clear enabledBy: %+v", v)
	}
}

// --- AC5: RBAC ----------------------------------------------------------------------------------

func TestReviewAutoUnauthenticated(t *testing.T) {
	cl := newRAClient(t, raFixtures()...)
	h := testReviewAutoServer(t, cl)
	rec := httptest.NewRecorder()
	// No session cookie ⇒ 401.
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/projects/web/repo/review-automation", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestReviewAutoViewerWriteForbidden(t *testing.T) {
	cl := newRAClient(t, raFixtures()...)
	h := testReviewAutoServer(t, cl)
	// A viewer clears the member+ route gate but must not write (contributor+).
	rec, _ := raWrite(t, h, raViewerToken, "web", http.MethodPut, map[string]any{
		"enabled": true, "reviewerAgentId": "rev",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestReviewAutoBelowMemberHidden404(t *testing.T) {
	cl := newRAClient(t, raFixtures()...)
	h := testReviewAutoServer(t, cl)
	// Authenticated but with no membership on the project ⇒ 404 existence-hiding,
	// never a distinguishable 403.
	rec, _ := raGet(t, h, raStrangerToken, "web")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 existence-hiding for below-member, got %d", rec.Code)
	}
}

func TestReviewAutoForeignProject404(t *testing.T) {
	cl := newRAClient(t, raFixtures()...)
	h := testReviewAutoServer(t, cl)
	rec, _ := raGet(t, h, raContributorToken, "does-not-exist")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 existence-hiding, got %d", rec.Code)
	}
}

// --- AC6: D5 eligibility -------------------------------------------------------------------------

func TestReviewAutoEligibilityRejections(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
	}{
		{"empty reviewer when enabled", map[string]any{"enabled": true, "reviewerAgentId": ""}},
		{"not a team agent", map[string]any{"enabled": true, "reviewerAgentId": "stranger"}},
		{"no code_review capability", map[string]any{"enabled": true, "reviewerAgentId": "writer"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cl := newRAClient(t, raFixtures()...)
			h := testReviewAutoServer(t, cl)
			rec, _ := raWrite(t, h, raContributorToken, "web", http.MethodPut, tc.payload)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("want 422, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestReviewAutoEligibilityAccepts(t *testing.T) {
	cl := newRAClient(t, raFixtures()...)
	h := testReviewAutoServer(t, cl)
	rec, v := raWrite(t, h, raContributorToken, "web", http.MethodPut, map[string]any{
		"enabled": true, "reviewerAgentId": "rev",
	})
	if v == nil {
		t.Fatalf("code_review-capable team agent must be accepted, got %d: %s", rec.Code, rec.Body.String())
	}
}

// A disabled write need not name a reviewer (the enabled⇒reviewer rule only fires on enable).
func TestReviewAutoDisabledWriteNoReviewerOK(t *testing.T) {
	cl := newRAClient(t, raFixtures()...)
	h := testReviewAutoServer(t, cl)
	rec, v := raWrite(t, h, raContributorToken, "web", http.MethodPut, map[string]any{
		"enabled": false, "scope": "all",
	})
	if v == nil {
		t.Fatalf("disabled write must succeed without a reviewer, got %d: %s", rec.Code, rec.Body.String())
	}
	if v.Enabled || v.Scope != "all" {
		t.Fatalf("view: %+v", v)
	}
}

// --- AC7: enum validation -----------------------------------------------------------------------

func TestReviewAutoBadEnum422(t *testing.T) {
	cl := newRAClient(t, raFixtures()...)
	h := testReviewAutoServer(t, cl)
	for _, bad := range []map[string]any{
		{"enabled": false, "scope": "everything"},
		{"enabled": false, "trigger": "sometimes"},
	} {
		rec, _ := raWrite(t, h, raContributorToken, "web", http.MethodPut, bad)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("want 422 for %v, got %d", bad, rec.Code)
		}
	}
}

// --- ISI-4779: eligible-agents read surface (D5 dropdown pre-filter) -----------------------------

// raEligible GETs the eligible-agents surface and decodes the roster on 200.
func raEligible(t *testing.T, h http.Handler, token, projectID string) (*httptest.ResponseRecorder, *EligibleAgentsView) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/repo/review-automation/eligible-agents", nil), token))
	if rec.Code != http.StatusOK {
		return rec, nil
	}
	var out EligibleAgentsView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode eligible-agents: %v (body %s)", err, rec.Body.String())
	}
	return rec, &out
}

// The roster is pre-filtered to code_review-capable team agents: "rev" (role has
// the code_review phase) is present, "writer" (role lacks it) is not — and the
// list is exactly what CheckReviewerEligibility would accept on write.
func TestReviewAutoEligibleAgentsPreFilter(t *testing.T) {
	cl := newRAClient(t, raFixtures()...)
	h := testReviewAutoServer(t, cl)
	rec, v := raEligible(t, h, raViewerToken, "web") // member+ read
	if v == nil {
		t.Fatalf("want 200 roster, got %d: %s", rec.Code, rec.Body.String())
	}
	names := map[string]bool{}
	for _, a := range v.Agents {
		names[a.Name] = true
	}
	if !names["rev"] {
		t.Fatalf("code_review-capable agent 'rev' must be listed: %+v", v.Agents)
	}
	if names["writer"] {
		t.Fatalf("non-capable agent 'writer' must be filtered out: %+v", v.Agents)
	}
	if len(v.Agents) != 1 {
		t.Fatalf("want exactly the one eligible agent, got %+v", v.Agents)
	}
}

func TestReviewAutoEligibleAgentsUnauthenticated(t *testing.T) {
	cl := newRAClient(t, raFixtures()...)
	h := testReviewAutoServer(t, cl)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/projects/web/repo/review-automation/eligible-agents", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

// A below-member / foreign caller gets 404 existence-hiding, mirroring the config
// read — the roster never leaks that the project exists.
func TestReviewAutoEligibleAgentsHidden404(t *testing.T) {
	cl := newRAClient(t, raFixtures()...)
	h := testReviewAutoServer(t, cl)
	if rec, _ := raEligible(t, h, raStrangerToken, "web"); rec.Code != http.StatusNotFound {
		t.Fatalf("below-member want 404, got %d", rec.Code)
	}
	if rec, _ := raEligible(t, h, raContributorToken, "does-not-exist"); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign project want 404, got %d", rec.Code)
	}
}

// --- 501: nil service keeps the documented contract ---------------------------------------------

func TestReviewAutoNilService501(t *testing.T) {
	roles := &fakeMembershipStore{roles: map[string]string{"user:carol": "contributor"}}
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		raContributorToken: {Principal: "user:carol", TeamID: raTeamID},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		ProjectRoles:  roles,
		// ReviewAutomation intentionally nil.
	})
	h := srv.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/projects/web/repo/review-automation", nil), raContributorToken))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("nil service want 501, got %d", rec.Code)
	}
	// ISI-4779: the eligible-agents surface honors the same nil-service 501.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/projects/web/repo/review-automation/eligible-agents", nil), raContributorToken))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("nil service eligible-agents want 501, got %d", rec.Code)
	}
}
