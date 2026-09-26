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

package telemetry

import (
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/K8squad/K8squad/pkg/reconcile"
)

// SetSpanOutcome stamps a span's status from a reconcile outcome so
// span.status_code (Ok/Error) and the exception signal Bluebox derives
// request.is_failed from stay coupled to the SAME outcome (ISI-5011 P0#2):
//
//   - OutcomeFailed  → RecordError(err) + SetStatus(Error)  (span error,
//     request failed)
//   - OutcomeSuccess → SetStatus(Ok)                         (span ok,
//     request ok)
//   - OutcomeUnknown → SetStatus(Unset)                      (in-flight,
//     neither)
//
// A nil err on a failed outcome (e.g. a terminal StepFailed with no Go error)
// still marks the span error — the failure signal is the outcome, not the error
// value. The recorded exception event and the Error status are the two inputs
// Bluebox's failure detection evaluates to set request.is_failed, so keeping
// them driven by one outcome prevents the status/failed decoupling this helper
// exists to close.
func SetSpanOutcome(span trace.Span, outcome reconcile.Outcome, err error) {
	switch outcome {
	case reconcile.OutcomeFailed:
		desc := "reconcile failed"
		if err != nil {
			desc = err.Error()
			span.RecordError(err)
		}
		span.SetStatus(codes.Error, desc)
	case reconcile.OutcomeSuccess:
		span.SetStatus(codes.Ok, "")
	default:
		span.SetStatus(codes.Unset, "")
	}
}
