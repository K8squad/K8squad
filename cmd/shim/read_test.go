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

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// readRepo builds a base/run two-commit repo and points the read-verb env at it.
func readRepo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		full := append([]string{"-C", dir,
			"-c", "user.email=test@k8squad.local", "-c", "user.name=test",
			"-c", "commit.gpgsign=false"}, args...)
		cmd := exec.Command("git", full...)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out.String())
		}
	}
	write := func(rel, data string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	run("init", "-q", "-b", "base")
	write("a.txt", "base\n")
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	run("checkout", "-q", "-b", "run")
	write("a.txt", "run changed\n")
	write("added.txt", "brand new\n")
	run("add", "-A")
	run("commit", "-q", "-m", "run")

	t.Setenv("KSQUAD_WORKDIR", dir)
	t.Setenv("KSQUAD_RUN_REF", "run")
	t.Setenv("KSQUAD_BASE_REF", "base")
	t.Setenv("KSQUAD_RUN_ID", "run-xyz")
}

func TestReadVerb_Meta(t *testing.T) {
	readRepo(t)
	var out bytes.Buffer
	if err := run([]string{"read", "meta"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("read meta: %v", err)
	}
	var env struct {
		Live     bool   `json:"live"`
		Resource string `json:"resource"`
		Result   struct {
			RunID        string `json:"runId"`
			ChangedFiles int    `json:"changedFiles"`
			Live         bool   `json:"live"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("meta not valid JSON: %v\n%s", err, out.String())
	}
	if !env.Live {
		t.Errorf("envelope live = false, want true")
	}
	if env.Resource != "meta" {
		t.Errorf("resource = %q, want meta", env.Resource)
	}
	if env.Result.ChangedFiles != 2 {
		t.Errorf("changedFiles = %d, want 2", env.Result.ChangedFiles)
	}
	if !env.Result.Live {
		t.Errorf("result.live = false, want true (live worktree read)")
	}
	if env.Result.RunID != "run-xyz" {
		t.Errorf("runId = %q, want run-xyz", env.Result.RunID)
	}
}

func TestReadVerb_Tree(t *testing.T) {
	readRepo(t)
	var out bytes.Buffer
	if err := run([]string{"read", "tree"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("read tree: %v", err)
	}
	var env struct {
		Ref    string `json:"ref"`
		Result struct {
			Entries []struct {
				Path string `json:"path"`
			} `json:"entries"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("tree not valid JSON: %v", err)
	}
	if env.Ref != "run" {
		t.Errorf("ref = %q, want run", env.Ref)
	}
	var saw bool
	for _, e := range env.Result.Entries {
		if e.Path == "added.txt" {
			saw = true
		}
	}
	if !saw {
		t.Errorf("tree missing added.txt: %+v", env.Result.Entries)
	}
}

func TestReadVerb_File(t *testing.T) {
	readRepo(t)
	var out bytes.Buffer
	if err := run([]string{"read", "file", "--path", "a.txt"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("read file: %v", err)
	}
	var env struct {
		Path   string `json:"path"`
		Result struct {
			Content string `json:"content"` // base64 (FileResult.Content is []byte)
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("file not valid JSON: %v", err)
	}
	if env.Path != "a.txt" {
		t.Errorf("path = %q, want a.txt", env.Path)
	}
	raw, err := base64.StdEncoding.DecodeString(env.Result.Content)
	if err != nil {
		t.Fatalf("content not base64: %v", err)
	}
	if string(raw) != "run changed\n" {
		t.Errorf("a.txt = %q, want run changed", string(raw))
	}
}

func TestReadVerb_FileRequiresPath(t *testing.T) {
	readRepo(t)
	var out bytes.Buffer
	if err := run([]string{"read", "file"}, strings.NewReader(""), &out); err == nil {
		t.Fatalf("read file without --path should error")
	}
}

func TestReadVerb_UnknownResource(t *testing.T) {
	readRepo(t)
	var out bytes.Buffer
	if err := run([]string{"read", "bogus"}, strings.NewReader(""), &out); err == nil {
		t.Fatalf("unknown resource should error")
	}
}

// TestReadVerb_NoRuntimeNeeded proves the read verb is runtime-agnostic: it works with no
// KSQUAD_RUNTIME_TYPE set (the reconciler injects a runtime only for card/run).
func TestReadVerb_NoRuntimeNeeded(t *testing.T) {
	readRepo(t)
	t.Setenv("KSQUAD_RUNTIME_TYPE", "")
	t.Setenv("RUNTIME", "")
	var out bytes.Buffer
	if err := run([]string{"read", "meta"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("read meta without runtime: %v", err)
	}
}
