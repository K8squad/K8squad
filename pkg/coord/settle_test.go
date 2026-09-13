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

// SettleLaneOf (ISI-4237): the terminal-step → board-lane mapping for the lane
// the engine actually MOVES. failed/cancelled return the ticket to the
// dispatchable lane; succeeded maps to NO lane — a settled run is finished work
// the human moves to done, and the engine must not touch work_item on success
// (the ISI-4298 resume pin / rundrive D1/D5/D6 keep it in_progress). A retry
// re-entry (and anything off the terminal set) also maps to "".
func TestSettleLaneOf(t *testing.T) {
	cases := []struct {
		step string
		want string
	}{
		{"succeeded", ""}, // no engine lane move — human owns the move to done
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

// settleIsTerminal separates a terminal outcome (owes the thread a summary)
// from a non-terminal re-entry (owes nothing). succeeded is terminal even
// though it moves no lane.
func TestSettleIsTerminal(t *testing.T) {
	for _, s := range []string{"succeeded", "failed", "cancelled"} {
		if !settleIsTerminal(s) {
			t.Fatalf("settleIsTerminal(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"claiming_sandbox", "dispatching", "running", "pending", ""} {
		if settleIsTerminal(s) {
			t.Fatalf("settleIsTerminal(%q) = true, want false", s)
		}
	}
}

// settleSummary renders one honest line: outcome verb, agent attribution when
// known, and the lane claim only when the settle actually moved it.
func TestSettleSummary(t *testing.T) {
	// succeeded owns no engine lane — completion is recorded, the human owns
	// the move to done.
	if got := settleSummary("succeeded", "", false, "sam"); got != "Run succeeded — agent sam: recorded on the ticket; lane unchanged (awaiting review)." {
		t.Fatalf("succeeded summary = %q", got)
	}
	if got := settleSummary("failed", "todo", true, ""); got != "Run failed (retry budget exhausted): lane → todo (engine settle)." {
		t.Fatalf("failed unattributed summary = %q", got)
	}
	// A human lane move raced ahead of a failed/cancelled settle: the settle
	// reports the outcome without claiming a lane move.
	if got := settleSummary("cancelled", "todo", false, "sam"); got != "Run cancelled — agent sam: lane left as-is (moved by a human or not in progress)." {
		t.Fatalf("unmoved summary = %q", got)
	}
}
