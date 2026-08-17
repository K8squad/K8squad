package apiserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/buildbrowser"
	"github.com/K8squad/K8squad/internal/discussion"
)

// gitInitRepo builds a tiny two-branch repo (base→run) for the build-route tests.
func gitInitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		full := append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)
		cmd := exec.Command("git", full...)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out.String())
		}
	}
	run("init", "-q", "-b", "base")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	run("checkout", "-q", "-b", "run")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("run\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "run")
	return dir
}

// buildTestServer wires the build-browser Service with two sessions: alice owns run-1 in teamA;
// bob is a same-Team non-owner. This lets one server prove both the allow and the existence-hiding
// deny path through the real route + BFFAuthz.
func buildTestServer(t *testing.T, repo string) (http.Handler, uuid.UUID) {
	t.Helper()
	teamA := uuid.New()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		"tok-alice": {Principal: "user:alice", TeamID: teamA},
		"tok-bob":   {Principal: "user:bob", TeamID: teamA},
	}}
	src := buildbrowser.NewStaticRunSource(map[string]buildbrowser.RunMeta{
		"run-1": {RunID: "run-1", TeamID: teamA, Principal: "user:alice", RepoPath: repo, HeadRef: "run", BaseRef: "base"},
	})
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		Builds:        buildbrowser.NewService(src, buildbrowser.NewGitReader()),
	})
	return srv.Handler(), teamA
}

func buildGet(t *testing.T, h http.Handler, tok, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: tok})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestBuildRoute_OwnerReads — the owning principal gets a real 200 read of each resource.
func TestBuildRoute_OwnerReads(t *testing.T) {
	repo := gitInitRepo(t)
	h, _ := buildTestServer(t, repo)

	for _, res := range []string{"tree", "diff", "meta"} {
		rec := buildGet(t, h, "tok-alice", "/api/runs/run-1/build/"+res)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: got %d, want 200 (body=%s)", res, rec.Code, rec.Body.String())
		}
	}
	rec := buildGet(t, h, "tok-alice", "/api/runs/run-1/build/file?path=a.txt")
	if rec.Code != http.StatusOK {
		t.Fatalf("file: got %d, want 200", rec.Code)
	}
	var fr buildbrowser.FileResult
	if err := json.Unmarshal(rec.Body.Bytes(), &fr); err != nil {
		t.Fatal(err)
	}
	if string(fr.Content) != "run\n" {
		t.Errorf("file content = %q, want %q", fr.Content, "run\n")
	}
}

// TestBuildRoute_ExistenceHiding — a same-Team non-owner and an unknown Run BOTH get 404, and a
// missing file inside an authorized Run is 404 too. Deny is never distinguishable from not-found.
func TestBuildRoute_ExistenceHiding(t *testing.T) {
	repo := gitInitRepo(t)
	h, _ := buildTestServer(t, repo)

	// same-Team non-owner → 404 (not 403)
	if rec := buildGet(t, h, "tok-bob", "/api/runs/run-1/build/tree"); rec.Code != http.StatusNotFound {
		t.Errorf("non-owner: got %d, want 404", rec.Code)
	}
	// unknown Run → identical 404
	if rec := buildGet(t, h, "tok-alice", "/api/runs/run-nope/build/tree"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown run: got %d, want 404", rec.Code)
	}
	// path traversal inside an authorized run → 404
	if rec := buildGet(t, h, "tok-alice", "/api/runs/run-1/build/file?path=../../../etc/passwd"); rec.Code != http.StatusNotFound {
		t.Errorf("traversal: got %d, want 404", rec.Code)
	}
	// unknown resource → 404
	if rec := buildGet(t, h, "tok-alice", "/api/runs/run-1/build/bogus"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown resource: got %d, want 404", rec.Code)
	}
	// missing path arg on file → 400 (decided before any lookup, reveals nothing)
	if rec := buildGet(t, h, "tok-alice", "/api/runs/run-1/build/file"); rec.Code != http.StatusBadRequest {
		t.Errorf("missing path: got %d, want 400", rec.Code)
	}
}

// TestBuildRoute_Unauthenticated — no session cookie → 401 before any read (deny-by-default).
func TestBuildRoute_Unauthenticated(t *testing.T) {
	repo := gitInitRepo(t)
	h, _ := buildTestServer(t, repo)
	req := httptest.NewRequest(http.MethodGet, "/api/runs/run-1/build/tree", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauth: got %d, want 401", rec.Code)
	}
}

// TestBuildRoute_NilBuildsKeeps501 — without a Run source wired the route keeps the documented 501.
func TestBuildRoute_NilBuildsKeeps501(t *testing.T) {
	teamA := uuid.New()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		"tok-alice": {Principal: "user:alice", TeamID: teamA},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		// Builds intentionally nil
	})
	rec := buildGet(t, srv.Handler(), "tok-alice", "/api/runs/run-1/build/tree")
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("nil Builds: got %d, want 501", rec.Code)
	}
}
