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

package a2a

import "testing"

// TestShellCommandHead (ISI-4720): the head-token extraction returns a bounded,
// recognized executable name (never arguments or an arbitrary path), strips
// leading env-assignment prefixes, and reduces absolute paths to their
// basename — so a bash-wrapped git/kubectl call can be categorized without
// transporting the (possibly secret) command line.
func TestShellCommandHead(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{"plain git", "git status", "git"},
		{"git with args and flags", "git commit -m 'secret message'", "git"},
		{"kubectl", "kubectl apply -f deploy.yaml", "kubectl"},
		{"npm", "npm ci", "npm"},
		{"python3", "python3 -m pytest", "python3"},
		{"absolute path reduced to basename", "/usr/bin/git fetch", "git"},
		{"env prefix stripped", "GIT_SSH=x git push origin main", "git"},
		{"multiple env prefixes", "A=1 B=2 kubectl get pods", "kubectl"},
		{"leading whitespace", "   docker build .", "docker"},
		{"unrecognized executable omitted", "ls -la", ""},
		{"script path omitted (not recognized, not leaked)", "./deploy-secret.sh --token abc", ""},
		{"empty", "", ""},
		{"only env assignments", "FOO=bar BAZ=qux", ""},
		{"flag is not an env assignment", "-x=1 git status", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShellCommandHead(tt.command); got != tt.want {
				t.Errorf("ShellCommandHead(%q) = %q, want %q", tt.command, got, tt.want)
			}
		})
	}
}

// TestIsEnvAssignment guards the env-prefix classifier: only a NAME=value
// token (identifier chars before the =) counts, so a flag like -x=1 or a bare
// =value never masks the real executable.
func TestIsEnvAssignment(t *testing.T) {
	tests := []struct {
		tok  string
		want bool
	}{
		{"FOO=bar", true},
		{"GIT_SSH_COMMAND=ssh", true},
		{"A=1", true},
		{"git", false},
		{"-x=1", false},
		{"=value", false},
		{"", false},
		{"foo", false},
	}
	for _, tt := range tests {
		if got := isEnvAssignment(tt.tok); got != tt.want {
			t.Errorf("isEnvAssignment(%q) = %v, want %v", tt.tok, got, tt.want)
		}
	}
}
