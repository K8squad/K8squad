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

// Package recallhealth surfaces recall degradation as bounded Prometheus
// instruments (ISI-7228 GAP-5 / ISI-7233). A degraded recall tier — the memory
// store that will not open at operator boot, the HashingEmbedder fallback, a
// projection indexer that failed to start in ksquad-memory, or an assembly
// whose untrusted-recall tier came back empty — is fail-open BY DESIGN
// (degraded is not an error; recall must never gate a Run), and until now
// that also meant SILENT: an operator could not tell "no relevant memory"
// from "recall disabled". This package makes the state visible without
// changing a byte of recall behavior.
//
// The two instruments are BOUNDED — one gauge + one counter over a fixed
// reason enum, NO per-team/per-project/per-run labels — the same NFR-OBS3
// cardinality firewall the coord sweeper and event-seam metrics obey (the
// authoritative per-assembly record is the pinned context snapshot, never a
// metric):
//
//	ksquad_recall_degraded               gauge{reason}    1 while the reason holds
//	ksquad_recall_degraded_events_total  counter{reason}  every degradation observed
//
// Boot marks a process-lifetime state (gauge set to 1, counter ticks once);
// Event counts a transient per-assembly observation (gauge untouched — a
// flapping tier would make a standing 1 meaningless). Until Register is
// called both are no-ops, so libraries (contextsource) can emit
// unconditionally and binaries/tests that never register stay silent.
package recallhealth

import (
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

// Degradation reasons — the closed label set. Emitters MUST use these
// constants; adding a value is a deliberate, reviewed act (NFR-OBS3).
const (
	// ReasonMemoryOpenFailed: the operator could not open the memory store at
	// boot (pgvector absent, migration gap) — Deps.Memory stays nil and every
	// assembly's untrusted-recall tier is empty. Persistent.
	ReasonMemoryOpenFailed = "memory_open_failed"
	// ReasonEmbedderFallback: no embedder endpoint is configured, so the
	// deterministic local HashingEmbedder is active (the §7.1 seam) — recall
	// still serves, with reduced semantic quality. Persistent. The
	// configure-a-real-embedder-or-ratify-hashing decision itself is GAP-4;
	// this only makes the current state visible.
	ReasonEmbedderFallback = "embedder_fallback"
	// ReasonIndexerDiscussionUnavailable: the discussion-to-memory projection
	// indexer failed to start in ksquad-memory — the tier it feeds stays
	// empty (projection seam unreachable). Persistent.
	ReasonIndexerDiscussionUnavailable = "indexer_discussion_unavailable"
	// ReasonIndexerHandoffUnavailable: the handoff-to-memory mirror failed to
	// start in ksquad-memory. Persistent.
	ReasonIndexerHandoffUnavailable = "indexer_handoff_unavailable"
	// ReasonIndexerWorkItemUnavailable: the work-item-to-memory indexer
	// failed to start in ksquad-memory. Persistent.
	ReasonIndexerWorkItemUnavailable = "indexer_workitem_unavailable"
	// ReasonRecallUnwired: an assembly ran with NO memory service wired — its
	// untrusted-recall tier is empty because recall is disabled, not because
	// nothing is relevant. Transient (per assembly).
	ReasonRecallUnwired = "recall_unwired"
	// ReasonRecallNoHits: a fresh scoped recall with a non-empty query
	// returned zero docs — the tier is empty while memory exists. Transient.
	ReasonRecallNoHits = "recall_no_hits"
	// ReasonRecallNoQuery: a fresh assembly carried no query text (a title-
	// and body-less work item), so recall was skipped rather than served.
	// Transient.
	ReasonRecallNoQuery = "recall_no_query"
)

var (
	registerOnce sync.Once
	wired        atomic.Bool

	degraded = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ksquad_recall_degraded",
		Help: "1 while a recall-degradation reason holds (ISI-7228 GAP-5: degraded recall is fail-open but never silent). Absent or 0 means healthy.",
	}, []string{"reason"})

	events = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ksquad_recall_degraded_events_total",
		Help: "Recall-degradation observations by reason: boot-time states tick once per process; transient tier-empty observations tick per assembly.",
	}, []string{"reason"})
)

// Register wires the instruments on reg. The operator passes the
// controller-runtime metrics registry (served at its /metrics endpoint);
// ksquad-memory passes its own registry, served at /metrics on the health
// port. Idempotent; the first call wins.
func Register(reg prometheus.Registerer) {
	registerOnce.Do(func() {
		reg.MustRegister(degraded, events)
		wired.Store(true)
	})
}

// Boot marks a process-lifetime degradation: the reason holds from now until
// the process restarts, so the gauge is set to 1 and the counter ticks once.
func Boot(reason string) {
	if !wired.Load() {
		return
	}
	degraded.WithLabelValues(reason).Set(1)
	events.WithLabelValues(reason).Inc()
}

// Event counts a transient degradation observation (per assembly). The gauge
// is deliberately untouched: only standing states hold a 1.
func Event(reason string) {
	if !wired.Load() {
		return
	}
	events.WithLabelValues(reason).Inc()
}

// Reasons names the closed reason set — the completeness contract (the
// coord-metrics Signals discipline): a consumer can assert the scrape
// exposes every reason this package defines.
func Reasons() []string {
	return []string{
		ReasonMemoryOpenFailed,
		ReasonEmbedderFallback,
		ReasonIndexerDiscussionUnavailable,
		ReasonIndexerHandoffUnavailable,
		ReasonIndexerWorkItemUnavailable,
		ReasonRecallUnwired,
		ReasonRecallNoHits,
		ReasonRecallNoQuery,
	}
}
