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

package contextsource

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/K8squad/K8squad/internal/memory"
	"github.com/K8squad/K8squad/pkg/recallhealth"
)

// degradedRecaller stubs MemoryRecaller with canned hits so the recall-tier
// degradation observations (ISI-7228 GAP-5) can be asserted without a store.
type degradedRecaller struct {
	hits []memory.RecallHit
}

func (f degradedRecaller) ScopedRecall(_ context.Context, _ string, _ *string, _ string, _ int) ([]memory.RecallHit, error) {
	return f.hits, nil
}

func (f degradedRecaller) ScopedRecallByIDs(_ context.Context, _ string, _ *string, _ []string) ([]memory.RecallHit, error) {
	return f.hits, nil
}

// eventCounts scrapes the registry the package registered on and returns the
// ksquad_recall_degraded_events_total counter values by reason.
func eventCounts(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]float64{}
	for _, mf := range families {
		if mf.GetName() != "ksquad_recall_degraded_events_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "reason" {
					out[l.GetValue()] = m.GetCounter().GetValue()
				}
			}
		}
	}
	return out
}

// TestMemoryRecallSurfacesDegradedReasons (ISI-7228 GAP-5): every path that
// leaves the untrusted-recall tier empty must tick
// ksquad_recall_degraded_events_total with the reason that distinguishes
// "recall disabled" from "no relevant memory" — while returning nil, nil
// exactly as before (fail-open behavior unchanged).
func TestMemoryRecallSurfacesDegradedReasons(t *testing.T) {
	reg := prometheus.NewRegistry()
	recallhealth.Register(reg)

	// nil memory: tier empty because recall is UNWIRED.
	s := &Source{}
	if docs, err := s.MemoryRecall(context.Background(), "team-1", "proj-1", "query", nil, 5); err != nil || docs != nil {
		t.Fatalf("nil-memory MemoryRecall = (%v, %v), want (nil, nil)", docs, err)
	}

	// fresh arm, empty query text: recall skipped rather than served.
	s = &Source{memory: degradedRecaller{}}
	if docs, err := s.MemoryRecall(context.Background(), "team-1", "proj-1", "", nil, 5); err != nil || docs != nil {
		t.Fatalf("empty-query MemoryRecall = (%v, %v), want (nil, nil)", docs, err)
	}

	// fresh arm, real query, zero hits: tier empty while memory exists.
	if docs, err := s.MemoryRecall(context.Background(), "team-1", "proj-1", "query", nil, 5); err != nil {
		t.Fatalf("zero-hits MemoryRecall err = %v", err)
	} else if len(docs) != 0 {
		t.Fatalf("zero-hits MemoryRecall docs = %v, want empty", docs)
	}

	counts := eventCounts(t, reg)
	want := map[string]float64{
		recallhealth.ReasonRecallUnwired: 1,
		recallhealth.ReasonRecallNoQuery: 1,
		recallhealth.ReasonRecallNoHits:  1,
	}
	for reason, n := range want {
		if got := counts[reason]; got != n {
			t.Errorf("events[%s] = %v, want %v", reason, got, n)
		}
	}

	// healthy path: a hit arrives and no NEW degradation ticks (the pinned
	// arm with zero hits is snapshot decay, never an error — also unticked).
	s = &Source{memory: degradedRecaller{hits: []memory.RecallHit{{RecordID: "rec-1"}}}}
	if docs, err := s.MemoryRecall(context.Background(), "team-1", "proj-1", "query", nil, 5); err != nil {
		t.Fatalf("healthy MemoryRecall err = %v", err)
	} else if len(docs) != 1 {
		t.Fatalf("healthy MemoryRecall docs = %d, want 1", len(docs))
	}
	if _, err := s.MemoryRecall(context.Background(), "team-1", "proj-1", "query", []string{"rec-1"}, 5); err != nil {
		t.Fatalf("pinned MemoryRecall err = %v", err)
	}
	// pinned arm, zero hits: snapshot decay — documented as never an error,
	// so it must NOT tick recall_no_hits (GAP-5 marks degradation, not decay).
	decay := &Source{memory: degradedRecaller{}}
	if docs, err := decay.MemoryRecall(context.Background(), "team-1", "proj-1", "query", []string{"gone-1"}, 5); err != nil || docs == nil {
		t.Fatalf("pinned-decay MemoryRecall = (%v, %v), want (nil, nil)", docs, err)
	}
	after := eventCounts(t, reg)
	for reason, n := range want {
		if got := after[reason]; got != n {
			t.Errorf("events[%s] = %v after healthy calls, want unchanged %v", reason, got, n)
		}
	}
}
