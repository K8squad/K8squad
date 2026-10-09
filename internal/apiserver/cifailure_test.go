package apiserver

// cifailure_test.go — ISI-5595 WS-E: the Actions CI-failure config sub-resource
// behind GET/PUT/PATCH /api/projects/{projectId}/repo/ci-automation.
//
// Mirrors issuetriage_test.go. Covers:
//   - nil spec ⇒ "off" view with conclusions default (["failure"]);
//   - contributor write persists + read-back exact; enabledBy/enabledAt stamped;
//   - a bad conclusion enum ⇒ 422; enabled⇒agentId ⇒ 422;
//   - disable clears provenance; RBAC (401/403/404).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
)

func testCiFailureServer(t *testing.T, cl client.WithWatch) http.Handler {
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
		CiFailure:     NewCiFailureService(cl, cl, roles),
		ProjectRoles:  roles,
	})
	return srv.Handler()
}

func cfGet(t *testing.T, h http.Handler, token, projectID string) (*httptest.ResponseRecorder, *CiFailureView) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/repo/ci-automation", nil), token))
	if rec.Code != http.StatusOK {
		return rec, nil
	}
	var out CiFailureView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode view: %v (body %s)", err, rec.Body.String())
	}
	return rec, &out
}

func cfWrite(t *testing.T, h http.Handler, token, projectID, method string, payload any) (*httptest.ResponseRecorder, *CiFailureView) {
	t.Helper()
	buf, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	req := withSession(httptest.NewRequest(method, "/api/projects/"+projectID+"/repo/ci-automation", bytes.NewReader(buf)), token)
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return rec, nil
	}
	var out CiFailureView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode view: %v (body %s)", err, rec.Body.String())
	}
	return rec, &out
}

func TestCiFailureReadNilDefaults(t *testing.T) {
	cl := newRAClient(t, raFixtures()...)
	h := testCiFailureServer(t, cl)

	_, v := cfGet(t, h, raViewerToken, "web")
	if v == nil {
		t.Fatal("want 200 off-view")
	}
	if v.Enabled {
		t.Fatalf("nil spec must be off: %+v", v)
	}
	if len(v.Conclusions) != 1 || v.Conclusions[0] != ksquadv1.CiFailureConclusionFailure {
		t.Fatalf("conclusions default (['failure']) not applied: %+v", v.Conclusions)
	}
	if v.BranchFilter == nil {
		t.Fatal("branchFilter must be a non-nil slice on the wire")
	}
	if v.CanEdit {
		t.Fatal("viewer must not have canEdit")
	}
}

func TestCiFailureWritePersistsAndStamps(t *testing.T) {
	cl := newRAClient(t, raFixtures()...)
	h := testCiFailureServer(t, cl)

	rec, v := cfWrite(t, h, raContributorToken, "web", http.MethodPut, map[string]any{
		"enabled":      true,
		"agentId":      "writer",
		"branchFilter": []string{"main", "release"},
		"conclusions":  []string{"failure", "timed_out"},
		"enabledBy":    "user:impersonated",
	})
	if v == nil {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !v.Enabled || v.AgentID != "writer" || len(v.Conclusions) != 2 {
		t.Fatalf("write view: %+v", v)
	}
	if v.EnabledBy != "user:carol" {
		t.Fatalf("enabledBy must be server-stamped, got %q", v.EnabledBy)
	}

	var project ksquadv1.Project
	_ = cl.Get(context.Background(), client.ObjectKey{Namespace: "squad-a", Name: "web"}, &project)
	spec := project.Spec.Repo.Automation.CiFailure
	if spec == nil || spec.EnabledAt == nil || spec.EnabledAt.IsZero() {
		t.Fatalf("enabledAt watermark not stamped: %+v", spec)
	}
	if len(spec.BranchFilter) != 2 {
		t.Fatalf("branchFilter not stored verbatim: %+v", spec.BranchFilter)
	}

	_, rv := cfGet(t, h, raContributorToken, "web")
	if rv == nil || !rv.Enabled || rv.AgentID != "writer" || len(rv.Conclusions) != 2 {
		t.Fatalf("read-back mismatch: %+v", rv)
	}
}

func TestCiFailureBadConclusion422(t *testing.T) {
	cl := newRAClient(t, raFixtures()...)
	h := testCiFailureServer(t, cl)

	rec, _ := cfWrite(t, h, raContributorToken, "web", http.MethodPut, map[string]any{
		"enabled": true, "agentId": "writer", "conclusions": []string{"failure", "exploded"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for bad conclusion, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Fields []struct {
			Field string `json:"field"`
		} `json:"fields"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Fields) != 1 || body.Fields[0].Field != "conclusions" {
		t.Fatalf("want conclusions field error, got %s", rec.Body.String())
	}
}

func TestCiFailureEnabledRequiresAgent(t *testing.T) {
	cl := newRAClient(t, raFixtures()...)
	h := testCiFailureServer(t, cl)

	rec, _ := cfWrite(t, h, raContributorToken, "web", http.MethodPut, map[string]any{
		"enabled": true, "agentId": "",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for enabled-without-agent, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCiFailureDisableClearsProvenance(t *testing.T) {
	cl := newRAClient(t, raFixtures()...)
	h := testCiFailureServer(t, cl)

	_, _ = cfWrite(t, h, raContributorToken, "web", http.MethodPut, map[string]any{
		"enabled": true, "agentId": "writer",
	})
	_, v := cfWrite(t, h, raContributorToken, "web", http.MethodPut, map[string]any{
		"enabled": false, "agentId": "writer",
	})
	if v == nil || v.Enabled || v.EnabledBy != "" {
		t.Fatalf("disable view must clear provenance: %+v", v)
	}
	var project ksquadv1.Project
	_ = cl.Get(context.Background(), client.ObjectKey{Namespace: "squad-a", Name: "web"}, &project)
	if at := project.Spec.Repo.Automation.CiFailure.EnabledAt; at != nil {
		t.Fatalf("enabledAt must be cleared on disable, got %v", at)
	}
}

func TestCiFailureRBAC(t *testing.T) {
	cl := newRAClient(t, raFixtures()...)
	h := testCiFailureServer(t, cl)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/projects/web/repo/ci-automation", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 unauth, got %d", rec.Code)
	}

	rec, _ = cfWrite(t, h, raViewerToken, "web", http.MethodPut, map[string]any{"enabled": false})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 for viewer write, got %d: %s", rec.Code, rec.Body.String())
	}

	rec, _ = cfGet(t, h, raStrangerToken, "web")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for stranger read, got %d", rec.Code)
	}
}
