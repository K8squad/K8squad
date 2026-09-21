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

import "testing"

// TestIsVersionArgs pins the fast-exit contract that lets the distroless image serve as
// its own image-warm / pre-pull command (ISI-4721 / GH #525): only the exact version
// forms short-circuit, everything else falls through to the read server so a stray arg
// can never accidentally turn a serving pod into a no-op (or vice versa).
func TestIsVersionArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"no args starts server", nil, false},
		{"empty slice starts server", []string{}, false},
		{"version subcommand", []string{"version"}, true},
		{"double dash version", []string{"--version"}, true},
		{"single dash version", []string{"-version"}, true},
		{"short v flag", []string{"-v"}, true},
		{"version with trailing args", []string{"version", "extra"}, true},
		{"unknown flag serves", []string{"--serve"}, false},
		{"bin false does not match", []string{"/bin/false"}, false},
		{"help is not version", []string{"help"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isVersionArgs(tc.args); got != tc.want {
				t.Fatalf("isVersionArgs(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}
