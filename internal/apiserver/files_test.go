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
	"errors"
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
	return buildFilesServerBusy(reader, nil, resolver, authn)
}

func buildFilesServerBusy(reader WorkspaceReader, busy BusySnapshotReader, resolver ProjectRoleResolver, authn discussion.Authenticator) *Server {
	opts := Options{
		Authenticator:       authn,
		Discussion:          nil,
		WorkspaceReader:     reader,
		WorkspaceBusyReader: busy,
		ProjectRoles:        resolver,
	}
	return NewServer(opts)
}

// fakeBusySnapshotReader serves (or refuses) the last-committed snapshot for a busy workspace.
type fakeBusySnapshotReader struct {
	listing *DirListing
	content *FileContent
	stat    *FileStat
	err     error // ErrNoWorkspaceSnapshot to refuse
}

func (f *fakeBusySnapshotReader) ListSnapshotDir(_ context.Context, _, _ string, _ int) (*DirListing, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.listing, nil
}

func (f *fakeBusySnapshotReader) ReadSnapshotFile(_ context.Context, _, _ string, _, _ int64) (*FileContent, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.content, nil
}

func (f *fakeBusySnapshotReader) StatSnapshotFile(_ context.Context, _, _ string) (*FileStat, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.stat, nil
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

func TestProjectFiles_NoBrowseTarget_Returns200HonestEmpty(t *testing.T) {
	// ISI-5140: no completed Run yet is an honest empty state, NOT a snapshot:
	// degraded=false + reason=no_browse_target so the UI skips the busy banner.
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
	if body.Degraded || body.Reason != "no_browse_target" || len(body.Entries) != 0 {
		t.Errorf("want honest empty listing reason=no_browse_target, got %+v", body)
	}
}

func TestProjectFiles_BusyServesSnapshotListing(t *testing.T) {
	// ISI-5140: a busy workspace must serve the last-committed snapshot, never a
	// fabricated empty listing.
	reader := &fakeWorkspaceReader{err: ErrWorkspaceBusy}
	busy := &fakeBusySnapshotReader{listing: &DirListing{Entries: []DirEntry{
		{Name: "README.md", Type: "file", Size: 42},
	}}}
	authn := filesAuthn("alice", true)
	srv := buildFilesServerBusy(reader, busy, nil, authn)

	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("busy+snapshot: got %d, want 200", w.Code)
	}
	var body DirListing
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Degraded || body.Reason != "workspace_busy" {
		t.Errorf("want degraded reason=workspace_busy, got %+v", body)
	}
	if len(body.Entries) != 1 || body.Entries[0].Name != "README.md" {
		t.Errorf("want snapshot entries served, got %+v", body.Entries)
	}
}

func TestProjectFiles_BusyNoSnapshot_LabelsEmptyHonestly(t *testing.T) {
	// Busy + no servable snapshot (nil reader / ErrNoWorkspaceSnapshot): the empty
	// form is still labelled reason=workspace_busy so the UI can say so.
	reader := &fakeWorkspaceReader{err: ErrWorkspaceBusy}
	busy := &fakeBusySnapshotReader{err: ErrNoWorkspaceSnapshot}
	authn := filesAuthn("alice", true)
	srv := buildFilesServerBusy(reader, busy, nil, authn)

	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("busy+no-snapshot: got %d, want 200", w.Code)
	}
	var body DirListing
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Degraded || body.Reason != "workspace_busy" || len(body.Entries) != 0 {
		t.Errorf("want labelled empty degraded listing, got %+v", body)
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

// TestProjectFiles_Preparing_Returns202: when the WorkspaceReader returns ErrReaderPreparing (cold
// start, ADR-0025 D1/S2c) the route returns 202 Accepted with code=preparing so the client polls.
func TestProjectFiles_Preparing_Returns202(t *testing.T) {
	reader := &fakeWorkspaceReader{err: ErrReaderPreparing}
	authn := filesAuthn("alice", true)
	srv := buildFilesServer(reader, nil, authn)

	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-cold/files", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Errorf("preparing: got %d, want 202 (body: %s)", w.Code, w.Body.String())
	}
	var body FileErrorBody
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Code != ErrCodePreparing {
		t.Errorf("code = %q, want %q", body.Code, ErrCodePreparing)
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
	// ISI-5140: honest empty state, NOT a snapshot — reason only, no degraded flag.
	if !strings.Contains(w.Body.String(), `"reason":"no_browse_target"`) || strings.Contains(w.Body.String(), `"degraded":true`) {
		t.Errorf("content no browse target: want reason=no_browse_target without degraded, body: %s", w.Body.String())
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
	// ISI-5140: honest empty state, NOT a snapshot — reason only, no degraded flag.
	if !strings.Contains(w.Body.String(), `"reason":"no_browse_target"`) || strings.Contains(w.Body.String(), `"degraded":true`) {
		t.Errorf("stat no browse target: want reason=no_browse_target without degraded, body: %s", w.Body.String())
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

// ---- /files/stream (ADR-0025 D5) ----------------------------------------

func TestProjectFilesStream_NilReader_Returns501(t *testing.T) {
	srv := buildFilesServer(nil, nil, filesAuthn("u1", true))
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj1/files/stream?path=.", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotImplemented {
		t.Errorf("nil reader: got %d, want 501", w.Code)
	}
}

func TestProjectFilesStream_HappyPath_SinglePage(t *testing.T) {
	reader := &fakeWorkspaceReader{listing: &DirListing{
		Entries: []DirEntry{
			{Name: "a.go", Type: "file", Size: 100},
			{Name: "src", Type: "dir"},
		},
	}}
	srv := buildFilesServer(reader, nil, filesAuthn("u1", true))
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj1/files/stream?path=.", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/x-ndjson") {
		t.Errorf("want application/x-ndjson, got %q", ct)
	}

	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	// expect: preamble + 2 entries + done = 4 lines
	if len(lines) != 4 {
		t.Fatalf("want 4 NDJSON lines, got %d: %q", len(lines), w.Body.String())
	}
	// Last line must be {"done":true}
	var last streamEntry
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil || !last.Done {
		t.Errorf("last line must be done marker, got %q", lines[len(lines)-1])
	}
	// Second line must be first entry
	var first streamEntry
	if err := json.Unmarshal([]byte(lines[1]), &first); err != nil || first.Name != "a.go" {
		t.Errorf("second line must be a.go entry, got %q", lines[1])
	}
}

// multiPageReader serves different DirListing responses per page index.
type multiPageReader struct {
	pages []*DirListing
}

func (p *multiPageReader) ListDir(_ context.Context, _, _ string, page int) (*DirListing, error) {
	if page >= len(p.pages) {
		return &DirListing{}, nil
	}
	return p.pages[page], nil
}

func (p *multiPageReader) ReadFile(_ context.Context, _, _ string, _, _ int64) (*FileContent, error) {
	return nil, errors.New("not implemented")
}

func (p *multiPageReader) StatFile(_ context.Context, _, _ string) (*FileStat, error) {
	return nil, errors.New("not implemented")
}

func TestProjectFilesStream_MultiPage(t *testing.T) {
	reader := &multiPageReader{pages: []*DirListing{
		{Entries: []DirEntry{{Name: "a", Type: "file"}}, NextPage: 1},
		{Entries: []DirEntry{{Name: "b", Type: "dir"}}, NextPage: 0},
	}}
	srv := buildFilesServer(reader, nil, filesAuthn("u1", true))
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj1/files/stream?path=.", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	// preamble + entry a + entry b + done = 4 lines
	if len(lines) != 4 {
		t.Fatalf("want 4 NDJSON lines, got %d: %q", len(lines), w.Body.String())
	}
	var entryA, entryB streamEntry
	if err := json.Unmarshal([]byte(lines[1]), &entryA); err != nil || entryA.Name != "a" {
		t.Errorf("want entry a at line 1, got %q", lines[1])
	}
	if err := json.Unmarshal([]byte(lines[2]), &entryB); err != nil || entryB.Name != "b" {
		t.Errorf("want entry b at line 2, got %q", lines[2])
	}
}

func TestProjectFilesStream_BusyDegraded(t *testing.T) {
	reader := &fakeWorkspaceReader{err: ErrWorkspaceBusy}
	busy := &fakeBusySnapshotReader{listing: &DirListing{
		Entries: []DirEntry{{Name: "snap.go", Type: "file"}},
	}}
	srv := buildFilesServerBusy(reader, busy, nil, filesAuthn("u1", true))
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj1/files/stream?path=.", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	// preamble + 1 entry + done = 3 lines
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d: %q", len(lines), w.Body.String())
	}
	var preamble streamEntry
	if err := json.Unmarshal([]byte(lines[0]), &preamble); err != nil {
		t.Fatalf("decode preamble: %v", err)
	}
	if !preamble.Degraded || preamble.Reason != reasonWorkspaceBusy {
		t.Errorf("preamble must be degraded workspace_busy, got %+v", preamble)
	}
}

func TestProjectFilesStream_TraversalRejected(t *testing.T) {
	srv := buildFilesServer(&fakeWorkspaceReader{listing: &DirListing{}}, nil, filesAuthn("u1", true))
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj1/files/stream?path=../secret", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("traversal: got %d, want 400", w.Code)
	}
}
