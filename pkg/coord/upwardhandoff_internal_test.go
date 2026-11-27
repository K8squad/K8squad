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

package coord

import (
	"strings"
	"testing"
)

// upwardKindFromChanges (the WS-B kind mapping): a pull request outranks a
// commit, a commit outranks a bare completion; unknown kinds change nothing.
func TestUpwardKindFromChanges(t *testing.T) {
	cases := []struct {
		name  string
		kinds []string
		want  string
	}{
		{"no changes", nil, upwardKindDone},
		{"empty", []string{}, upwardKindDone},
		{"one commit", []string{"commit"}, upwardKindTicket},
		{"commits only", []string{"commit", "commit"}, upwardKindTicket},
		{"one pr", []string{"pull_request"}, upwardKindPR},
		{"commit then pr", []string{"commit", "pull_request"}, upwardKindPR},
		{"pr then commit", []string{"pull_request", "commit"}, upwardKindPR},
		{"unknown kind", []string{"other"}, upwardKindDone},
	}
	for _, tc := range cases {
		if got := upwardKindFromChanges(tc.kinds); got != tc.want {
			t.Fatalf("%s: upwardKindFromChanges(%v) = %q, want %q", tc.name, tc.kinds, got, tc.want)
		}
	}
}

// renderUpwardBrief: the wake body names the kind, the finishing agent, the
// run and work-item refs, the changes, the artifacts and the next action.
func TestRenderUpwardBrief(t *testing.T) {
	got := renderUpwardBrief("pr", "ic-sam", "run-42", "item-7",
		[]string{"- pull_request https://github.com/x/pull/1 — merged"},
		[]string{"- handoff: coord+audit://42"})
	for _, want := range []string{
		"Upward hand-off (pr) from agent ic-sam.",
		"Run run-42 succeeded on work item item-7",
		"What changed:",
		"- pull_request https://github.com/x/pull/1 — merged",
		"Artifacts:",
		"- handoff: coord+audit://42",
		"Next: review the outcome",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("renderUpwardBrief missing %q in:\n%s", want, got)
		}
	}
}
