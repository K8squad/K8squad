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

package coord_test

import (
	"testing"

	"github.com/K8squad/K8squad/pkg/coord"
)

// TestA2AFollowGateOpen pins the ISI-4432/4435 decision: the terminal
// collecting→succeeded advance is permitted ONLY for a non-a2a run (no follow to
// wait on) or an a2a run whose follow durably settled with a succeeded outcome —
// never on the bare submit-ack, and never on a failed / follow_error settlement
// (those finalize through the drive loop's death paths, not this happy-path hop).
func TestA2AFollowGateOpen(t *testing.T) {
	cases := []struct {
		name       string
		dispatched bool
		settled    bool
		outcome    string
		want       bool
	}{
		{"non-a2a run (no dispatch lap) advances freely", false, false, "", true},
		{"a2a dispatched, follow not yet settled → park", true, false, "", false},
		{"a2a settled succeeded → advance", true, true, coord.SettleOutcomeSucceeded, true},
		{"a2a settled failed → park (death path finalizes)", true, true, coord.SettleOutcomeFailed, false},
		{"a2a settled follow_error → park (death path finalizes)", true, true, coord.SettleOutcomeFollowError, false},
		// Defensive: an outcome-less but settled row (should not occur — the
		// migration writes settled_at + settle_outcome together) is not succeeded.
		{"a2a settled without a succeeded outcome → park", true, true, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := coord.A2AFollowGateOpen(tc.dispatched, tc.settled, tc.outcome); got != tc.want {
				t.Fatalf("A2AFollowGateOpen(%v,%v,%q) = %v, want %v",
					tc.dispatched, tc.settled, tc.outcome, got, tc.want)
			}
		})
	}
}
