/*
Copyright 2026 The K8sAgentMesh Authors.

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

// worktree.go — the per-process branch + worktree write discipline
// (ISI-6009, AgentMesh P1.4a, ADR-0024 §1/§2).
//
// Every standing agent process runs on its OWN branch
// agent/<role>/<work-item-id> in its OWN git worktree, never in the shared
// team-main checkout. The mesh seam carries the layout as a SourceSpec on the
// Envelope (what the process is) resolved onto the LaunchSpec (what the host
// runs); the host provisions the worktree before exec so the spawned runtime
// lands in it. team-main stays read-only to agent processes — filesystem
// enforced at the kube mount layer (pkg/sandbox WorkspaceVolumeMounts,
// workspace-team-main ReadOnly), carried on the POSIX-local substrate as the
// worktree WorkDir + the KSQUAD_* env contract below.
package mesh

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// branchPrefix roots every per-process source branch (ADR-0024 §1).
const branchPrefix = "agent/"

// DefaultBaseRef is the ref a process branch forks from when the envelope
// does not pin one — the team-main tip the shared checkout tracks.
const DefaultBaseRef = "origin/main"

// branchPartLen caps one sanitized branch path segment so the joined ref
// stays well under git's refname and filesystem limits.
const branchPartLen = 48

// The process-env source-discipline contract (ADR-0024 §1/§2): stamped onto
// every SourceSpec-launched process so the shim/runtime honors the layout
// without re-deriving it. Values are paths/branch names — never secret.
const (
	// EnvTeamMainDir is the shared team-main checkout the process may READ
	// (its branch's base) but never write.
	EnvTeamMainDir = "KSQUAD_TEAM_MAIN_DIR"
	// EnvTeamMainReadOnly pins the enforcement stance: "true" — the team-main
	// checkout is read-only to this process (kube: mount-level).
	EnvTeamMainReadOnly = "KSQUAD_TEAM_MAIN_READ_ONLY"
	// EnvSourceBranch is the per-process branch the run's commits land on.
	EnvSourceBranch = "KSQUAD_SOURCE_BRANCH"
	// EnvWorktreeDir is the process's exclusive write surface.
	EnvWorktreeDir = "KSQUAD_WORKTREE_DIR"
)

// SourceSpec is the per-process source-checkout layout (ADR-0024 §1). When an
// Envelope carries one, the spawned process runs ON Branch in WorktreeDir —
// its exclusive write surface — with TeamMainDir readable as the base but
// never writable. A nil Source is the pre-P1.4a shape (plain WorkDir, no
// source isolation) and stays valid for runtimes with no source output.
type SourceSpec struct {
	// TeamMainDir is the shared team-main checkout (read-only to the process).
	TeamMainDir string
	// WorktreeDir is the per-process worktree to provision and run in.
	WorktreeDir string
	// Branch is the per-process branch (BranchName(role, workItemID)).
	Branch string
	// BaseRef is the ref Branch forks from (default DefaultBaseRef).
	BaseRef string
}

// BranchName renders the per-process branch agent/<role>/<work-item-id>. Both
// segments are sanitized (path-safe, bounded, collision-stable) so an
// attacker-shaped role or work-item id cannot forge a refspec or traverse.
func BranchName(role, workItemID string) string {
	return branchPrefix + sanitizeBranchPart(role) + "/" + sanitizeBranchPart(workItemID)
}

// WorktreeDirName renders the per-process worktree directory name a caller
// joins under the workspace PVC worktrees/ partition (pkg/sandbox
// WorkspaceWorktreeDir): agent-<role>-<work-item-id>.
func WorktreeDirName(role, workItemID string) string {
	return "agent-" + sanitizeBranchPart(role) + "-" + sanitizeBranchPart(workItemID)
}

// sanitizeBranchPart reduces one ref/path segment to lowercase [a-z0-9-],
// collapsing runs of anything else to a single dash (mirrors the sandbox
// principal-normalization discipline). An empty result falls back to "x" so
// the branch is never malformed.
func sanitizeBranchPart(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "x"
	}
	if len(out) > branchPartLen {
		out = strings.Trim(out[:branchPartLen], "-")
	}
	return out
}

// Validate fail-closes on a layout that cannot provision safely: both dirs
// must be absolute and clean, the worktree must not BE or CONTAIN team-main
// (that would make the shared checkout the write surface), the branch must
// carry the agent/ prefix, and no component may traverse.
func (s SourceSpec) Validate() error {
	for name, dir := range map[string]string{"TeamMainDir": s.TeamMainDir, "WorktreeDir": s.WorktreeDir} {
		if dir == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
			return fmt.Errorf("mesh: SourceSpec.%s %q is not a clean absolute path", name, dir)
		}
	}
	tm, wt := s.TeamMainDir, s.WorktreeDir
	if wt == tm || strings.HasPrefix(tm, wt+"/") || strings.HasPrefix(wt, tm+"/") {
		return fmt.Errorf("mesh: worktree %q and team-main %q overlap — the write surface must be disjoint from the shared checkout", wt, tm)
	}
	if !strings.HasPrefix(s.Branch, branchPrefix) || len(s.Branch) <= len(branchPrefix) {
		return fmt.Errorf("mesh: SourceSpec.Branch %q lacks the %s per-process prefix", s.Branch, branchPrefix)
	}
	for _, seg := range strings.Split(s.Branch, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("mesh: SourceSpec.Branch %q contains a traversal segment", s.Branch)
		}
	}
	return nil
}

// Env renders the process-env source-discipline contract (ADR-0024 §1).
func (s SourceSpec) Env() []string {
	return []string{
		EnvTeamMainDir + "=" + s.TeamMainDir,
		EnvTeamMainReadOnly + "=true",
		EnvSourceBranch + "=" + s.Branch,
		EnvWorktreeDir + "=" + s.WorktreeDir,
	}
}

// EnsureWorktree provisions s.WorktreeDir as a git worktree of the team-main
// checkout on s.Branch, idempotently:
//   - the worktree already exists → reuse it (a respawned process reattaches);
//   - the branch already exists (a crashed run left it) → attach a fresh
//     worktree to that branch rather than failing on -b;
//   - otherwise fork Branch from BaseRef.
//
// A git failure is a launch failure — a process that cannot get its own write
// surface must not fall back into the shared checkout.
func EnsureWorktree(ctx context.Context, s SourceSpec) error {
	if err := s.Validate(); err != nil {
		return err
	}
	base := s.BaseRef
	if base == "" {
		base = DefaultBaseRef
	}
	if _, err := os.Stat(filepath.Join(s.WorktreeDir, ".git")); err == nil {
		return nil // already provisioned — idempotent reuse
	}
	if out, err := gitOut(ctx, s.TeamMainDir, "worktree", "add", "-b", s.Branch, s.WorktreeDir, base); err != nil {
		if out2, err2 := gitOut(ctx, s.TeamMainDir, "worktree", "add", s.WorktreeDir, s.Branch); err2 != nil {
			return fmt.Errorf("mesh: provision worktree %s on %s: %v (%s); attach-existing fallback: %v (%s)",
				s.WorktreeDir, s.Branch, err, strings.TrimSpace(out), err2, strings.TrimSpace(out2))
		}
	}
	return nil
}

// gitOut runs one git plumbing command inside dir. The worktree subcommand is
// local-only (no credential, no network), so this never rides the egress gate.
func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	// #nosec G204 -- args are the host-resolved SourceSpec (validated
	// fail-closed above), never attacker-controlled task input.
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
