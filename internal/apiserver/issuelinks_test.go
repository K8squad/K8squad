package apiserver

// issuelinks_test.go — story 11.2 (ISI-2738) issue⇄work-item linkage API tests:
// auth, §12.1 tenancy resolution (existence-hiding), request validation, and
// error mapping over a fake IssueLinkStore — the same two-session discipline
// as the 8.8a dashboard tests (the store seam, never a DB).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/issuesync"
)

// fakeLinkStore records calls and answers canned results/errors.
type fakeLinkStore struct {
	gotNS, gotProject string

	listed    bool
	establish bool
	deleted   bool
	gotParams issuesync.LinkParams
	gotDelete [3]string // ns, project, externalID

	listErr   error
	estErr    error
	delErr    error
	listLinks []issuesync.Link
}

func (f *fakeLinkStore) ListLinks(_ context.Context, ns, project string) ([]issuesync.Link, error) {
	f.listed = true
	f.gotNS, f.gotProject = ns, project
	return f.listLinks, f.listErr
}

func (f *fakeLinkStore) EstablishLink(_ context.Context, p issuesync.LinkParams) (issuesync.Link, error) {
	f.establish = true
	f.gotParams = p
	f.gotNS, f.gotProject = p.ProjectNamespace, p.ProjectName
	if f.estErr != nil {
		return issuesync.Link{}, f.estErr
	}
	return issuesync.Link{
		ProjectNamespace: p.ProjectNamespace,
		ProjectName:      p.ProjectName,
		WorkItemID:       p.WorkItemID,
		Provider:         p.Provider,
		Repo:             p.Repo,
		ExternalID:       p.ExternalID,
		Direction:        p.Direction,
		Provenance:       p.Provenance,
		LastWriter:       issuesync.WriterExternal,
		ExternalLabels:   []string{},
	}, nil
}

func (f *fakeLinkStore) DeleteLink(_ context.Context, ns, project, externalID string) error {
	f.deleted = true
	f.gotDelete = [3]string{ns, project, externalID}
	return f.delErr
}

func testIssueLinkServer(t *testing.T, teamID uuid.UUID, store IssueLinkStore, objs ...client.Object) http.Handler {
	t.Helper()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: teamID},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		IssueLinks:    NewIssueLinkService(store, newDashboardClient(t, objs...)),
	})
	return srv.Handler()
}

func issueLinkReq(method, path, body, token string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if token != "" {
		r = withSession(r, token)
	}
	return r
}

// TestIssueLinkListOK — a Team-scoped caller lists their Project's links and
// the wire shape carries the AC2 provenance badge fields.
func TestIssueLinkListOK(t *testing.T) {
	teamID := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	store := &fakeLinkStore{listLinks: []issuesync.Link{{
		ProjectNamespace: "squad-a", ProjectName: "web", WorkItemID: "wi-1",
		Provider: "github", Repo: "acme/web", ExternalID: "42", Direction: "inbound",
		Provenance: issuesync.ProvenanceExternalSourced, LastWriter: issuesync.WriterExternal,
		ExternalState: "open", ExternalLabels: []string{"bug"},
	}}}
	h := testIssueLinkServer(t, teamID, store,
		team("squad-a", "alpha", teamID.String()),
		project("squad-a", "web", "https://github.com/acme/web"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, issueLinkReq(http.MethodGet, "/api/projects/web/issue-links", "", devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !store.listed || store.gotNS != "squad-a" || store.gotProject != "web" {
		t.Fatalf("store scoped wrong: %+v", store)
	}
	var out struct {
		Links []struct {
			WorkItemID string   `json:"workItemId"`
			Provenance string   `json:"provenance"`
			Labels     []string `json:"externalLabels"`
		} `json:"links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Links) != 1 || out.Links[0].Provenance != "external-sourced" || out.Links[0].WorkItemID != "wi-1" {
		t.Fatalf("link view: %+v", out.Links)
	}
	if len(out.Links[0].Labels) != 1 || out.Links[0].Labels[0] != "bug" {
		t.Fatalf("labels: %+v", out.Links[0].Labels)
	}
}

// TestIssueLinkEstablishOK — the write path resolves (namespace, project)
// from the caller's Team, validates provenance/direction, and answers 201.
func TestIssueLinkEstablishOK(t *testing.T) {
	teamID := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	store := &fakeLinkStore{}
	h := testIssueLinkServer(t, teamID, store,
		team("squad-a", "alpha", teamID.String()),
		project("squad-a", "web", "https://github.com/acme/web"))

	body := `{"workItemId":"wi-1","provider":"github","repo":"acme/web","externalId":"42",` +
		`"provenance":"ksquad-native","direction":"bidirectional"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, issueLinkReq(http.MethodPost, "/api/projects/web/issue-links", body, devToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	p := store.gotParams
	if !store.establish || p.ProjectNamespace != "squad-a" || p.ProjectName != "web" {
		t.Fatalf("tenancy not server-derived: %+v", p)
	}
	if p.Provenance != issuesync.ProvenanceKSquadNative || p.Direction != issuesync.DirectionBidirectional {
		t.Fatalf("params: %+v", p)
	}
}

// TestIssueLinkEstablishConflict — a divergent re-link answers 409 with the
// existing counterpart's coordinates.
func TestIssueLinkEstablishConflict(t *testing.T) {
	teamID := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	store := &fakeLinkStore{estErr: &issuesync.ErrLinkExists{Existing: issuesync.LinkParams{
		Provider: "github", Repo: "acme/web", ExternalID: "42", WorkItemID: "wi-other",
	}}}
	h := testIssueLinkServer(t, teamID, store,
		team("squad-a", "alpha", teamID.String()),
		project("squad-a", "web", "https://github.com/acme/web"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, issueLinkReq(http.MethodPost, "/api/projects/web/issue-links",
		`{"workItemId":"wi-1","provider":"github","repo":"acme/web","externalId":"42","provenance":"ksquad-native"}`, devToken))
	if rec.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "wi-other") {
		t.Fatalf("409 must name the existing counterpart: %s", rec.Body.String())
	}
}

// TestIssueLinkEstablishValidation — bad provenance, bad direction and
// missing fields each answer 400 without touching the store.
func TestIssueLinkEstablishValidation(t *testing.T) {
	teamID := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	cases := []struct {
		name string
		body string
	}{
		{"bad provenance", `{"workItemId":"wi-1","provider":"github","repo":"r","externalId":"1","provenance":"nope"}`},
		{"bad direction", `{"workItemId":"wi-1","provider":"github","repo":"r","externalId":"1","provenance":"ksquad-native","direction":"both"}`},
		{"missing fields", `{"provider":"github","provenance":"ksquad-native"}`},
		{"bad json", `{not-json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeLinkStore{}
			h := testIssueLinkServer(t, teamID, store,
				team("squad-a", "alpha", teamID.String()),
				project("squad-a", "web", "https://github.com/acme/web"))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, issueLinkReq(http.MethodPost, "/api/projects/web/issue-links", tc.body, devToken))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			if store.establish {
				t.Fatalf("store must not be called on a 400")
			}
		})
	}
}

// TestIssueLinkUnauthenticated — no session, no links: 401 before any store
// or tenancy work.
func TestIssueLinkUnauthenticated(t *testing.T) {
	teamID := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	store := &fakeLinkStore{}
	h := testIssueLinkServer(t, teamID, store)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, issueLinkReq(method, "/api/projects/web/issue-links", "", ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: got %d, want 401", method, rec.Code)
		}
		if store.listed || store.establish {
			t.Fatalf("store touched unauthenticated")
		}
	}
}

// TestIssueLinkForeignProjectIs404 — a Project outside the caller's Team
// namespace does not exist (existence-hiding NFR-SEC5), for every verb.
func TestIssueLinkForeignProjectIs404(t *testing.T) {
	teamID := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	store := &fakeLinkStore{}
	h := testIssueLinkServer(t, teamID, store,
		team("squad-a", "alpha", teamID.String()),
		project("squad-b", "web", "https://github.com/acme/web")) // foreign namespace

	verbs := []struct {
		method, path, body string
	}{
		{http.MethodGet, "/api/projects/web/issue-links", ""},
		{http.MethodPost, "/api/projects/web/issue-links", `{"workItemId":"wi-1","provider":"github","repo":"r","externalId":"1","provenance":"ksquad-native"}`},
		{http.MethodDelete, "/api/projects/web/issue-links/42", ""},
	}
	for _, v := range verbs {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, issueLinkReq(v.method, v.path, v.body, devToken))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s: got %d, want 404 (body %s)", v.method, v.path, rec.Code, rec.Body.String())
		}
	}
	if store.listed || store.establish || store.deleted {
		t.Fatalf("store touched through a foreign Project")
	}
}

// TestIssueLinkDeleteOK — DELETE carries the server-derived scope and the
// path externalId to the store and answers 200.
func TestIssueLinkDeleteOK(t *testing.T) {
	teamID := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	store := &fakeLinkStore{}
	h := testIssueLinkServer(t, teamID, store,
		team("squad-a", "alpha", teamID.String()),
		project("squad-a", "web", "https://github.com/acme/web"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, issueLinkReq(http.MethodDelete, "/api/projects/web/issue-links/42", "", devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !store.deleted || store.gotDelete != [3]string{"squad-a", "web", "42"} {
		t.Fatalf("delete scoped wrong: %+v", store.gotDelete)
	}
}

// TestIssueLinkDeleteMissing — an unknown link answers 404.
func TestIssueLinkDeleteMissing(t *testing.T) {
	teamID := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	store := &fakeLinkStore{delErr: errors.New("no link")}
	h := testIssueLinkServer(t, teamID, store,
		team("squad-a", "alpha", teamID.String()),
		project("squad-a", "web", "https://github.com/acme/web"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, issueLinkReq(http.MethodDelete, "/api/projects/web/issue-links/99", "", devToken))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}
