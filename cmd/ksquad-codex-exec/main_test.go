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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildEnvelope(t *testing.T) {
	tests := []struct {
		name          string
		systemContext string
		input         string
		want          string
	}{
		{"both", "you are an agent", "fix the bug", "you are an agent\n\nfix the bug"},
		{"input only", "", "fix the bug", "fix the bug"},
		{"system only", "you are an agent", "", "you are an agent"},
		{"neither", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildEnvelope(tc.systemContext, tc.input); got != tc.want {
				t.Fatalf("buildEnvelope(%q, %q) = %q, want %q", tc.systemContext, tc.input, got, tc.want)
			}
		})
	}
}

// TestBuildArgs asserts the codex argv is `exec -` + passthrough flags, and —
// crucially — that no prompt/envelope value can leak onto it (AC1/AC2).
func TestBuildArgs(t *testing.T) {
	passthrough := []string{"--model", "gpt-5-codex", "--sandbox", "workspace-write"}
	got := buildArgs(passthrough)

	if len(got) < 2 || got[0] != "exec" || got[1] != "-" {
		t.Fatalf("buildArgs must begin with [exec -], got %v", got)
	}
	if want := append([]string{"exec", "-"}, passthrough...); !equal(got, want) {
		t.Fatalf("buildArgs(%v) = %v, want %v", passthrough, got, want)
	}
	// The prompt halves must never appear on argv.
	for _, a := range got {
		if strings.Contains(a, "you are an agent") || strings.Contains(a, "fix the bug") {
			t.Fatalf("prompt leaked onto argv: %v", got)
		}
	}
}

// TestRunDeliversEnvelopeOnStdinNotArgv is the AC1/AC2 end-to-end proof: with a
// fake codex that records its argv and stdin, the envelope arrives on stdin and
// never on argv, while adapter flags pass through verbatim.
func TestRunDeliversEnvelopeOnStdinNotArgv(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	stdinFile := filepath.Join(dir, "stdin")

	// Fake codex: dump "$@" to argvFile (one arg per line) and stdin to stdinFile.
	fake := filepath.Join(dir, "codex")
	script := "#!/bin/sh\n" +
		": > \"$HELPER_ARGV_FILE\"\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\" >> \"$HELPER_ARGV_FILE\"; done\n" +
		"cat > \"$HELPER_STDIN_FILE\"\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil { // #nosec G306 -- test fake must be executable
		t.Fatal(err)
	}

	systemContext := "you are an agent"
	input := "fix the bug"
	env := map[string]string{
		envCodexBin:         fake,
		envSystemContext:    systemContext,
		envInput:            input,
		"HELPER_ARGV_FILE":  argvFile,
		"HELPER_STDIN_FILE": stdinFile,
	}
	// The fake inherits the process env (run leaves cmd.Env nil), so the helper
	// file paths must be set on the real environment.
	t.Setenv("HELPER_ARGV_FILE", argvFile)
	t.Setenv("HELPER_STDIN_FILE", stdinFile)
	getenv := func(k string) string { return env[k] }

	var out, errOut bytes.Buffer
	passthrough := []string{"--model", "gpt-5-codex"}
	if code := run(passthrough, getenv, &out, &errOut); code != 0 {
		t.Fatalf("run exit=%d stderr=%q", code, errOut.String())
	}

	gotArgv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	gotStdin, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatal(err)
	}

	// AC2: argv passthrough — exec - --model gpt-5-codex, one per line.
	wantArgv := "exec\n-\n--model\ngpt-5-codex\n"
	if string(gotArgv) != wantArgv {
		t.Fatalf("argv = %q, want %q", gotArgv, wantArgv)
	}
	// AC1: the prompt never appears on argv...
	if strings.Contains(string(gotArgv), systemContext) || strings.Contains(string(gotArgv), input) {
		t.Fatalf("prompt leaked onto argv: %q", gotArgv)
	}
	// ...and the assembled envelope is delivered on stdin.
	if want := buildEnvelope(systemContext, input); string(gotStdin) != want {
		t.Fatalf("stdin = %q, want %q", gotStdin, want)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
