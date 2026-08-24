package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/artifactbrowser"
	"github.com/K8squad/K8squad/internal/buildbrowser"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/coord"
)

// artifactsTestServer wires the artifact-browser Service with the same three sessions the build
// routes prove: alice owns run-1 in teamA; bob is a same-Team non-owner; carol is cross-Team.
func artifactsTestServer(t *testing.T) (http.Handler, *fakeStore, string) {
	t.Helper()
	teamA, teamB := uuid.New(), uuid.New()
	rid := uuid.New().String()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		"tok-alice": {Principal: "user:alice", TeamID: teamA},
		"tok-bob":   {Principal: "user:bob", TeamID: teamA},
		"tok-carol": {Principal: "user:carol", TeamID: teamB},
	}}
	src := buildbrowser.NewStaticRunSource(map[string]buildbrowser.RunMeta{
		rid: {RunID: rid, TeamID: teamA, Principal: "user:alice"},
	})
	store := &fakeStore{
		rows: map[string][]artifactbrowser.Artifact{}, content: map[string][]byte{},
	}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		Artifacts:     artifactbrowser.NewService(src, store),
	})
	return srv.Handler(), store, rid
}

// TestArtifactsRoute_OwnerListsAndReads — the owning principal gets a real 200 listing (with the
// parsed handoff) and a real 200 content read through the actual route + BFFAuthz stack.
func TestArtifactsRoute_OwnerListsAndReads(t *testing.T) {
	h, store, rid := artifactsTestServer(t)
	uri := coord.AuditHandoffURI + "7"
	art := artifactbrowser.Artifact{
		ID: uuid.New().String(), WorkItemID: uuid.New().String(), RunID: rid,
		Kind: coord.HandoffKind, URI: uri, SHA256: "ab12", CreatedAt: time.Now(),
	}
	store.rows[rid] = []artifactbrowser.Artifact{art}
	store.content[uri] = []byte(`{"did":["shipped the thing"],"findings":"f"}`)

	rec := artifactsGet(t, h, "tok-alice", "/api/runs/"+rid+"/artifacts")
	if rec.Code != http.StatusOK {
		t.Fatalf("list code = %d body=%s", rec.Code, rec.Body.String())
	}
	var listing artifactbrowser.Listing
	if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if listing.RunID != rid || len(listing.Artifacts) != 1 || listing.Handoff == nil {
		t.Fatalf("listing = %+v", listing)
	}

	rec = artifactsGet(t, h, "tok-alice", "/api/runs/"+rid+"/artifacts/"+art.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("content code = %d body=%s", rec.Code, rec.Body.String())
	}
	var res artifactbrowser.ContentResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Artifact.ID != art.ID || res.Size == 0 {
		t.Fatalf("content result = %+v", res)
	}
}

// TestArtifactsRoute_ExistenceHiding — a same-Team non-owner, a cross-Team caller, and an unknown
// Run all get the SAME 404 through the real route (deny ≡ not-found, NFR-SEC5).
func TestArtifactsRoute_ExistenceHiding(t *testing.T) {
	h, store, rid := artifactsTestServer(t)
	store.rows[rid] = []artifactbrowser.Artifact{{
		ID: uuid.New().String(), WorkItemID: uuid.New().String(), RunID: rid,
		Kind: coord.HandoffKind, URI: coord.AuditHandoffURI + "1", SHA256: "x", CreatedAt: time.Now(),
	}}

	for _, tok := range []string{"tok-bob", "tok-carol"} {
		if rec := artifactsGet(t, h, tok, "/api/runs/"+rid+"/artifacts"); rec.Code != http.StatusNotFound {
			t.Errorf("%s list: code = %d, want 404", tok, rec.Code)
		}
	}
	unknown := uuid.New().String()
	if rec := artifactsGet(t, h, "tok-alice", "/api/runs/"+unknown+"/artifacts"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown run: code = %d, want 404", rec.Code)
	}
}

// TestArtifactsRoute_Unauthenticated401 — no cookie ⇒ 401 (the authz choke point, not a 404).
func TestArtifactsRoute_Unauthenticated401(t *testing.T) {
	h, _, _ := artifactsTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/runs/"+uuid.New().String()+"/artifacts", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
}

// TestArtifactsRoute_NilServiceKeepsDocumented501 — without a coord store wired the routes keep
// the honest 501 contract (same dev-run posture as the build and overview routes).
func TestArtifactsRoute_NilServiceKeepsDocumented501(t *testing.T) {
	teamA := uuid.New()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		"tok-alice": {Principal: "user:alice", TeamID: teamA},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
	})
	h := srv.Handler()
	for _, path := range []string{"/api/runs/x/artifacts", "/api/runs/x/artifacts/y"} {
		if rec := artifactsGet(t, h, "tok-alice", path); rec.Code != http.StatusNotImplemented {
			t.Errorf("%s: code = %d, want 501", path, rec.Code)
		}
	}
}

// TestArtifactsRoute_WriteVerbsRejected — the read model is GET-only; POST is structurally absent
// (405), matching the BFF route's no-mutating-verb contract.
func TestArtifactsRoute_WriteVerbsRejected(t *testing.T) {
	h, _, rid := artifactsTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/runs/"+rid+"/artifacts", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "tok-alice"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST code = %d, want 405", rec.Code)
	}
}

// ── shared helpers ─────────────────────────────────────────────────────────────────────────────

type fakeStore struct {
	rows    map[string][]artifactbrowser.Artifact
	content map[string][]byte
}

func (f *fakeStore) ListByRun(_ context.Context, runID string) ([]artifactbrowser.Artifact, error) {
	return f.rows[runID], nil
}

func (f *fakeStore) GetByRunAndID(_ context.Context, runID, artifactID string) (artifactbrowser.Artifact, bool, error) {
	for _, a := range f.rows[runID] {
		if a.ID == artifactID {
			return a, true, nil
		}
	}
	return artifactbrowser.Artifact{}, false, nil
}

func (f *fakeStore) Content(_ context.Context, a artifactbrowser.Artifact) ([]byte, error) {
	raw, ok := f.content[a.URI]
	if !ok {
		return nil, context.Canceled
	}
	return raw, nil
}

func artifactsGet(t *testing.T, h http.Handler, tok, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: tok})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
