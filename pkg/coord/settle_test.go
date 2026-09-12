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

import "testing"

// SettleLaneOf (ISI-4237): the terminal-step → board-lane mapping. succeeded
// lands the terminal lane; failed/cancelled return the ticket to the
// dispatchable lane; a retry re-entry (and anything off the terminal set)
// maps to NO lane — the settle must be a no-op there, never a comment on the
// thread for an in-flight retry.
func TestSettleLaneOf(t *testing.T) {
	cases := []struct {
		step string
		want string
	}{
		{"succeeded", "done"},
		{"failed", "todo"},
		{"cancelled", "todo"},
		{"claiming_sandbox", ""}, // retry lap: nothing owed
		{"dispatching", ""},
		{"running", ""},
		{"collecting", ""},
		{"pending", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := SettleLaneOf(tc.step); got != tc.want {
			t.Fatalf("SettleLaneOf(%q) = %q, want %q", tc.step, got, tc.want)
		}
	}
}

// settleSummary renders one honest line: outcome verb, agent attribution when
// known, and the lane claim only when the settle actually moved it.
func TestSettleSummary(t *testing.T) {
	if got := settleSummary("succeeded", "done", true, "sam"); got != "Run succeeded — agent sam: lane → done (engine settle)." {
		t.Fatalf("succeeded summary = %q", got)
	}
	if got := settleSummary("failed", "todo", true, ""); got != "Run failed (retry budget exhausted): lane → todo (engine settle)." {
		t.Fatalf("failed unattributed summary = %q", got)
	}
	// A human lane move raced ahead: the settle reports the outcome without
	// claiming a lane move.
	if got := settleSummary("succeeded", "done", false, "sam"); got != "Run succeeded — agent sam: lane left as-is (moved by a human or not in progress)." {
		t.Fatalf("unmoved summary = %q", got)
	}
}
