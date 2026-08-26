package buildbrowser

// snapshot.go is the 8.7c (ISI-2903) build-snapshot: at Collecting the reconcile machine captures a
// Run's build git-natively so a COMPLETED Run — whose pod (and live worktree) is gone — still serves
// tree + diffs + changed-file code with live:false.
//
// Two halves live here:
//
//  1. GitReader.Snapshot CAPTURES. It bundles the run + base refs (read-only — never writes a ref
//     into the source repo), content-addresses the bundle (uri = "sha256:<hex>"), and computes the
//     summary meta the console header + degradation path read without re-hydrating the bundle. The
//     coord layer (pkg/coord ProdEffects) persists that into a kind="build-snapshot" coord.artifact
//     row (0010_build_snapshot.sql), fence-guarded and re-entry-idempotent.
//
//  2. SnapshotReader SERVES. It materializes a captured bundle into a throwaway bare clone and
//     delegates to a GitReader, so the same Tree/Diff/File/Meta the live path answered off the
//     worktree are answered off the snapshot — with MetaResult.Live=false.
//
// v1 = snapshot-only (per ISI-2273): a bundle larger than MaxSnapshotBytes is captured as a
// byte-less TRUNCATED snapshot — a legible "no build view" signal — never a silent 404.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// MaxSnapshotBytes caps the captured bundle at 32 MiB. A Run whose run..base bundle exceeds this is
// recorded as a truncated (byte-less) snapshot rather than streaming an unbounded blob through the
// collector — the read side surfaces that as an explicit "unavailable" build view, not a bare 404.
const MaxSnapshotBytes = 32 * 1024 * 1024

// SnapshotSummary is the git-native meta the console header and the "no build view" degradation path
// read WITHOUT re-hydrating the bundle. It marshals to the coord.artifact.meta jsonb column (0010).
type SnapshotSummary struct {
	Base           string `json:"base"`           // base commit short sha
	RunRef         string `json:"runRef"`         // the run ref (worktree branch / HEAD name)
	Commit         string `json:"commit"`         // run head commit short sha
	FileCount      int    `json:"fileCount"`      // changed files, base..run
	TotalAdditions int    `json:"totalAdditions"` // summed added lines (binary files contribute 0)
	TotalDeletions int    `json:"totalDeletions"` // summed deleted lines
	Truncated      bool   `json:"truncated"`      // bundle exceeded MaxSnapshotBytes → no servable bytes
}

// Snapshot is the content-addressed capture of a Run's build at Collecting. Bundle is a self-contained
// `git bundle` of the run + base refs (nil when Summary.Truncated); URI/SHA256 content-address it;
// Summary is persisted alongside as the artifact meta.
type Snapshot struct {
	Bundle  []byte          // the git bundle bytes (nil when truncated)
	SHA256  string          // hex sha256 of Bundle ("" when truncated)
	URI     string          // "sha256:<hex>" ("" when truncated)
	Summary SnapshotSummary // git-native meta (always populated)
}

// Snapshot captures the Run's build at m.HeadRef against m.BaseRef. It is READ-ONLY on the source
// repo: the bundle is created from the refs' existing names, never by writing a temp ref. A missing
// ref collapses to ErrNotFound (existence-hiding, consistent with the live reader).
func (g *GitReader) Snapshot(ctx context.Context, m RunMeta) (*Snapshot, error) {
	headSha, _, err := g.capture(ctx, m.RepoPath, 64, "rev-parse", "--short=12", m.HeadRef+"^{commit}")
	if err != nil {
		return nil, ErrNotFound
	}
	baseSha, _, err := g.capture(ctx, m.RepoPath, 64, "rev-parse", "--short=12", m.BaseRef+"^{commit}")
	if err != nil {
		return nil, ErrNotFound
	}

	summary := SnapshotSummary{
		Base:   strings.TrimSpace(string(baseSha)),
		RunRef: m.HeadRef,
		Commit: strings.TrimSpace(string(headSha)),
	}

	// --numstat is line-oriented: "<add>\t<del>\t<path>"; a binary file is "-\t-\t<path>".
	numstat, _, err := g.capture(ctx, m.RepoPath, MaxDiffBytes, "diff", "--numstat", m.BaseRef, m.HeadRef, "--")
	if err != nil {
		return nil, err
	}
	summary.FileCount, summary.TotalAdditions, summary.TotalDeletions = parseNumstat(numstat)

	// Bundle the run + base refs BY NAME (read-only). capture caps at the snapshot cap: a bundle
	// over the cap comes back truncated, and we discard the partial (invalid) bytes and record a
	// byte-less snapshot — a legible degradation, not a corrupt artifact.
	bundle, truncated, err := g.capture(ctx, m.RepoPath, g.snapshotCap(), "bundle", "create", "-", m.HeadRef, m.BaseRef)
	if err != nil {
		return nil, err
	}
	if truncated {
		summary.Truncated = true
		return &Snapshot{Summary: summary}, nil
	}

	sum := sha256.Sum256(bundle)
	sha := hex.EncodeToString(sum[:])
	return &Snapshot{
		Bundle:  bundle,
		SHA256:  sha,
		URI:     "sha256:" + sha,
		Summary: summary,
	}, nil
}

// snapshotCap is the effective bundle byte cap: the reader's SnapshotCap override, else the package
// default MaxSnapshotBytes.
func (g *GitReader) snapshotCap() int64 {
	if g.SnapshotCap > 0 {
		return g.SnapshotCap
	}
	return MaxSnapshotBytes
}

// parseNumstat folds `git diff --numstat` output into (files, additions, deletions). A binary file
// ("-\t-\t…") counts toward files but contributes 0 lines.
func parseNumstat(b []byte) (files, adds, dels int) {
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) < 3 {
			continue
		}
		files++
		if n, err := strconv.Atoi(fields[0]); err == nil {
			adds += n
		}
		if n, err := strconv.Atoi(fields[1]); err == nil {
			dels += n
		}
	}
	return files, adds, dels
}

// ── SnapshotReader ──────────────────────────────────────────────────────────────────────────────

// SnapshotReader serves a COMPLETED Run's build (live:false) from a captured Snapshot bundle. It
// materializes the bundle into a throwaway bare clone and delegates every read to a GitReader, so the
// tree/diff/file/meta the live path answered off the worktree are answered identically off the
// snapshot. Callers MUST Close it to remove the temp clone.
//
// Because the bundle records the run + base refs by their ORIGINAL names, the RunMeta's HeadRef/BaseRef
// resolve unchanged inside the clone — only RepoPath is swapped. Reads never touch the (gone) worktree.
type SnapshotReader struct {
	git      *GitReader
	repoPath string
	tmp      string
}

// NewSnapshotReader materializes bundle into a bare clone under a temp dir and returns a reader over
// it. A truncated (byte-less) snapshot has nothing to serve — the caller degrades to the meta signal
// instead of constructing a reader.
func NewSnapshotReader(bundle []byte) (*SnapshotReader, error) {
	if len(bundle) == 0 {
		return nil, ErrNotFound
	}
	tmp, err := os.MkdirTemp("", "ksquad-buildsnap-")
	if err != nil {
		return nil, fmt.Errorf("buildbrowser: snapshot tmpdir: %w", err)
	}
	bundlePath := filepath.Join(tmp, "snap.bundle")
	if werr := os.WriteFile(bundlePath, bundle, 0o600); werr != nil {
		_ = os.RemoveAll(tmp)
		return nil, fmt.Errorf("buildbrowser: write snapshot bundle: %w", werr)
	}
	repoPath := filepath.Join(tmp, "repo.git")
	// #nosec G204 -- fixed `git` binary; both operands are server-controlled temp paths, never
	// request-tainted, and the bundle content is a git object stream (not shell input).
	cmd := exec.Command("git", "clone", "--bare", "--quiet", bundlePath, repoPath)
	cmd.Env = gitEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if cerr := cmd.Run(); cerr != nil {
		_ = os.RemoveAll(tmp)
		return nil, fmt.Errorf("buildbrowser: clone snapshot bundle: %w: %s", cerr, strings.TrimSpace(stderr.String()))
	}
	return &SnapshotReader{git: NewGitReader(), repoPath: repoPath, tmp: tmp}, nil
}

// Close removes the materialized clone. It is safe to call more than once.
func (r *SnapshotReader) Close() error {
	if r.tmp == "" {
		return nil
	}
	err := os.RemoveAll(r.tmp)
	r.tmp = ""
	return err
}

// at returns m re-pointed at the materialized clone; HeadRef/BaseRef are unchanged because the bundle
// preserved their names.
func (r *SnapshotReader) at(m RunMeta) RunMeta {
	m.RepoPath = r.repoPath
	return m
}

// Tree serves the snapshot's tree listing.
func (r *SnapshotReader) Tree(ctx context.Context, m RunMeta, ref string) (*TreeResult, error) {
	return r.git.Tree(ctx, r.at(m), ref)
}

// Diff serves the snapshot's base..run diff.
func (r *SnapshotReader) Diff(ctx context.Context, m RunMeta) (*DiffResult, error) {
	return r.git.Diff(ctx, r.at(m))
}

// File serves a file from the snapshot at ref.
func (r *SnapshotReader) File(ctx context.Context, m RunMeta, ref, path string) (*FileResult, error) {
	return r.git.File(ctx, r.at(m), ref, path)
}

// Meta serves the snapshot's build summary, forced to Live=false: this is a completed Run served from
// the captured bundle, never the (gone) live worktree.
func (r *SnapshotReader) Meta(ctx context.Context, m RunMeta) (*MetaResult, error) {
	res, err := r.git.Meta(ctx, r.at(m))
	if err != nil {
		return nil, err
	}
	res.Live = false
	return res, nil
}

// Compile-time proof SnapshotReader is a Reader — it serves the same surface as the live GitReader.
var _ Reader = (*SnapshotReader)(nil)
