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

// Outcome is the observability outcome of one reconcile drive — the single
// signal that both span.status_code (Ok/Error) and request.is_failed are
// derived from (ISI-5011 P0#2). It collapses the two formerly-decoupled failure
// signals — the drive's returned error (an infra/transient failure the caller
// requeues on) and a terminal StepFailed (a business failure) — into one
// tri-state so an error reconcile marks the span error and the request failed
// consistently.
type Outcome uint8

const (
	// OutcomeUnknown is an in-flight or not-yet-terminal drive: nothing failed,
	// nothing succeeded. The span leaves status Unset.
	OutcomeUnknown Outcome = iota
	// OutcomeSuccess is a cleanly-completed drive (succeeded or cancelled).
	OutcomeSuccess
	// OutcomeFailed is a failed drive: either the drive returned an error or the
	// durable step settled on failed.
	OutcomeFailed
)

// OutcomeFrom derives the observability outcome from a durable step and the
// drive's error. A non-nil err is always failed (an infra/transient error the
// caller requeues on); with a nil err, StepFailed is a failed business outcome,
// StepSucceeded/StepCancelled are success, and any non-terminal step is unknown
// (still in flight, not yet a failure).
func OutcomeFrom(step Step, err error) Outcome {
	if err != nil {
		return OutcomeFailed
	}
	switch step {
	case StepSucceeded, StepCancelled:
		return OutcomeSuccess
	case StepFailed:
		return OutcomeFailed
	default:
		return OutcomeUnknown
	}
}

// IsError reports whether the outcome is a failed drive.
func (o Outcome) IsError() bool { return o == OutcomeFailed }

// String returns the outcome's wire vocabulary, aligned with the ksquad.outcome
// attribute ("success" | "error" | "unknown") used across telemetry spans.
func (o Outcome) String() string {
	switch o {
	case OutcomeSuccess:
		return "success"
	case OutcomeFailed:
		return "error"
	default:
		return "unknown"
	}
}
