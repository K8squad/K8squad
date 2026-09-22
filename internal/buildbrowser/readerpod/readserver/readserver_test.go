/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package readserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newTestServer builds a jailed Server over a fresh temp workspace populated with:
//
//	/workspace/a.txt        "hello world"
//	/workspace/bin.dat      NUL-containing bytes
//	/workspace/sub/b.txt    "nested"
//	/workspace/outside.lnk  -> <sibling temp dir>/secret.txt  (symlink escaping the jail)
func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), []byte("hello world"))
	mustWrite(t, filepath.Join(root, "bin.dat"), []byte{'P', 'K', 0x00, 0x01, 0x02})
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "sub", "b.txt"), []byte("nested"))

	// A symlink that points OUTSIDE the jail — must never be followed.
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret.txt"), []byte("TOP SECRET"))
	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "outside.lnk")); err != nil {
			t.Fatal(err)
		}
	}

	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, root
}

func mustWrite(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func doList(t *testing.T, s *Server, path, page string) (*httptest.ResponseRecorder, DirListing) {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/list?path="+path+"&page="+page, nil)
	s.Handler().ServeHTTP(rr, req)
	var dl DirListing
	if rr.Code == http.StatusOK {
		if err := json.Unmarshal(rr.Body.Bytes(), &dl); err != nil {
			t.Fatalf("decode list: %v (%s)", err, rr.Body.String())
		}
	}
	return rr, dl
}

func doRead(t *testing.T, s *Server, query string) (*httptest.ResponseRecorder, FileContent) {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/read?"+query, nil)
	s.Handler().ServeHTTP(rr, req)
	var fc FileContent
	if rr.Code == http.StatusOK {
		if err := json.Unmarshal(rr.Body.Bytes(), &fc); err != nil {
			t.Fatalf("decode read: %v (%s)", err, rr.Body.String())
		}
	}
	return rr, fc
}

// AC3: listing the jail root returns entries with name/type/size.
func TestListRoot(t *testing.T) {
	s, _ := newTestServer(t)
	rr, dl := doList(t, s, "", "0")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	got := map[string]Entry{}
	for _, e := range dl.Entries {
		got[e.Name] = e
	}
	if got["a.txt"].Type != "file" || got["a.txt"].Size != 11 {
		t.Errorf("a.txt entry wrong: %+v", got["a.txt"])
	}
	if got["sub"].Type != "dir" || got["sub"].Size != 0 {
		t.Errorf("sub entry wrong: %+v", got["sub"])
	}
}

// AC3: nested directory listing works and empty/"/" both resolve to the root.
func TestListNestedAndRootAliases(t *testing.T) {
	s, _ := newTestServer(t)
	_, dl := doList(t, s, "sub", "0")
	if len(dl.Entries) != 1 || dl.Entries[0].Name != "b.txt" {
		t.Fatalf("sub listing wrong: %+v", dl.Entries)
	}
	for _, alias := range []string{"", "/", "."} {
		if rr, _ := doList(t, s, alias, "0"); rr.Code != http.StatusOK {
			t.Errorf("alias %q → status %d", alias, rr.Code)
		}
	}
}

// AC3: traversal and absolute-path escapes are rejected, never followed.
func TestListRejectsTraversal(t *testing.T) {
	s, _ := newTestServer(t)
	// ".." escapes the root outright → 400.
	if rr, _ := doList(t, s, "..", "0"); rr.Code != http.StatusBadRequest {
		t.Errorf("`..` want 400, got %d", rr.Code)
	}
	if rr, _ := doList(t, s, "../..", "0"); rr.Code != http.StatusBadRequest {
		t.Errorf("`../..` want 400, got %d", rr.Code)
	}
	// An absolute path is treated as jail-relative, so "/etc/passwd" → "etc/passwd" → not found.
	if rr, _ := doList(t, s, "/etc", "0"); rr.Code != http.StatusNotFound {
		t.Errorf("`/etc` want 404 (jailed), got %d", rr.Code)
	}
}

// AC3: a symlink pointing outside the jail is rejected on access, not followed.
func TestSymlinkEscapeRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks unreliable on windows")
	}
	s, _ := newTestServer(t)
	// Read via the escaping symlink must be refused (400 escape), never leaking the secret.
	rr, _ := doRead(t, s, "path=outside.lnk")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("symlink read want 400 escape, got %d (%s)", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "SECRET") {
		t.Fatal("SECURITY: escaping symlink leaked out-of-jail content")
	}
}

// AC4: file read returns bytes, honours byte-range offset/length, and flags text vs binary.
func TestReadFileRangeAndType(t *testing.T) {
	s, _ := newTestServer(t)

	rr, fc := doRead(t, s, "path=a.txt")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	if string(fc.Data) != "hello world" || fc.Size != 11 || fc.ContentType != "text" {
		t.Fatalf("full read wrong: %+v data=%q", fc, fc.Data)
	}

	// Byte-range: offset 6, length 5 → "world".
	_, fc = doRead(t, s, "path=a.txt&offset=6&length=5")
	if string(fc.Data) != "world" || fc.Offset != 6 || fc.Length != 5 {
		t.Fatalf("range read wrong: %+v data=%q", fc, fc.Data)
	}

	// Binary detection: a NUL byte flags binary.
	_, fc = doRead(t, s, "path=bin.dat")
	if fc.ContentType != "binary" {
		t.Fatalf("bin.dat want binary, got %q", fc.ContentType)
	}
}

// AC4: reads are byte-capped so no unbounded stream reaches the apiserver.
func TestReadByteCap(t *testing.T) {
	s, root := newTestServer(t)
	s.maxBytes = 8 // shrink cap for the test
	mustWrite(t, filepath.Join(root, "big.txt"), []byte("0123456789ABCDEF"))
	_, fc := doRead(t, s, "path=big.txt")
	if fc.Length != 8 || string(fc.Data) != "01234567" {
		t.Fatalf("cap not enforced: len=%d data=%q", fc.Length, fc.Data)
	}
	if fc.Size != 16 {
		t.Fatalf("Size should report full file: %d", fc.Size)
	}
	// The next page picks up where the cap left off.
	_, fc = doRead(t, s, "path=big.txt&offset=8")
	if string(fc.Data) != "89ABCDEF" {
		t.Fatalf("second range wrong: %q", fc.Data)
	}
}

// AC3/AC4: type errors — reading a dir and listing a file are rejected; missing path → 400.
func TestTypeAndMissingPathErrors(t *testing.T) {
	s, _ := newTestServer(t)
	if rr, _ := doRead(t, s, "path=sub"); rr.Code != http.StatusBadRequest {
		t.Errorf("read dir want 400, got %d", rr.Code)
	}
	if rr, _ := doList(t, s, "a.txt", "0"); rr.Code != http.StatusBadRequest {
		t.Errorf("list file want 400, got %d", rr.Code)
	}
	if rr, _ := doRead(t, s, "path="); rr.Code != http.StatusBadRequest {
		t.Errorf("empty read path want 400, got %d", rr.Code)
	}
	if rr, _ := doRead(t, s, "path=nope.txt"); rr.Code != http.StatusNotFound {
		t.Errorf("missing file want 404, got %d", rr.Code)
	}
}

// AC3: pagination bounds a listing and advances NextPage.
func TestPagination(t *testing.T) {
	s, root := newTestServer(t)
	s.pageSize = 2
	dir := filepath.Join(root, "many")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"1", "2", "3", "4", "5"} {
		mustWrite(t, filepath.Join(dir, n), []byte("x"))
	}
	_, dl := doList(t, s, "many", "0")
	if len(dl.Entries) != 2 || dl.NextPage != 1 {
		t.Fatalf("page 0 wrong: n=%d next=%d", len(dl.Entries), dl.NextPage)
	}
	_, dl = doList(t, s, "many", "2")
	if len(dl.Entries) != 1 || dl.NextPage != 0 {
		t.Fatalf("last page wrong: n=%d next=%d", len(dl.Entries), dl.NextPage)
	}
}

// ISI-4076 (PR #359 F1): a huge page value must not overflow page*pageSize into a negative
// start and panic the entries slice; it clamps to the empty tail page instead.
func TestListPageOverflowClamp(t *testing.T) {
	s, _ := newTestServer(t)
	for _, page := range []string{"9223372036854775807", "4611686018427387904"} {
		rr, dl := doList(t, s, "sub", page)
		if rr.Code != http.StatusOK {
			t.Fatalf("page=%s: want 200, got %d", page, rr.Code)
		}
		if len(dl.Entries) != 0 || dl.NextPage != 0 {
			t.Fatalf("page=%s: want empty tail page, got n=%d next=%d", page, len(dl.Entries), dl.NextPage)
		}
	}
}

// AC5-adjacent: only GET list/read/healthz exist — there is no mutating verb surface.
func TestNoMutatingVerbs(t *testing.T) {
	s, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	// A non-existent verb path 404s; /list under POST still routes to the read-only handler
	// (which ignores the method) — the point is no delete/write/exec route is registered.
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/delete?path=a.txt", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("no write/delete/exec verb should exist; /delete → %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz → %d", rr.Code)
	}
}

// ISI-4649: /stat returns mtime and size; without a .git checkout the git field is
// omitted (graceful fallback), and the route still answers 200.
func TestStatNoGitFallback(t *testing.T) {
	s, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/stat?path=a.txt", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("stat → %d (%s)", rr.Code, rr.Body.String())
	}
	var st FileStat
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode stat: %v", err)
	}
	if st.Name != "a.txt" || st.Type != "file" || st.Size != 11 {
		t.Errorf("unexpected stat: %+v", st)
	}
	if st.ModTime == "" {
		t.Error("modTime must be set")
	}
	if st.Git != nil {
		t.Errorf("no .git in jail: git must be nil, got %+v", st.Git)
	}
}

// ISI-4649: /stat rejects traversal and a missing path param like the other routes.
func TestStatRejectsTraversalAndMissingPath(t *testing.T) {
	s, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/stat?path=../etc", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("traversal → %d, want 400", rr.Code)
	}
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/stat", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("missing path → %d, want 400", rr.Code)
	}
}

// ISI-4649: when the jail IS a git checkout and a git binary is available, /stat
// returns the last-change commit for the path. Skipped without git.
func TestStatGitLastChange(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "tracked.txt"), []byte("v1"))
	runGit(t, root, "init")
	runGit(t, root, "add", "tracked.txt")
	runGit(t, root, "-c", "user.email=t@t", "-c", "user.name=tester", "commit", "-m", "add tracked")

	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/stat?path=tracked.txt", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("stat → %d (%s)", rr.Code, rr.Body.String())
	}
	var st FileStat
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode stat: %v", err)
	}
	if st.Git == nil {
		t.Fatal("git checkout: expected last-change metadata")
	}
	if len(st.Git.CommitHash) != 40 || st.Git.Author != "tester" || st.Git.Message != "add tracked" || st.Git.Timestamp == "" {
		t.Errorf("unexpected git change: %+v", st.Git)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

// New fails closed on a missing or non-directory root.
func TestNewFailsClosed(t *testing.T) {
	if _, err := New(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("New should fail on missing root")
	}
	f := filepath.Join(t.TempDir(), "afile")
	mustWrite(t, f, []byte("x"))
	if _, err := New(f); err == nil {
		t.Fatal("New should fail when root is a file")
	}
}

// dotfileServer builds a jail whose root is polluted with the HOME dotfiles + credential files the
// sandbox layout co-locates with the project checkout (KSQUAD_WORKDIR == HOME == /workspace). The
// legit project file a.txt stands in for the code the reader MUST keep serving.
func dotfileServer(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), []byte("project code"))
	mustWrite(t, filepath.Join(root, "secret.env"), []byte("API_KEY=leak"))
	mustWrite(t, filepath.Join(root, ".env"), []byte("TOKEN=leak"))
	mustWrite(t, filepath.Join(root, "id_rsa"), []byte("PRIVATE KEY leak"))
	mustWrite(t, filepath.Join(root, "credentials"), []byte("aws creds leak"))
	mustWrite(t, filepath.Join(root, ".npmrc"), []byte("//registry/:_authToken=leak"))
	// A future-junk dotfile nobody enumerated — fail-closed must still hide it.
	mustWrite(t, filepath.Join(root, ".surprise-junk"), []byte("whatever leak"))
	for _, d := range []string{".ssh", ".config", ".local"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(root, ".ssh", "id_rsa"), []byte("nested key leak"))
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// ISI-4785 locked-down v1: dotfiles + the hard sensitive set are structurally ABSENT from a listing,
// while legitimate project files remain.
func TestListHidesDotfilesAndSensitive(t *testing.T) {
	s := dotfileServer(t)
	_, dl := doList(t, s, "", "0")
	for _, e := range dl.Entries {
		if e.Name != "a.txt" {
			t.Errorf("leaked entry in listing: %q", e.Name)
		}
	}
	if len(dl.Entries) != 1 || dl.Entries[0].Name != "a.txt" {
		t.Fatalf("want only a.txt visible, got %+v", dl.Entries)
	}
}

// ISI-4785: /read on any hidden or sensitive path is 404 (not a hidden row), at the root AND nested,
// and never leaks content.
func TestReadRejectsBlockedPaths(t *testing.T) {
	s := dotfileServer(t)
	for _, p := range []string{".env", "secret.env", "id_rsa", "credentials", ".npmrc", ".surprise-junk", ".ssh/id_rsa", ".config/anything"} {
		rr, _ := doRead(t, s, "path="+p)
		if rr.Code != http.StatusNotFound {
			t.Errorf("read %q want 404, got %d", p, rr.Code)
		}
		if strings.Contains(rr.Body.String(), "leak") {
			t.Errorf("SECURITY: read %q leaked content: %s", p, rr.Body.String())
		}
	}
	// The legit project file is unaffected — read-only semantics unchanged.
	rr, fc := doRead(t, s, "path=a.txt")
	if rr.Code != http.StatusOK || string(fc.Data) != "project code" {
		t.Fatalf("legit read broken: code=%d data=%q", rr.Code, fc.Data)
	}
}

// ISI-4785: /stat on any hidden or sensitive path is 404, at the root AND nested.
func TestStatRejectsBlockedPaths(t *testing.T) {
	s := dotfileServer(t)
	for _, p := range []string{".env", "secret.env", "id_rsa", "credentials", ".ssh", ".ssh/id_rsa", ".config"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/stat?path="+p, nil)
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Errorf("stat %q want 404, got %d", p, rr.Code)
		}
	}
	// Legit project file still stats.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stat?path=a.txt", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("legit stat broken: code=%d", rr.Code)
	}
}

// ISI-4785: /list of a blocked directory itself is 404 — you cannot enumerate inside .ssh/.config.
func TestListBlockedDirIsNotFound(t *testing.T) {
	s := dotfileServer(t)
	for _, p := range []string{".ssh", ".config", ".local"} {
		if rr, _ := doList(t, s, p, "0"); rr.Code != http.StatusNotFound {
			t.Errorf("list %q want 404, got %d", p, rr.Code)
		}
	}
}

// ISI-4785: case-insensitivity — an upper/mixed-case sensitive name is still blocked.
func TestBlockedNameCaseInsensitive(t *testing.T) {
	for _, n := range []string{"PROD.ENV", "Id_Rsa", "Credentials", ".SSH"} {
		if !blockedName(n) {
			t.Errorf("blockedName(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"a.txt", "main.go", "README.md", "envfile", "credentials.txt"} {
		if blockedName(n) {
			t.Errorf("blockedName(%q) = true, want false (legit project file)", n)
		}
	}
}

// ISI-4785 (CR hand-back from ISI-4786): an IN-JAIL symlink aliasing blocked credential material must
// be 404 on /read and /stat. The requested name (key.txt / notes) passes the front-door pathBlocked
// check, but the EvalSymlinks-resolved real path lands on id_rsa (or .ssh/id_rsa) and must be caught by
// the post-resolution re-check in jail(). Before the fix this served 200 + full PRIVATE-KEY content.
func TestInJailSymlinkToBlockedIsNotFound(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks unreliable on windows")
	}
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), []byte("project code"))
	mustWrite(t, filepath.Join(root, "id_rsa"), []byte("PRIVATE KEY leak"))
	if err := os.Mkdir(filepath.Join(root, ".ssh"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, ".ssh", "id_rsa"), []byte("nested PRIVATE KEY leak"))
	// key.txt -> id_rsa (blocked hard-set name); notes -> .ssh/id_rsa (blocked dot-segment). Both stay
	// inside the jail, so the within() re-check alone would pass them.
	if err := os.Symlink("id_rsa", filepath.Join(root, "key.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(".ssh", "id_rsa"), filepath.Join(root, "notes")); err != nil {
		t.Fatal(err)
	}
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, p := range []string{"key.txt", "notes"} {
		rr, _ := doRead(t, s, "path="+p)
		if rr.Code != http.StatusNotFound {
			t.Errorf("read symlink %q want 404, got %d", p, rr.Code)
		}
		if strings.Contains(rr.Body.String(), "leak") {
			t.Errorf("SECURITY: in-jail symlink %q leaked blocked content: %s", p, rr.Body.String())
		}
		sr := httptest.NewRecorder()
		s.Handler().ServeHTTP(sr, httptest.NewRequest(http.MethodGet, "/stat?path="+p, nil))
		if sr.Code != http.StatusNotFound {
			t.Errorf("stat symlink %q want 404, got %d", p, sr.Code)
		}
	}
	// Sanity: a legit file is still readable — the re-check didn't over-block.
	rr, fc := doRead(t, s, "path=a.txt")
	if rr.Code != http.StatusOK || string(fc.Data) != "project code" {
		t.Fatalf("legit read broken: code=%d data=%q", rr.Code, fc.Data)
	}
}
