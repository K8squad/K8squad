package buildbrowser

import (
	"context"
	"strings"
	"testing"
)

// TestGitReader_Snapshot captures the fixture Run and asserts the content-addressed bundle + the
// git-native summary (base/runRef/commit + changed-file count).
func TestGitReader_Snapshot(t *testing.T) {
	requireGit(t)
	m := setupRepo(t)
	r := NewGitReader()

	snap, err := r.Snapshot(context.Background(), m)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Summary.Truncated {
		t.Fatalf("fixture bundle unexpectedly truncated")
	}
	if len(snap.Bundle) == 0 {
		t.Fatalf("expected non-empty bundle")
	}
	if snap.URI == "" || !strings.HasPrefix(snap.URI, "sha256:") {
		t.Errorf("uri = %q, want sha256: prefix", snap.URI)
	}
	if snap.URI != "sha256:"+snap.SHA256 {
		t.Errorf("uri %q inconsistent with sha256 %q", snap.URI, snap.SHA256)
	}
	if snap.Summary.RunRef != "run" {
		t.Errorf("runRef = %q, want run", snap.Summary.RunRef)
	}
	if snap.Summary.Base == "" || snap.Summary.Commit == "" {
		t.Errorf("base/commit shas empty: %+v", snap.Summary)
	}
	if snap.Summary.Base == snap.Summary.Commit {
		t.Errorf("base and run commit shas should differ (base=%s commit=%s)", snap.Summary.Base, snap.Summary.Commit)
	}
	// base..run changes a.txt + adds newdir/b.txt + adds huge.txt = 3 files.
	if snap.Summary.FileCount != 3 {
		t.Errorf("fileCount = %d, want 3", snap.Summary.FileCount)
	}
	if snap.Summary.TotalAdditions <= 0 {
		t.Errorf("totalAdditions = %d, want > 0", snap.Summary.TotalAdditions)
	}
}

// TestGitReader_Snapshot_Truncated drives the byte-less degradation: a bundle over the cap yields a
// truncated (nil-bytes) snapshot whose summary still carries the git-native meta.
func TestGitReader_Snapshot_Truncated(t *testing.T) {
	requireGit(t)
	m := setupRepo(t)
	r := NewGitReader()
	r.SnapshotCap = 1024 // fixture bundle (huge.txt = 2.5 MiB) far exceeds this

	snap, err := r.Snapshot(context.Background(), m)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !snap.Summary.Truncated {
		t.Fatalf("expected truncated snapshot at 1 KiB cap")
	}
	if len(snap.Bundle) != 0 || snap.URI != "" || snap.SHA256 != "" {
		t.Errorf("truncated snapshot must carry no servable bytes: bundle=%d uri=%q", len(snap.Bundle), snap.URI)
	}
	if snap.Summary.FileCount != 3 {
		t.Errorf("truncated snapshot still summarizes: fileCount = %d, want 3", snap.Summary.FileCount)
	}
}

// TestGitReader_Snapshot_MissingRef hides a bad ref as ErrNotFound (existence-hiding, consistent with
// the live reader).
func TestGitReader_Snapshot_MissingRef(t *testing.T) {
	requireGit(t)
	m := setupRepo(t)
	m.HeadRef = "does-not-exist"
	r := NewGitReader()

	if _, err := r.Snapshot(context.Background(), m); err != ErrNotFound {
		t.Fatalf("Snapshot(missing ref) err = %v, want ErrNotFound", err)
	}
}

// TestSnapshotReader_ServesFromBundle proves the 8.7c serve half: a completed Run's tree/diff/file/meta
// are answered off the captured bundle, with Meta.Live=false.
func TestSnapshotReader_ServesFromBundle(t *testing.T) {
	requireGit(t)
	m := setupRepo(t)
	r := NewGitReader()

	snap, err := r.Snapshot(context.Background(), m)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	sr, err := NewSnapshotReader(snap.Bundle)
	if err != nil {
		t.Fatalf("NewSnapshotReader: %v", err)
	}
	defer func() {
		if cerr := sr.Close(); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	}()

	ctx := context.Background()

	// tree(run) sees the run's files including the added one.
	tree, err := sr.Tree(ctx, m, "run")
	if err != nil {
		t.Fatalf("snapshot Tree: %v", err)
	}
	var sawAdded bool
	for _, e := range tree.Entries {
		if e.Path == "newdir/b.txt" {
			sawAdded = true
		}
	}
	if !sawAdded {
		t.Errorf("snapshot tree missing newdir/b.txt: %+v", tree.Entries)
	}

	// file(run, a.txt) serves the run's content, not the base's.
	f, err := sr.File(ctx, m, "run", "a.txt")
	if err != nil {
		t.Fatalf("snapshot File: %v", err)
	}
	if got := string(f.Content); got != "run contents changed\n" {
		t.Errorf("snapshot a.txt = %q, want run contents", got)
	}

	// diff base..run is non-empty.
	d, err := sr.Diff(ctx, m)
	if err != nil {
		t.Fatalf("snapshot Diff: %v", err)
	}
	if !strings.Contains(d.Patch, "a.txt") {
		t.Errorf("snapshot diff missing a.txt: %q", d.Patch)
	}

	// meta is served live:false — this is a completed Run served from the snapshot.
	meta, err := sr.Meta(ctx, m)
	if err != nil {
		t.Fatalf("snapshot Meta: %v", err)
	}
	if meta.Live {
		t.Errorf("snapshot Meta.Live = true, want false")
	}
	if meta.ChangedFiles != 3 {
		t.Errorf("snapshot Meta.ChangedFiles = %d, want 3", meta.ChangedFiles)
	}
}

// TestSnapshotReader_EmptyBundle rejects a byte-less (truncated) snapshot — there is nothing to serve.
func TestSnapshotReader_EmptyBundle(t *testing.T) {
	if _, err := NewSnapshotReader(nil); err != ErrNotFound {
		t.Fatalf("NewSnapshotReader(nil) err = %v, want ErrNotFound", err)
	}
}

// TestGitReader_Meta_Live confirms the live worktree reader stamps Live=true (the counterpart to the
// snapshot reader's false).
func TestGitReader_Meta_Live(t *testing.T) {
	requireGit(t)
	m := setupRepo(t)
	meta, err := NewGitReader().Meta(context.Background(), m)
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if !meta.Live {
		t.Errorf("live GitReader Meta.Live = false, want true")
	}
}

func TestParseNumstat(t *testing.T) {
	in := []byte("1\t0\ta.txt\n3\t2\tnewdir/b.txt\n-\t-\tbin.dat\n")
	files, adds, dels := parseNumstat(in)
	if files != 3 {
		t.Errorf("files = %d, want 3", files)
	}
	if adds != 4 {
		t.Errorf("adds = %d, want 4", adds)
	}
	if dels != 2 {
		t.Errorf("dels = %d, want 2", dels)
	}
}
