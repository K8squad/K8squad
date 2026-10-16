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

package mesh

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBranchName(t *testing.T) {
	got := BranchName("Coder", "ISI-6009")
	if got != "agent/coder/isi-6009" {
		t.Fatalf("BranchName = %q, want agent/coder/isi-6009", got)
	}
	// Sanitization: unsafe chars collapse, traversal segments cannot survive.
	got = BranchName("../evil role!", "wi/../..")
	if strings.Contains(got, "..") || !strings.HasPrefix(got, "agent/") {
		t.Fatalf("BranchName unsafe: %q", got)
	}
	// Empty segments fall back, never malformed.
	if got := BranchName("", ""); got != "agent/x/x" {
		t.Fatalf("BranchName empty = %q, want agent/x/x", got)
	}
	// Bounded length per segment.
	long := strings.Repeat("a", 200)
	if got := BranchName(long, long); len(got) > 2*branchPartLen+len(branchPrefix)+1 {
		t.Fatalf("BranchName unbounded: %d chars", len(got))
	}
}

func TestWorktreeDirName(t *testing.T) {
	if got := WorktreeDirName("Coder", "ISI-6009"); got != "agent-coder-isi-6009" {
		t.Fatalf("WorktreeDirName = %q", got)
	}
}

func TestSourceSpecValidate(t *testing.T) {
	good := SourceSpec{
		TeamMainDir: "/ws/team-main",
		WorktreeDir: "/ws/worktrees/agent-coder-isi-6009",
		Branch:      "agent/coder/isi-6009",
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("good spec rejected: %v", err)
	}

	cases := []struct {
		name string
		mut  func(*SourceSpec)
	}{
		{"relative team-main", func(s *SourceSpec) { s.TeamMainDir = "ws/team-main" }},
		{"relative worktree", func(s *SourceSpec) { s.WorktreeDir = "ws/wt" }},
		{"unclean path", func(s *SourceSpec) { s.WorktreeDir = "/ws/worktrees/../wt" }},
		{"worktree == team-main", func(s *SourceSpec) { s.WorktreeDir = s.TeamMainDir }},
		{"worktree inside team-main", func(s *SourceSpec) { s.WorktreeDir = s.TeamMainDir + "/wt" }},
		{"team-main inside worktree", func(s *SourceSpec) { s.TeamMainDir = s.WorktreeDir + "/tm" }},
		{"branch without prefix", func(s *SourceSpec) { s.Branch = "main" }},
		{"branch traversal", func(s *SourceSpec) { s.Branch = "agent/../escape" }},
	}
	for _, tc := range cases {
		s := good
		tc.mut(&s)
		if err := s.Validate(); err == nil {
			t.Fatalf("%s: expected rejection, got nil", tc.name)
		}
	}
}

func TestSourceSpecEnv(t *testing.T) {
	s := SourceSpec{TeamMainDir: "/ws/team-main", WorktreeDir: "/ws/wt", Branch: "agent/coder/wi-1"}
	env := s.Env()
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		EnvTeamMainDir + "=/ws/team-main",
		EnvTeamMainReadOnly + "=true",
		EnvSourceBranch + "=agent/coder/wi-1",
		EnvWorktreeDir + "=/ws/wt",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("env missing %q (got %v)", want, env)
		}
	}
}

// initRepo builds a tiny real git repo with one commit on main and an
// origin/main ref to fork from. Skips when git is unavailable.
func initRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("commit", "--allow-empty", "-m", "init")
	run("update-ref", "refs/remotes/origin/main", "main")
	return dir
}

func TestEnsureWorktree(t *testing.T) {
	repo := initRepo(t)
	wt := filepath.Join(t.TempDir(), "agent-coder-wi-1")
	spec := SourceSpec{TeamMainDir: repo, WorktreeDir: wt, Branch: "agent/coder/wi-1"}

	if err := EnsureWorktree(context.Background(), spec); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		t.Fatalf("worktree not provisioned: %v", err)
	}
	// The branch exists and forks from main.
	cmd := exec.Command("git", "rev-parse", "--verify", "agent/coder/wi-1")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("branch missing: %v (%s)", err, out)
	}

	// Idempotent: a second provision reuses, no error.
	if err := EnsureWorktree(context.Background(), spec); err != nil {
		t.Fatalf("EnsureWorktree not idempotent: %v", err)
	}
}

func TestEnsureWorktreeAttachesExistingBranch(t *testing.T) {
	repo := initRepo(t)
	// Simulate a crashed run: the branch exists but no worktree does.
	cmd := exec.Command("git", "branch", "agent/coder/wi-2", "main")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed branch: %v (%s)", err, out)
	}
	wt := filepath.Join(t.TempDir(), "agent-coder-wi-2")
	spec := SourceSpec{TeamMainDir: repo, WorktreeDir: wt, Branch: "agent/coder/wi-2"}
	if err := EnsureWorktree(context.Background(), spec); err != nil {
		t.Fatalf("attach-existing fallback failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		t.Fatalf("worktree not provisioned: %v", err)
	}
}

func TestEnsureWorktreeFailClosed(t *testing.T) {
	// An invalid spec must fail BEFORE any git invocation.
	spec := SourceSpec{TeamMainDir: "relative", WorktreeDir: "/tmp/x", Branch: "main"}
	if err := EnsureWorktree(context.Background(), spec); err == nil {
		t.Fatal("expected validation failure")
	}
}
