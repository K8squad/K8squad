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

package reconcile

import (
	"errors"
	"testing"
)

// TestOutcomeFrom asserts the single-signal outcome derivation for the success
// and error outcomes of a reconcile drive (ISI-5011 P0#2): an error return is
// always failed, StepFailed is a failed business outcome, and
// Succeeded/Cancelled are success. No terminal/failed step may yield unknown.
func TestOutcomeFrom(t *testing.T) {
	errBoom := errors.New("boom")

	cases := []struct {
		name string
		step Step
		err  error
		want Outcome
	}{
		{"succeeded-is-success", StepSucceeded, nil, OutcomeSuccess},
		{"cancelled-is-success", StepCancelled, nil, OutcomeSuccess},
		{"failed-step-is-failed", StepFailed, nil, OutcomeFailed},
		{"error-wins-over-succeeded", StepSucceeded, errBoom, OutcomeFailed},
		{"error-wins-over-failed", StepFailed, errBoom, OutcomeFailed},
		{"error-wins-over-inflight", StepRunning, errBoom, OutcomeFailed},
		{"running-is-unknown", StepRunning, nil, OutcomeUnknown},
		{"pending-is-unknown", StepPending, nil, OutcomeUnknown},
		{"paused-is-unknown", StepPausedRateLimited, nil, OutcomeUnknown},
		{"zero-step-is-unknown", "", nil, OutcomeUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := OutcomeFrom(tc.step, tc.err); got != tc.want {
				t.Fatalf("OutcomeFrom(%q, %v) = %v, want %v", tc.step, tc.err, got, tc.want)
			}
		})
	}
}

// TestOutcomeIsErrorAndString pins the helper predicates and the wire
// vocabulary so a caller mapping the outcome to ksquad.outcome stays stable.
func TestOutcomeIsErrorAndString(t *testing.T) {
	if !OutcomeFailed.IsError() {
		t.Error("OutcomeFailed.IsError() = false, want true")
	}
	if OutcomeSuccess.IsError() || OutcomeUnknown.IsError() {
		t.Error("success/unknown must not report IsError")
	}

	for _, tc := range []struct {
		o    Outcome
		want string
	}{
		{OutcomeSuccess, "success"},
		{OutcomeFailed, "error"},
		{OutcomeUnknown, "unknown"},
	} {
		if got := tc.o.String(); got != tc.want {
			t.Errorf("%v.String() = %q, want %q", tc.o, got, tc.want)
		}
	}
}
