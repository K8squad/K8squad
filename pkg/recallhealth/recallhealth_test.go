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

package recallhealth

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Registering on a throwaway registry executes the once; package-level
// instruments are shared, so assertions read them directly. Registering a
// second time (any registry) must be a no-op, not a panic.
func TestRegisterIdempotent(t *testing.T) {
	Register(prometheus.NewRegistry())
	Register(prometheus.NewRegistry())
}

func TestBootSetsGaugeAndTicksOnce(t *testing.T) {
	Register(prometheus.NewRegistry())

	Boot(ReasonMemoryOpenFailed)
	Boot(ReasonMemoryOpenFailed) // idempotent state, but the counter is per call

	if v := testutil.ToFloat64(degraded.WithLabelValues(ReasonMemoryOpenFailed)); v != 1 {
		t.Fatalf("gauge %s = %v, want 1", ReasonMemoryOpenFailed, v)
	}
	if v := testutil.ToFloat64(events.WithLabelValues(ReasonMemoryOpenFailed)); v != 2 {
		t.Fatalf("counter %s = %v, want 2 (one per Boot call)", ReasonMemoryOpenFailed, v)
	}
}

func TestEventCountsNeverTouchesGauge(t *testing.T) {
	Register(prometheus.NewRegistry())

	before := testutil.ToFloat64(events.WithLabelValues(ReasonRecallNoHits))
	Event(ReasonRecallNoHits)
	Event(ReasonRecallNoHits)
	if after := testutil.ToFloat64(events.WithLabelValues(ReasonRecallNoHits)); after != before+2 {
		t.Fatalf("counter %s = %v, want %v", ReasonRecallNoHits, after, before+2)
	}
	if v := testutil.ToFloat64(degraded.WithLabelValues(ReasonRecallNoHits)); v != 0 {
		t.Fatalf("gauge %s = %v, want 0 (transient events never set the gauge)", ReasonRecallNoHits, v)
	}
}

// TestReasonsNamesBoundedSet pins the closed reason enum — adding or removing
// a reason is a deliberate act that this test makes visible in review.
func TestReasonsNamesBoundedSet(t *testing.T) {
	got := Reasons()
	want := []string{
		"memory_open_failed",
		"embedder_fallback",
		"indexer_discussion_unavailable",
		"indexer_handoff_unavailable",
		"indexer_workitem_unavailable",
		"recall_unwired",
		"recall_no_hits",
		"recall_no_query",
	}
	if len(got) != len(want) {
		t.Fatalf("Reasons() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Reasons()[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}
