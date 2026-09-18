package apiserver

// S4b unit tests (ISI-3991, ADR-0012 §D2 — "Verification (smallest that proves it)").
//
// Coverage:
//   - workspaceJailPath: all jail-escape vectors + valid paths
//   - nil reader → 501 (AC6)
//   - foreign-team caller → 404 via requireProjectRole (AC3)
//   - global-admin → 200 with no membership required (AC3)
//   - path traversal rejected at the route (AC5 defence-in-depth)
//   - /files/content: missing path param → 400
//   - busy/degraded: ErrWorkspaceBusy surfaces as 200 + degraded=true (AC7)
//   - valid list and content round-trips (AC1, AC2)

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/auth"
)

// ---- test doubles --------------------------------------------------------

type fakeWorkspaceReader struct {
	listing *DirListing
	content *FileContent
	stat    *FileStat
	err     error
}

func (f *fakeWorkspaceReader) ListDir(_ context.Context, _, _ string, _ int) (*DirListing, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.listing, nil
}

func (f *fakeWorkspaceReader) ReadFile(_ context.Context, _, _ string, _, _ int64) (*FileContent, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.content, nil
}

func (f *fakeWorkspaceReader) StatFile(_ context.Context, _, _ string) (*FileStat, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.stat, nil
}

type fakeMembershipStore struct {
	// principal → role; absent = ErrNoMembership
	roles map[string]string
}

func (f *fakeMembershipStore) RoleForPrincipal(_ context.Context, principal, _ string) (string, error) {
	role, ok := f.roles[principal]
	if !ok {
		return "", auth.ErrNoMembership
	}
	return role, nil
}

// filesAuthn returns a stubAuthenticator (declared in authroutes_test.go) configured
// to accept the given principal with the given admin flag.
func filesAuthn(principal string, isAdmin bool) *stubAuthenticator {
	return &stubAuthenticator{
		author: discussion.AuthorContext{Principal: principal, IsAdmin: isAdmin},
		ok:     true,
	}
}

// buildFilesServer constructs a minimal Server with the files routes wired.
func buildFilesServer(reader WorkspaceReader, resolver ProjectRoleResolver, authn discussion.Authenticator) *Server {
	opts := Options{
		Authenticator:   authn,
		Discussion:      nil,
		WorkspaceReader: reader,
		ProjectRoles:    resolver,
	}
	return NewServer(opts)
}

// ---- workspaceJailPath ---------------------------------------------------

func TestWorkspaceJailPath(t *testing.T) {
	cases := []struct {
		raw  string
		want string
		err  bool
	}{
		{"", ".", false},
		{"/", ".", false},
		{".", ".", false},
		{"foo/bar", "foo/bar", false},
		{"/foo/bar", "foo/bar", false},
		{"foo/../bar", "bar", false},         // normalised, still inside
		{"foo/../../bar", "", true},          // escapes root
		{"../secret", "", true},              // escapes root
		{"/etc/passwd", "etc/passwd", false}, // absolute stripped, valid
		{"foo/./bar", "foo/bar", false},
	}
	for _, tc := range cases {
		got, err := workspaceJailPath(tc.raw)
		if tc.err && err == nil {
			t.Errorf("jailPath(%q): wanted error, got %q", tc.raw, got)
		}
		if !tc.err && err != nil {
			t.Errorf("jailPath(%q): unexpected error: %v", tc.raw, err)
		}
		if !tc.err && got != tc.want {
			t.Errorf("jailPath(%q): got %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// ---- /files route --------------------------------------------------------

func TestProjectFiles_NilReader_Returns501(t *testing.T) {
	authn := filesAuthn("alice", false)
	srv := buildFilesServer(nil, nil, authn)
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotImplemented {
		t.Errorf("nil reader: got %d, want 501", w.Code)
	}
}

func TestProjectFiles_ForeignTeam_Returns404(t *testing.T) {
	reader := &fakeWorkspaceReader{listing: &DirListing{Entries: []DirEntry{}}}
	resolver := &fakeMembershipStore{roles: map[string]string{
		"alice": auth.ProjectRoleViewer,
	}}
	authn := filesAuthn("bob", false) // bob has no membership
	srv := buildFilesServer(reader, resolver, authn)

	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("foreign team: got %d, want 404", w.Code)
	}
}

func TestProjectFiles_GlobalAdmin_Returns200(t *testing.T) {
	reader := &fakeWorkspaceReader{listing: &DirListing{Entries: []DirEntry{
		{Name: "main.go", Type: "file", Size: 1024},
	}}}
	resolver := &fakeMembershipStore{roles: map[string]string{}} // admin needs no membership row
	authn := filesAuthn("admin-principal", true)
	srv := buildFilesServer(reader, resolver, authn)

	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("global admin: got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var body DirListing
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Entries) != 1 || body.Entries[0].Name != "main.go" {
		t.Errorf("unexpected entries: %+v", body.Entries)
	}
}

func TestProjectFiles_PathTraversal_Returns400(t *testing.T) {
	reader := &fakeWorkspaceReader{listing: &DirListing{Entries: []DirEntry{}}}
	authn := filesAuthn("alice", true)
	srv := buildFilesServer(reader, nil, authn)

	for _, bad := range []string{"../secret", "foo/../../etc"} {
		r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files?path="+bad, nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("traversal %q: got %d, want 400", bad, w.Code)
		}
	}
}

func TestProjectFiles_BusyDegraded_Returns200WithFlag(t *testing.T) {
	reader := &fakeWorkspaceReader{err: ErrWorkspaceBusy}
	authn := filesAuthn("alice", true)
	srv := buildFilesServer(reader, nil, authn)

	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("busy: got %d, want 200", w.Code)
	}
	var body DirListing
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Degraded {
		t.Error("busy: expected degraded=true")
	}
}

func TestProjectFiles_NoBrowseTarget_Returns200DegradedEmpty(t *testing.T) {
	// Project exists but has no completed Run yet → degraded empty listing, not 5xx.
	reader := &fakeWorkspaceReader{err: ErrNoBrowseTarget}
	authn := filesAuthn("alice", true)
	srv := buildFilesServer(reader, nil, authn)

	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("no browse target: got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var body DirListing
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Degraded || len(body.Entries) != 0 {
		t.Errorf("want degraded empty listing, got %+v", body)
	}
}

func TestProjectFiles_ProjectNotFound_Returns404(t *testing.T) {
	// Unknown/nonexistent projectId → existence-hiding 404, not 500.
	reader := &fakeWorkspaceReader{err: ErrProjectNotFound}
	authn := filesAuthn("alice", true)
	srv := buildFilesServer(reader, nil, authn)

	r := httptest.NewRequest(http.MethodGet, "/api/projects/does-not-exist/files", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown project: got %d, want 404 (body: %s)", w.Code, w.Body.String())
	}
}

func TestProjectFiles_Member_Returns200(t *testing.T) {
	reader := &fakeWorkspaceReader{listing: &DirListing{Entries: []DirEntry{
		{Name: "Makefile", Type: "file", Size: 512},
	}}}
	resolver := &fakeMembershipStore{roles: map[string]string{"alice": auth.ProjectRoleViewer}}
	authn := filesAuthn("alice", false)
	srv := buildFilesServer(reader, resolver, authn)

	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("member: got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
}

// ---- /files/content route ------------------------------------------------

func TestProjectFilesContent_NilReader_Returns501(t *testing.T) {
	authn := filesAuthn("alice", true)
	srv := buildFilesServer(nil, nil, authn)
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files/content?path=main.go", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotImplemented {
		t.Errorf("nil reader content: got %d, want 501", w.Code)
	}
}

func TestProjectFilesContent_MissingPath_Returns400(t *testing.T) {
	reader := &fakeWorkspaceReader{content: &FileContent{Data: []byte("hello"), ContentType: "text", Size: 5}}
	authn := filesAuthn("alice", true)
	srv := buildFilesServer(reader, nil, authn)
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files/content", nil) // no path param
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing path: got %d, want 400", w.Code)
	}
}

func TestProjectFilesContent_TraversalRejected(t *testing.T) {
	reader := &fakeWorkspaceReader{content: &FileContent{Data: []byte("x"), ContentType: "text", Size: 1}}
	authn := filesAuthn("alice", true)
	srv := buildFilesServer(reader, nil, authn)
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files/content?path=../etc/passwd", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("content traversal: got %d, want 400", w.Code)
	}
}

func TestProjectFilesContent_HappyPath(t *testing.T) {
	data := []byte("package main\n")
	reader := &fakeWorkspaceReader{content: &FileContent{
		Data: data, ContentType: "text", Size: int64(len(data)),
		Offset: 0, Length: int64(len(data)),
	}}
	authn := filesAuthn("alice", true)
	srv := buildFilesServer(reader, nil, authn)
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files/content?path=main.go", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("content happy path: got %d, want 200", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "application/json") {
		t.Errorf("content-type: %q", w.Header().Get("Content-Type"))
	}
}

func TestProjectFilesContent_BusyDegraded(t *testing.T) {
	reader := &fakeWorkspaceReader{err: ErrWorkspaceBusy}
	authn := filesAuthn("alice", true)
	srv := buildFilesServer(reader, nil, authn)
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files/content?path=main.go", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("content busy: got %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"degraded":true`) {
		t.Errorf("content busy: degraded flag missing in body: %s", w.Body.String())
	}
}

func TestProjectFilesContent_NoBrowseTarget_Returns200Degraded(t *testing.T) {
	reader := &fakeWorkspaceReader{err: ErrNoBrowseTarget}
	authn := filesAuthn("alice", true)
	srv := buildFilesServer(reader, nil, authn)
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files/content?path=main.go", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("content no browse target: got %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"degraded":true`) {
		t.Errorf("content no browse target: degraded flag missing in body: %s", w.Body.String())
	}
}

func TestProjectFilesContent_ProjectNotFound_Returns404(t *testing.T) {
	reader := &fakeWorkspaceReader{err: ErrProjectNotFound}
	authn := filesAuthn("alice", true)
	srv := buildFilesServer(reader, nil, authn)
	r := httptest.NewRequest(http.MethodGet, "/api/projects/does-not-exist/files/content?path=main.go", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("content unknown project: got %d, want 404 (body: %s)", w.Code, w.Body.String())
	}
}

// ---- /files/stat route (ISI-4649) -----------------------------------------

func TestProjectFilesStat_NilReader_Returns501(t *testing.T) {
	authn := filesAuthn("alice", true)
	srv := buildFilesServer(nil, nil, authn)
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files/stat?path=main.go", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotImplemented {
		t.Errorf("nil reader stat: got %d, want 501", w.Code)
	}
}

func TestProjectFilesStat_MissingPath_Returns400(t *testing.T) {
	reader := &fakeWorkspaceReader{stat: &FileStat{Name: "main.go", Type: "file", Size: 13, ModTime: "2026-09-18T00:00:00Z"}}
	authn := filesAuthn("alice", true)
	srv := buildFilesServer(reader, nil, authn)
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files/stat", nil) // no path param
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing path: got %d, want 400", w.Code)
	}
}

func TestProjectFilesStat_TraversalRejected(t *testing.T) {
	reader := &fakeWorkspaceReader{stat: &FileStat{Name: "x", Type: "file"}}
	authn := filesAuthn("alice", true)
	srv := buildFilesServer(reader, nil, authn)
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files/stat?path=../etc/passwd", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("stat traversal: got %d, want 400", w.Code)
	}
}

func TestProjectFilesStat_JailRootRejected(t *testing.T) {
	reader := &fakeWorkspaceReader{stat: &FileStat{Name: ".", Type: "dir"}}
	authn := filesAuthn("alice", true)
	srv := buildFilesServer(reader, nil, authn)
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files/stat?path=/", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("stat jail root: got %d, want 400", w.Code)
	}
}

func TestProjectFilesStat_HappyPath_WithGit(t *testing.T) {
	reader := &fakeWorkspaceReader{stat: &FileStat{
		Name: "main.go", Type: "file", Size: 13, ModTime: "2026-09-18T01:02:03Z",
		Git: &GitChange{
			CommitHash: "deadbeef", Author: "alice", Message: "feat: add stat endpoint",
			Timestamp: "2026-09-17T22:00:00Z",
		},
	}}
	resolver := &fakeMembershipStore{roles: map[string]string{"alice": auth.ProjectRoleViewer}}
	authn := filesAuthn("alice", false)
	srv := buildFilesServer(reader, resolver, authn)

	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files/stat?path=main.go", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("stat happy path: got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var body FileStat
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ModTime != "2026-09-18T01:02:03Z" {
		t.Errorf("modTime: %q", body.ModTime)
	}
	if body.Git == nil || body.Git.CommitHash != "deadbeef" || body.Git.Author != "alice" {
		t.Errorf("git last-change missing or wrong: %+v", body.Git)
	}
}

func TestProjectFilesStat_GitUnavailable_OmitsGitField(t *testing.T) {
	// Graceful fallback: workspace is not a git checkout (or no git binary) ⇒ Git nil,
	// still 200 with mtime.
	reader := &fakeWorkspaceReader{stat: &FileStat{
		Name: "notes.txt", Type: "file", Size: 5, ModTime: "2026-09-18T01:02:03Z",
	}}
	authn := filesAuthn("alice", true)
	srv := buildFilesServer(reader, nil, authn)

	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files/stat?path=notes.txt", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("stat no-git: got %d, want 200", w.Code)
	}
	if strings.Contains(w.Body.String(), `"git"`) {
		t.Errorf("no-git fallback: git field must be omitted, body: %s", w.Body.String())
	}
}

func TestProjectFilesStat_BusyDegraded(t *testing.T) {
	reader := &fakeWorkspaceReader{err: ErrWorkspaceBusy}
	authn := filesAuthn("alice", true)
	srv := buildFilesServer(reader, nil, authn)
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files/stat?path=main.go", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("stat busy: got %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"degraded":true`) {
		t.Errorf("stat busy: degraded flag missing in body: %s", w.Body.String())
	}
}

func TestProjectFilesStat_NoBrowseTarget_Returns200Degraded(t *testing.T) {
	reader := &fakeWorkspaceReader{err: ErrNoBrowseTarget}
	authn := filesAuthn("alice", true)
	srv := buildFilesServer(reader, nil, authn)
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files/stat?path=main.go", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("stat no browse target: got %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"degraded":true`) {
		t.Errorf("stat no browse target: degraded flag missing in body: %s", w.Body.String())
	}
}

func TestProjectFilesStat_ProjectNotFound_Returns404(t *testing.T) {
	reader := &fakeWorkspaceReader{err: ErrProjectNotFound}
	authn := filesAuthn("alice", true)
	srv := buildFilesServer(reader, nil, authn)
	r := httptest.NewRequest(http.MethodGet, "/api/projects/does-not-exist/files/stat?path=main.go", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("stat unknown project: got %d, want 404 (body: %s)", w.Code, w.Body.String())
	}
}
