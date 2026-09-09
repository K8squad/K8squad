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
