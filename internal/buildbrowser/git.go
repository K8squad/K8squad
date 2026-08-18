package buildbrowser

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"
)

// ── Result types (JSON wire shape the BFF proxies verbatim to the console) ──────────────────────

// TreeEntry is one blob in a `tree` listing. Trees themselves are elided (a flat, recursive file
// list is what the build browser renders); Size is the blob's byte length.
type TreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Size int64  `json:"size"`
}

// TreeResult is the capped file listing at a ref. Truncated is set when the tree exceeds
// MaxTreeEntries, so the console can show "…and N more".
type TreeResult struct {
	Ref       string      `json:"ref"`
	Entries   []TreeEntry `json:"entries"`
	Truncated bool        `json:"truncated"`
}

// DiffResult is the capped base..run unified diff. Truncated is set when the patch exceeds
// MaxDiffBytes; the returned Patch is a valid prefix.
type DiffResult struct {
	Base      string `json:"base"`
	Head      string `json:"head"`
	Patch     string `json:"patch"`
	Truncated bool   `json:"truncated"`
}

// FileResult is a single file's capped contents at a ref. Content JSON-marshals as base64 (safe for
// binary blobs); Size is the FULL blob size even when Content is truncated to MaxFileBytes.
type FileResult struct {
	Ref       string `json:"ref"`
	Path      string `json:"path"`
	Content   []byte `json:"content"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
}

// MetaResult is the build summary — the short shas the tree/diff are taken against and the changed
// file count — the console header renders before the user drills in.
type MetaResult struct {
	RunID        string `json:"runId"`
	Head         string `json:"head"`
	Base         string `json:"base"`
	ChangedFiles int    `json:"changedFiles"`
}

// ── GitReader ───────────────────────────────────────────────────────────────────────────────────

// GitReader implements Reader over git PLUMBING (never the raw filesystem). Every read runs
// `git -C RepoPath …` against server-controlled refs, with the caller's path resolved as a tree
// object (`<ref>:<path>`) so traversal outside the workspace is structurally impossible.
type GitReader struct {
	GitBin  string        // git binary; defaults to "git"
	Timeout time.Duration // per-invocation timeout; 0 ⇒ no extra timeout beyond ctx
}

// NewGitReader returns a GitReader with sane defaults (a 30s per-call timeout).
func NewGitReader() *GitReader { return &GitReader{GitBin: "git", Timeout: 30 * time.Second} }

func (g *GitReader) bin() string {
	if g.GitBin == "" {
		return "git"
	}
	return g.GitBin
}

// gitEnv neutralizes user/system git config and network prompts so a read is deterministic and can
// never hang waiting for credentials — this is a read-only, offline plumbing call.
func gitEnv() []string {
	return append([]string{},
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"HOME=/nonexistent",
		"LC_ALL=C",
	)
}

// resolveRef maps the caller's ?ref= to a server-controlled revision. Default (empty) ⇒ the Run
// head; "base" ⇒ the branch point. Anything else is a 400 — it never reaches git.
func (g *GitReader) resolveRef(m RunMeta, ref string) (string, error) {
	switch ref {
	case "", "run":
		return m.HeadRef, nil
	case "base":
		return m.BaseRef, nil
	default:
		return "", ErrBadRequest
	}
}

// capture runs a git command capturing stdout up to limit bytes. If the command produces more, the
// prefix is kept, the process is killed, and truncated=true. git's own stderr surfaces as the error.
func (g *GitReader) capture(ctx context.Context, repo string, limit int64, args ...string) (out []byte, truncated bool, err error) {
	cctx, cancel := context.WithCancel(ctx)
	if g.Timeout > 0 {
		cctx, cancel = context.WithTimeout(ctx, g.Timeout)
	}
	defer cancel()

	// #nosec G204 -- fixed `git` binary; refs come from server-controlled RunMeta and the only
	// caller input (a file path) is validated by cleanTreePath and passed as the DATA operand
	// `rev:path`, never as a flag. No shell is involved.
	cmd := exec.CommandContext(cctx, g.bin(), append([]string{"-C", repo}, args...)...)
	cmd.Env = gitEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}
	buf, rerr := io.ReadAll(io.LimitReader(stdout, limit+1))
	if int64(len(buf)) > limit {
		truncated = true
		buf = buf[:limit]
		cancel() // we have enough — kill git rather than drain a huge stream
	}
	_, _ = io.Copy(io.Discard, stdout)
	werr := cmd.Wait()
	if truncated {
		return buf, true, nil // a killed process is expected here; the prefix is valid
	}
	if rerr != nil {
		return nil, false, rerr
	}
	if werr != nil {
		return nil, false, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), werr, strings.TrimSpace(stderr.String()))
	}
	return buf, false, nil
}

// Tree lists blobs recursively at ref, capped at MaxTreeEntries. Uses `ls-tree -r -l -z` and
// stops reading once the cap is hit (bounded memory even for a huge tree).
func (g *GitReader) Tree(ctx context.Context, m RunMeta, ref string) (*TreeResult, error) {
	rev, err := g.resolveRef(m, ref)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithCancel(ctx)
	if g.Timeout > 0 {
		cctx, cancel = context.WithTimeout(ctx, g.Timeout)
	}
	defer cancel()

	// #nosec G204 -- fixed `git` binary; rev is a server-controlled ref from RunMeta (not the body),
	// terminated with `--`, no shell. See capture() for the rationale on the shared read path.
	cmd := exec.CommandContext(cctx, g.bin(), "-C", m.RepoPath, "ls-tree", "-r", "-l", "-z", rev, "--")
	cmd.Env = gitEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	sc.Split(scanNUL)

	res := &TreeResult{Ref: refLabel(ref), Entries: make([]TreeEntry, 0, 64)}
	for sc.Scan() {
		if len(res.Entries) >= MaxTreeEntries {
			res.Truncated = true
			cancel()
			break
		}
		if e, ok := parseLsTreeLine(sc.Text()); ok {
			res.Entries = append(res.Entries, e)
		}
	}
	_, _ = io.Copy(io.Discard, stdout)
	werr := cmd.Wait()
	if !res.Truncated {
		if serr := sc.Err(); serr != nil {
			return nil, serr
		}
		if werr != nil {
			return nil, gitErr("ls-tree", werr, &stderr)
		}
	}
	return res, nil
}

// Diff returns the base..head unified diff, capped at MaxDiffBytes.
func (g *GitReader) Diff(ctx context.Context, m RunMeta) (*DiffResult, error) {
	out, truncated, err := g.capture(ctx, m.RepoPath, MaxDiffBytes,
		"diff", "--no-color", m.BaseRef, m.HeadRef, "--")
	if err != nil {
		return nil, err
	}
	return &DiffResult{Base: m.BaseRef, Head: m.HeadRef, Patch: string(out), Truncated: truncated}, nil
}

// File returns path's contents at ref, capped at MaxFileBytes. The path is resolved as a tree
// object; traversal/absolute paths and non-existent paths return ErrNotFound (existence-hiding).
func (g *GitReader) File(ctx context.Context, m RunMeta, ref, p string) (*FileResult, error) {
	rev, err := g.resolveRef(m, ref)
	if err != nil {
		return nil, err
	}
	clean, err := cleanTreePath(p)
	if err != nil {
		return nil, err
	}
	spec := rev + ":" + clean // e.g. "abc123:cmd/main.go" — never begins with '-', safe as an arg

	// Full size first (so Size is honest even when Content is truncated). A resolution failure here
	// means the path is not a blob in the tree → 404.
	sizeOut, _, serr := g.capture(ctx, m.RepoPath, 64, "cat-file", "-s", spec)
	if serr != nil {
		return nil, ErrNotFound
	}
	size, _ := strconv.ParseInt(strings.TrimSpace(string(sizeOut)), 10, 64)

	content, truncated, err := g.capture(ctx, m.RepoPath, MaxFileBytes, "cat-file", "blob", spec)
	if err != nil {
		return nil, ErrNotFound
	}
	return &FileResult{Ref: refLabel(ref), Path: clean, Content: content, Size: size, Truncated: truncated}, nil
}

// Meta returns the short head/base shas and the changed-file count.
func (g *GitReader) Meta(ctx context.Context, m RunMeta) (*MetaResult, error) {
	head, _, err := g.capture(ctx, m.RepoPath, 64, "rev-parse", "--short=12", m.HeadRef+"^{commit}")
	if err != nil {
		return nil, ErrNotFound
	}
	base, _, err := g.capture(ctx, m.RepoPath, 64, "rev-parse", "--short=12", m.BaseRef+"^{commit}")
	if err != nil {
		return nil, ErrNotFound
	}
	names, _, err := g.capture(ctx, m.RepoPath, MaxDiffBytes, "diff", "--name-only", "-z", m.BaseRef, m.HeadRef, "--")
	if err != nil {
		return nil, err
	}
	return &MetaResult{
		RunID:        m.RunID,
		Head:         strings.TrimSpace(string(head)),
		Base:         strings.TrimSpace(string(base)),
		ChangedFiles: countNUL(names),
	}, nil
}

// ── small helpers ───────────────────────────────────────────────────────────────────────────────

// refLabel normalizes the ref echoed back in a response ("" ⇒ "run").
func refLabel(ref string) string {
	if ref == "" {
		return "run"
	}
	return ref
}

// cleanTreePath validates a caller-supplied file path. Empty ⇒ 400. Absolute or `..`-escaping ⇒
// ErrNotFound (hide, don't 400) so a probe cannot distinguish a rejected path from a missing one.
func cleanTreePath(p string) (string, error) {
	if p == "" {
		return "", ErrBadRequest
	}
	if strings.HasPrefix(p, "/") {
		return "", ErrNotFound
	}
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") || clean == "." {
		return "", ErrNotFound
	}
	return clean, nil
}

// parseLsTreeLine parses one `ls-tree -l -z` record: "<mode> <type> <object> <size>\t<path>".
func parseLsTreeLine(line string) (TreeEntry, bool) {
	tab := strings.IndexByte(line, '\t')
	if tab < 0 {
		return TreeEntry{}, false
	}
	meta, p := line[:tab], line[tab+1:]
	f := strings.Fields(meta)
	if len(f) < 4 {
		return TreeEntry{}, false
	}
	if f[1] != "blob" { // trees/commits (submodules) are not listed
		return TreeEntry{}, false
	}
	size, _ := strconv.ParseInt(f[3], 10, 64)
	return TreeEntry{Path: p, Mode: f[0], Size: size}, true
}

// scanNUL is a bufio.SplitFunc splitting on NUL bytes (git's -z output).
func scanNUL(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexByte(data, 0); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// countNUL counts NUL-terminated records in git -z output.
func countNUL(b []byte) int {
	b = bytes.TrimRight(b, "\x00")
	if len(b) == 0 {
		return 0
	}
	return bytes.Count(b, []byte{0}) + 1
}

func gitErr(what string, werr error, stderr *bytes.Buffer) error {
	return fmt.Errorf("git %s: %w: %s", what, werr, strings.TrimSpace(stderr.String()))
}
