package apiserver

// issuetriage_test.go — ISI-5595 WS-E: the issue auto-triage config sub-resource
// behind GET/PUT/PATCH /api/projects/{projectId}/repo/issue-triage.
//
// Mirrors reviewautomation_test.go. Covers:
//   - nil spec ⇒ explicit "off" view with onlyUnassigned default (true);
//   - read = member+ (viewer reads, no canEdit);
//   - contributor write persists + read-back exact; a nil automation group is
//     allocated on first write;
//   - enabledBy + enabledAt server-stamped on enable, body value ignored, cleared
//     on disable; enabledAt PRESERVED across an enabled re-save (forward-only);
//   - enabled⇒triageAgentId 422; a NON-code_review agent is accepted (no capability
//     pre-check, unlike review-automation);
//   - RBAC: unauth 401, below-contributor write 403, foreign/unknown 404;
//   - a write mutates ONLY spec.repo.automation.issueTriage.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
)

// testIssueTriageServer mounts the issue-triage route reusing the review-automation
// fixtures (Team "alpha" / Project "web" in ns squad-a, carol=contributor,
// vic=viewer, mallory=stranger).
func testIssueTriageServer(t *testing.T, cl client.WithWatch) http.Handler {
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
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		IssueTriage:   NewIssueTriageService(cl, cl, roles),
		ProjectRoles:  roles,
	})
	return srv.Handler()
}

func itGet(t *testing.T, h http.Handler, token, projectID string) (*httptest.ResponseRecorder, *IssueTriageView) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/repo/issue-triage", nil), token))
	if rec.Code != http.StatusOK {
		return rec, nil
	}
	var out IssueTriageView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode view: %v (body %s)", err, rec.Body.String())
	}
	return rec, &out
}

func itWrite(t *testing.T, h http.Handler, token, projectID, method string, payload any) (*httptest.ResponseRecorder, *IssueTriageView) {
	t.Helper()
	buf, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	req := withSession(httptest.NewRequest(method, "/api/projects/"+projectID+"/repo/issue-triage", bytes.NewReader(buf)), token)
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return rec, nil
	}
	var out IssueTriageView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode view: %v (body %s)", err, rec.Body.String())
	}
	return rec, &out
}

func TestIssueTriageReadNilDefaults(t *testing.T) {
	cl := newRAClient(t, raFixtures()...)
	h := testIssueTriageServer(t, cl)

	_, v := itGet(t, h, raViewerToken, "web")
	if v == nil {
		t.Fatal("want 200 off-view")
	}
	if v.Enabled {
		t.Fatalf("nil spec must be off: %+v", v)
	}
	if !v.OnlyUnassigned {
		t.Fatalf("onlyUnassigned default (true) not applied: %+v", v)
	}
	if v.LabelFilter == nil {
		t.Fatal("labelFilter must be a non-nil slice on the wire")
	}
	if v.EnabledBy != "" || v.TriageAgentID != "" {
		t.Fatalf("off-view leaks provenance: %+v", v)
	}
	if v.CanEdit {
		t.Fatal("viewer must not have canEdit")
	}
}

func TestIssueTriageWritePersistsAndStamps(t *testing.T) {
	cl := newRAClient(t, raFixtures()...)
	h := testIssueTriageServer(t, cl)

	// Body carries a bogus enabledBy — it MUST be ignored. "writer" is NOT
	// code_review-capable, proving triage needs no reviewer capability.
	rec, v := itWrite(t, h, raContributorToken, "web", http.MethodPut, map[string]any{
		"enabled":        true,
		"triageAgentId":  "writer",
		"labelFilter":    []string{"bug", "needs-triage"},
		"onlyUnassigned": false,
		"enabledBy":      "user:impersonated",
	})
	if v == nil {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !v.Enabled || v.TriageAgentID != "writer" || v.OnlyUnassigned {
		t.Fatalf("write view: %+v", v)
	}
	if v.EnabledBy != "user:carol" {
		t.Fatalf("enabledBy must be server-stamped to the caller, got %q", v.EnabledBy)
	}
	if !v.CanEdit {
		t.Fatal("contributor must have canEdit")
	}

	// The stored spec carries the server-stamped EnabledAt watermark + dedup'd labels.
	var project ksquadv1.Project
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "squad-a", Name: "web"}, &project); err != nil {
		t.Fatalf("get project: %v", err)
	}
	spec := project.Spec.Repo.Automation.IssueTriage
	if spec == nil || spec.EnabledAt == nil || spec.EnabledAt.IsZero() {
		t.Fatalf("enabledAt watermark not stamped: %+v", spec)
	}
	if len(spec.LabelFilter) != 2 {
		t.Fatalf("labelFilter not stored verbatim: %+v", spec.LabelFilter)
	}
	// A write touches ONLY the issueTriage sub-policy — review-automation untouched.
	if project.Spec.Repo.ReviewAutomation != nil {
		t.Fatalf("write leaked into reviewAutomation: %+v", project.Spec.Repo.ReviewAutomation)
	}

	// Read-back is exact.
	_, rv := itGet(t, h, raContributorToken, "web")
	if rv == nil || !rv.Enabled || rv.TriageAgentID != "writer" || rv.OnlyUnassigned {
		t.Fatalf("read-back mismatch: %+v", rv)
	}
}

func TestIssueTriageWatermarkPreservedOnReSave(t *testing.T) {
	cl := newRAClient(t, raFixtures()...)
	h := testIssueTriageServer(t, cl)

	_, _ = itWrite(t, h, raContributorToken, "web", http.MethodPut, map[string]any{
		"enabled": true, "triageAgentId": "writer", "onlyUnassigned": true,
	})
	var p1 ksquadv1.Project
	_ = cl.Get(context.Background(), client.ObjectKey{Namespace: "squad-a", Name: "web"}, &p1)
	first := p1.Spec.Repo.Automation.IssueTriage.EnabledAt

	// Backdate the stored watermark so a (mistaken) re-stamp would be observable.
	back := metav1.NewTime(first.Add(-1000000000))
	p1.Spec.Repo.Automation.IssueTriage.EnabledAt = &back
	if err := cl.Update(context.Background(), &p1); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// Re-save while STILL enabled (changing only the label filter).
	_, _ = itWrite(t, h, raContributorToken, "web", http.MethodPut, map[string]any{
		"enabled": true, "triageAgentId": "writer", "onlyUnassigned": true,
		"labelFilter": []string{"p1"},
	})
	var p2 ksquadv1.Project
	_ = cl.Get(context.Background(), client.ObjectKey{Namespace: "squad-a", Name: "web"}, &p2)
	if !p2.Spec.Repo.Automation.IssueTriage.EnabledAt.Equal(&back) {
		t.Fatalf("watermark moved on enabled re-save: want %v got %v", back, p2.Spec.Repo.Automation.IssueTriage.EnabledAt)
	}
}

func TestIssueTriageDisableClearsProvenance(t *testing.T) {
	cl := newRAClient(t, raFixtures()...)
	h := testIssueTriageServer(t, cl)

	_, _ = itWrite(t, h, raContributorToken, "web", http.MethodPut, map[string]any{
		"enabled": true, "triageAgentId": "writer", "onlyUnassigned": true,
	})
	_, v := itWrite(t, h, raContributorToken, "web", http.MethodPut, map[string]any{
		"enabled": false, "triageAgentId": "writer", "onlyUnassigned": true,
	})
	if v == nil || v.Enabled {
		t.Fatalf("disable view: %+v", v)
	}
	if v.EnabledBy != "" {
		t.Fatalf("enabledBy must be cleared on disable, got %q", v.EnabledBy)
	}
	var project ksquadv1.Project
	_ = cl.Get(context.Background(), client.ObjectKey{Namespace: "squad-a", Name: "web"}, &project)
	if at := project.Spec.Repo.Automation.IssueTriage.EnabledAt; at != nil {
		t.Fatalf("enabledAt must be cleared on disable, got %v", at)
	}
}

func TestIssueTriageEnabledRequiresAgent(t *testing.T) {
	cl := newRAClient(t, raFixtures()...)
	h := testIssueTriageServer(t, cl)

	rec, _ := itWrite(t, h, raContributorToken, "web", http.MethodPut, map[string]any{
		"enabled": true, "triageAgentId": "", "onlyUnassigned": true,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for enabled-without-agent, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Fields []struct {
			Field string `json:"field"`
		} `json:"fields"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Fields) != 1 || body.Fields[0].Field != "triageAgentId" {
		t.Fatalf("want triageAgentId field error, got %s", rec.Body.String())
	}
}

func TestIssueTriageRBAC(t *testing.T) {
	cl := newRAClient(t, raFixtures()...)
	h := testIssueTriageServer(t, cl)

	// Unauthenticated read.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/projects/web/repo/issue-triage", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 unauth, got %d", rec.Code)
	}

	// A viewer cleared the member+ gate but a write is 403.
	rec, _ = itWrite(t, h, raViewerToken, "web", http.MethodPut, map[string]any{
		"enabled": false, "onlyUnassigned": true,
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 for viewer write, got %d: %s", rec.Code, rec.Body.String())
	}

	// A stranger (no project membership) gets existence-hiding 404 on read.
	rec, _ = itGet(t, h, raStrangerToken, "web")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for stranger read, got %d", rec.Code)
	}
}
