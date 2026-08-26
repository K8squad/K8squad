package buildbrowser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// ── test repo fixture ────────────────────────────────────────────────────────────────────────────

// git runs a setup git command (identity injected inline) and fails the test on error. Setup is
// distinct from the read path: the reader neutralizes config, so commits here supply their own.
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir,
		"-c", "user.email=test@k8squad.local", "-c", "user.name=test",
		"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=base"}, args...)
	cmd := exec.Command("git", full...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
}

func write(t *testing.T, dir, rel string, data []byte) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// setupRepo builds a repo with a `base` branch and a `run` branch that modifies a.txt, adds
// newdir/b.txt, and adds huge.txt (2.5 MiB — big enough to trip both the file and diff caps).
func setupRepo(t *testing.T) RunMeta {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "base")

	write(t, dir, "a.txt", []byte("base contents\n"))
	write(t, dir, "keep.txt", []byte("unchanged\n"))
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "base")

	git(t, dir, "checkout", "-q", "-b", "run")
	write(t, dir, "a.txt", []byte("run contents changed\n"))
	write(t, dir, "newdir/b.txt", []byte("new file\n"))
	write(t, dir, "huge.txt", bytes.Repeat([]byte("x"), 2_500_000))
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "run")

	return RunMeta{
		RunID:     "run-1",
		TeamID:    uuid.New(),
		Principal: "user:alice",
		RepoPath:  dir,
		HeadRef:   "run",
		BaseRef:   "base",
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// ── git read-model ───────────────────────────────────────────────────────────────────────────────

func TestGitReader_Tree(t *testing.T) {
	requireGit(t)
	m := setupRepo(t)
	r := NewGitReader()

	res, err := r.Tree(context.Background(), m, "run")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, e := range res.Entries {
		got[e.Path] = e.Size
	}
	for _, want := range []string{"a.txt", "keep.txt", "newdir/b.txt", "huge.txt"} {
		if _, ok := got[want]; !ok {
			t.Errorf("tree(run) missing %q; entries=%v", want, got)
		}
	}
	if got["huge.txt"] != 2_500_000 {
		t.Errorf("huge.txt size = %d, want 2500000", got["huge.txt"])
	}
	if res.Truncated {
		t.Error("small tree should not be truncated")
	}

	// base ref: huge.txt / newdir do not exist yet.
	base, err := r.Tree(context.Background(), m, "base")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range base.Entries {
		if e.Path == "huge.txt" || e.Path == "newdir/b.txt" {
			t.Errorf("base tree should not contain %q", e.Path)
		}
	}
}

func TestGitReader_TreeCap(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "base")
	for i := 0; i < MaxTreeEntries+50; i++ {
		write(t, dir, fmt.Sprintf("d%03d/f%d.txt", i%100, i), []byte("x"))
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "many")
	m := RunMeta{RepoPath: dir, HeadRef: "base", BaseRef: "base"}

	res, err := NewGitReader().Tree(context.Background(), m, "run")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != MaxTreeEntries {
		t.Errorf("capped entries = %d, want %d", len(res.Entries), MaxTreeEntries)
	}
	if !res.Truncated {
		t.Error("oversize tree must report Truncated=true")
	}
}

func TestGitReader_File(t *testing.T) {
	requireGit(t)
	m := setupRepo(t)
	r := NewGitReader()

	run, err := r.File(context.Background(), m, "run", "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(run.Content) != "run contents changed\n" {
		t.Errorf("file(run,a.txt) = %q", run.Content)
	}
	if run.Truncated {
		t.Error("small file should not be truncated")
	}

	base, err := r.File(context.Background(), m, "base", "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(base.Content) != "base contents\n" {
		t.Errorf("file(base,a.txt) = %q", base.Content)
	}
}

func TestGitReader_FileCap(t *testing.T) {
	requireGit(t)
	m := setupRepo(t)
	res, err := NewGitReader().File(context.Background(), m, "run", "huge.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != MaxFileBytes {
		t.Errorf("capped content len = %d, want %d", len(res.Content), MaxFileBytes)
	}
	if !res.Truncated {
		t.Error("oversize file must report Truncated=true")
	}
	if res.Size != 2_500_000 {
		t.Errorf("Size should be the FULL blob size, got %d", res.Size)
	}
}

func TestGitReader_FilePathSafety(t *testing.T) {
	requireGit(t)
	m := setupRepo(t)
	r := NewGitReader()
	cases := []struct {
		path string
		want error
	}{
		{"../../etc/passwd", ErrNotFound},
		{"/etc/passwd", ErrNotFound},
		{"nonexistent.txt", ErrNotFound},
		{"newdir/../../../etc/passwd", ErrNotFound},
		{"", ErrBadRequest},
	}
	for _, c := range cases {
		_, err := r.File(context.Background(), m, "run", c.path)
		if !errors.Is(err, c.want) {
			t.Errorf("File(%q) err = %v, want %v", c.path, err, c.want)
		}
	}
}

func TestGitReader_Diff(t *testing.T) {
	requireGit(t)
	m := setupRepo(t)
	// exclude the huge file so we get a small, assertable diff.
	m.HeadRef = "run"
	res, err := NewGitReader().Diff(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Patch, "a.txt") || !strings.Contains(res.Patch, "run contents changed") {
		t.Errorf("diff missing a.txt change:\n%s", res.Patch[:min(len(res.Patch), 400)])
	}
	// huge.txt added in run → the diff blows past the 2 MiB cap.
	if !res.Truncated {
		t.Error("diff containing huge.txt must report Truncated=true")
	}
	if len(res.Patch) > MaxDiffBytes {
		t.Errorf("patch len %d exceeds cap %d", len(res.Patch), MaxDiffBytes)
	}
}

func TestGitReader_Meta(t *testing.T) {
	requireGit(t)
	m := setupRepo(t)
	res, err := NewGitReader().Meta(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if res.RunID != "run-1" {
		t.Errorf("RunID = %q", res.RunID)
	}
	if len(res.Head) != 12 || len(res.Base) != 12 {
		t.Errorf("short shas: head=%q base=%q", res.Head, res.Base)
	}
	if res.Head == res.Base {
		t.Error("head and base shas should differ")
	}
	// a.txt modified + newdir/b.txt + huge.txt added = 3 changed files.
	if res.ChangedFiles != 3 {
		t.Errorf("ChangedFiles = %d, want 3", res.ChangedFiles)
	}
}

// TestGitReader_Meta_PrCiEcho covers the 8.7g header-strip seam: when the server-derived RunMeta
// carries PR/CI facts (from the Epic 11 SCM mirror), Meta echoes them; when it does not, the fields
// are empty and JSON-drop (omitempty) so the console header strip is absent — git-only degradation.
func TestGitReader_Meta_PrCiEcho(t *testing.T) {
	requireGit(t)

	// Populated: prUrl/ciStatus flow through to MetaResult and marshal into the JSON.
	m := setupRepo(t)
	m.PrURL = "https://github.com/K8squad/K8squad/pull/140"
	m.CIStatus = "passing"
	res, err := NewGitReader().Meta(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if res.PrURL != m.PrURL || res.CIStatus != m.CIStatus {
		t.Errorf("PR/CI echo = %q/%q, want %q/%q", res.PrURL, res.CIStatus, m.PrURL, m.CIStatus)
	}
	j, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(j, []byte(`"prUrl":"https://github.com/K8squad/K8squad/pull/140"`)) ||
		!bytes.Contains(j, []byte(`"ciStatus":"passing"`)) {
		t.Errorf("populated meta JSON missing PR/CI fields: %s", j)
	}

	// Absent (no SCM sync): omitempty drops both keys, so the strip renders nothing.
	bare := setupRepo(t)
	resBare, err := NewGitReader().Meta(context.Background(), bare)
	if err != nil {
		t.Fatal(err)
	}
	jb, err := json.Marshal(resBare)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(jb, []byte("prUrl")) || bytes.Contains(jb, []byte("ciStatus")) {
		t.Errorf("bare meta JSON should omit PR/CI fields (git-only degradation): %s", jb)
	}
}

func TestGitReader_BadRef(t *testing.T) {
	requireGit(t)
	m := setupRepo(t)
	if _, err := NewGitReader().Tree(context.Background(), m, "bogus"); !errors.Is(err, ErrBadRequest) {
		t.Errorf("Tree(bogus ref) err = %v, want ErrBadRequest", err)
	}
	if _, err := NewGitReader().File(context.Background(), m, "sideways", "a.txt"); !errors.Is(err, ErrBadRequest) {
		t.Errorf("File(bogus ref) err = %v, want ErrBadRequest", err)
	}
}

// ── 8.7d scope gate ──────────────────────────────────────────────────────────────────────────────

// fakeReader records that a read happened and returns a marker, so gate tests need no git.
type fakeReader struct{ called bool }

func (f *fakeReader) Tree(context.Context, RunMeta, string) (*TreeResult, error) {
	f.called = true
	return &TreeResult{Ref: "run"}, nil
}
func (f *fakeReader) Diff(context.Context, RunMeta) (*DiffResult, error) {
	f.called = true
	return &DiffResult{}, nil
}
func (f *fakeReader) File(context.Context, RunMeta, string, string) (*FileResult, error) {
	f.called = true
	return &FileResult{}, nil
}
func (f *fakeReader) Meta(context.Context, RunMeta) (*MetaResult, error) {
	f.called = true
	return &MetaResult{}, nil
}

func TestScopeGate(t *testing.T) {
	teamA, teamB := uuid.New(), uuid.New()
	src := NewStaticRunSource(map[string]RunMeta{
		"run-1": {RunID: "run-1", TeamID: teamA, Principal: "user:alice", RepoPath: "/x", HeadRef: "run", BaseRef: "base"},
	})

	cases := []struct {
		name    string
		caller  Caller
		runID   string
		allowed bool
	}{
		{"owner same team", Caller{Principal: "user:alice", TeamID: teamA}, "run-1", true},
		{"same-team non-owner hidden", Caller{Principal: "user:bob", TeamID: teamA}, "run-1", false},
		{"cross-team hidden", Caller{Principal: "user:alice", TeamID: teamB}, "run-1", false},
		{"same-team admin allowed", Caller{Principal: "user:bob", TeamID: teamA, IsAdmin: true}, "run-1", true},
		{"cross-team admin still hidden", Caller{Principal: "user:x", TeamID: teamB, IsAdmin: true}, "run-1", false},
		{"unknown run hidden", Caller{Principal: "user:alice", TeamID: teamA}, "run-nope", false},
		{"empty runid hidden", Caller{Principal: "user:alice", TeamID: teamA}, "", false},
		{"nil team hidden", Caller{Principal: "user:alice", TeamID: uuid.Nil}, "run-1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fr := &fakeReader{}
			svc := NewService(src, fr)
			_, err := svc.Tree(context.Background(), c.caller, c.runID, "run")
			if c.allowed {
				if err != nil {
					t.Fatalf("expected allow, got %v", err)
				}
				if !fr.called {
					t.Error("reader must be called on allow")
				}
			} else {
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("expected ErrNotFound (existence-hiding), got %v", err)
				}
				if fr.called {
					t.Error("reader MUST NOT be called on deny — authz precedes the read")
				}
			}
		})
	}
}
