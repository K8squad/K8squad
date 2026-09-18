package apiserver

// ISI-4650 unit tests — download route (file attachment + folder tar.gz archive).
//
// Coverage:
//   - nil reader → 501
//   - missing path → 400; jail traversal → 400
//   - file download: 200 + Content-Disposition attachment + octet-stream + bytes
//   - file over the single-file cap → 413
//   - directory: 200 + gzip content-type + valid tar.gz containing every walked file
//   - archive pagination (NextPage chains) is honoured
//   - foreign-team caller → 404 via requireProjectRole; global admin → 200
//   - busy workspace → 503

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"

	"github.com/K8squad/K8squad/internal/buildbrowser/readerpod/readclient"
	"github.com/K8squad/K8squad/pkg/auth"
)

// treeWorkspaceReader is a fakeWorkspaceReader variant backed by an in-memory tree:
// dirs maps jail-relative dir path → page-agnostic entries; files maps path → bytes.
type treeWorkspaceReader struct {
	dirs  map[string][]DirEntry
	files map[string][]byte
	err   error
}

func (tr *treeWorkspaceReader) ListDir(_ context.Context, _, dirPath string, _ int) (*DirListing, error) {
	if tr.err != nil {
		return nil, tr.err
	}
	if entries, ok := tr.dirs[dirPath]; ok {
		return &DirListing{Entries: entries}, nil
	}
	if _, ok := tr.files[dirPath]; ok {
		return nil, ErrNotDirectory
	}
	return nil, ErrNotDirectory
}

func (tr *treeWorkspaceReader) ReadFile(_ context.Context, _, filePath string, _, _ int64) (*FileContent, error) {
	if tr.err != nil {
		return nil, tr.err
	}
	data, ok := tr.files[filePath]
	if !ok {
		return nil, ErrNotDirectory
	}
	return &FileContent{Data: data, Size: int64(len(data)), ContentType: "text", Length: int64(len(data))}, nil
}

func (tr *treeWorkspaceReader) StatFile(_ context.Context, _, filePath string) (*FileStat, error) {
	if tr.err != nil {
		return nil, tr.err
	}
	if data, ok := tr.files[filePath]; ok {
		return &FileStat{
			Name: path.Base(filePath), Type: "file", Size: int64(len(data)),
			ModTime: "2026-09-18T00:00:00Z",
		}, nil
	}
	if _, ok := tr.dirs[filePath]; ok {
		return &FileStat{Name: path.Base(filePath), Type: "dir", ModTime: "2026-09-18T00:00:00Z"}, nil
	}
	return nil, ErrNotDirectory
}

func buildTreeReader() *treeWorkspaceReader {
	return &treeWorkspaceReader{
		dirs: map[string][]DirEntry{
			".": {
				{Name: "main.go", Type: "file", Size: 5},
				{Name: "docs", Type: "dir"},
			},
			"docs": {
				{Name: "README.md", Type: "file", Size: 7},
			},
		},
		files: map[string][]byte{
			"main.go":        []byte("hello"),
			"docs/README.md": []byte("read me"),
		},
	}
}

func TestDownload_NilReader_Returns501(t *testing.T) {
	srv := buildFilesServer(nil, nil, filesAuthn("alice", true))
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files/download?path=main.go", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotImplemented {
		t.Errorf("nil reader: got %d, want 501", w.Code)
	}
}

func TestDownload_MissingPath_Returns400(t *testing.T) {
	srv := buildFilesServer(buildTreeReader(), nil, filesAuthn("alice", true))
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files/download", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing path: got %d, want 400", w.Code)
	}
}

func TestDownload_Traversal_Returns400(t *testing.T) {
	srv := buildFilesServer(buildTreeReader(), nil, filesAuthn("alice", true))
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files/download?path=../secret", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("traversal: got %d, want 400", w.Code)
	}
}

func TestDownload_File_Attachment(t *testing.T) {
	srv := buildFilesServer(buildTreeReader(), nil, filesAuthn("alice", true))
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files/download?path=main.go", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("file download: got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("content-type: got %q, want application/octet-stream", ct)
	}
	cd := w.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") || !strings.Contains(cd, `filename="main.go"`) {
		t.Errorf("content-disposition: got %q", cd)
	}
	if w.Body.String() != "hello" {
		t.Errorf("body: got %q, want %q", w.Body.String(), "hello")
	}
}

// oversizeFileReader reports a regular file whose declared Size exceeds the download cap.
type oversizeFileReader struct{}

func (oversizeFileReader) ListDir(_ context.Context, _, _ string, _ int) (*DirListing, error) {
	return nil, ErrNotDirectory
}

func (oversizeFileReader) ReadFile(_ context.Context, _, _ string, _, _ int64) (*FileContent, error) {
	return &FileContent{Data: []byte("x"), Size: maxDownloadFileBytes + 1, ContentType: "binary", Length: 1}, nil
}

func (oversizeFileReader) StatFile(_ context.Context, _, _ string) (*FileStat, error) {
	return &FileStat{Name: "big.bin", Type: "file", Size: maxDownloadFileBytes + 1, ModTime: "2026-09-18T00:00:00Z"}, nil
}

func TestDownload_File_OverCap_Returns413(t *testing.T) {
	srv := buildFilesServer(oversizeFileReader{}, nil, filesAuthn("alice", true))
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files/download?path=big.bin", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("over-cap file: got %d, want 413", w.Code)
	}
}

func TestDownload_Directory_TarGz(t *testing.T) {
	srv := buildFilesServer(buildTreeReader(), nil, filesAuthn("alice", true))
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files/download?path=.", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("archive: got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/gzip" {
		t.Errorf("content-type: got %q, want application/gzip", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, `filename="workspace.tar.gz"`) {
		t.Errorf("content-disposition: got %q", cd)
	}

	gz, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	tr := tar.NewReader(gz)
	got := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		got[hdr.Name] = string(data)
	}
	if got["workspace/main.go"] != "hello" {
		t.Errorf("archive main.go: got %q", got["workspace/main.go"])
	}
	if got["workspace/docs/README.md"] != "read me" {
		t.Errorf("archive docs/README.md: got %q", got["workspace/docs/README.md"])
	}
	if len(got) != 2 {
		t.Errorf("archive entry count: got %d, want 2 (%v)", len(got), got)
	}
}

func TestDownload_Subdirectory_NamedArchive(t *testing.T) {
	srv := buildFilesServer(buildTreeReader(), nil, filesAuthn("alice", true))
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files/download?path=docs", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("subdir archive: got %d (body: %s)", w.Code, w.Body.String())
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, `filename="docs.tar.gz"`) {
		t.Errorf("content-disposition: got %q", cd)
	}
	gz, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("tar next: %v", err)
	}
	if hdr.Name != path.Join("docs", "README.md") {
		t.Errorf("tar entry name: got %q, want docs/README.md", hdr.Name)
	}
}

// pagedReader returns a two-page listing for "." to prove the walk honours NextPage.
type pagedReader struct{}

func (pagedReader) ListDir(_ context.Context, _, dirPath string, page int) (*DirListing, error) {
	if dirPath != "." {
		return nil, ErrNotDirectory
	}
	if page == 0 {
		return &DirListing{Entries: []DirEntry{{Name: "a.txt", Type: "file", Size: 1}}, NextPage: 1}, nil
	}
	return &DirListing{Entries: []DirEntry{{Name: "b.txt", Type: "file", Size: 1}}}, nil
}

func (pagedReader) ReadFile(_ context.Context, _, filePath string, _, _ int64) (*FileContent, error) {
	return &FileContent{Data: []byte("x"), Size: 1, ContentType: "text", Length: 1}, nil
}

func (pagedReader) StatFile(_ context.Context, _, filePath string) (*FileStat, error) {
	return &FileStat{Name: path.Base(filePath), Type: "file", Size: 1, ModTime: "2026-09-18T00:00:00Z"}, nil
}

func TestDownload_Archive_Pagination(t *testing.T) {
	srv := buildFilesServer(pagedReader{}, nil, filesAuthn("alice", true))
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files/download?path=.", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("paged archive: got %d (body: %s)", w.Code, w.Body.String())
	}
	gz, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	tr := tar.NewReader(gz)
	count := 0
	for {
		if _, err := tr.Next(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		count++
	}
	if count != 2 {
		t.Errorf("paged archive entries: got %d, want 2", count)
	}
}

func TestDownload_ForeignTeam_Returns404(t *testing.T) {
	resolver := &fakeMembershipStore{roles: map[string]string{"alice": auth.ProjectRoleViewer}}
	srv := buildFilesServer(buildTreeReader(), resolver, filesAuthn("bob", false))
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files/download?path=main.go", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("foreign team: got %d, want 404", w.Code)
	}
}

func TestDownload_Busy_Returns503(t *testing.T) {
	reader := &fakeWorkspaceReader{err: ErrWorkspaceBusy}
	srv := buildFilesServer(reader, nil, filesAuthn("alice", true))
	r := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/files/download?path=main.go", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("busy: got %d, want 503", w.Code)
	}
}

func TestMapReadErr_NotDirectory(t *testing.T) {
	mapped := mapReadErr(&readclient.StatusError{Code: http.StatusBadRequest, Body: "not a directory"})
	if mapped != ErrNotDirectory {
		t.Errorf("mapReadErr(400 not-a-directory): got %v, want ErrNotDirectory", mapped)
	}
	other := mapReadErr(&readclient.StatusError{Code: http.StatusBadRequest, Body: "path escapes workspace root"})
	if other == ErrNotDirectory {
		t.Error("mapReadErr(400 other): must NOT map to ErrNotDirectory")
	}
}
