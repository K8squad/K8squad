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

package run

import (
	"context"
	"time"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// phasejournal.go — WS-2 of ISI-5744 (ISI-5749; ADR-0032): the Run controller's
// emission half of the run phase-transition journal. On each COMMITTED Run phase
// transition the projector appends one immutable row to the WS-1 append-only
// store (coord.run_phase_transition) through the PhaseJournal seam, so the
// fleet "runs by status over time" series (ISI-5738 §3) has an in-cluster history
// to reconstruct from — etcd keeps only the live phase (ADR-0032).
//
// WS-3 (ISI-5750) reconstructs daily buckets from these rows; WS-4 (ISI-5752) is
// the FE chart. This file ships only the emission: the CR-phase → chip-token
// mapping, the tenancy derivation from the Run CR, and the idempotency guard.

// Run phase CHIP TOKENS — the four values coord.run_phase_transition stores (its
// CHECK enum) and the FleetRunPhaseHistorySource reconstruction keys on
// (claiming | running | complete | failed, lowercased; see
// db/migrations/0037 and internal/apiserver/squadseries.go RunsSeriesTile). The
// journal stores the NORMALIZED display token, NOT the raw 8-value Run CR phase,
// so WS-3's reconstruction keys line up with coord.StatusSnapshot.Counts with no
// re-mapping. These mirror pkg/coord.RunPhase{Claiming,Running,Complete,Failed};
// they are duplicated here (not imported) to keep the controller decoupled from
// the coord write package — the production PhaseJournal adapter owns that edge.
const (
	phaseTokenClaiming = "claiming"
	phaseTokenRunning  = "running"
	phaseTokenComplete = "complete"
	phaseTokenFailed   = "failed"
)

// phaseToken folds the Run CR's 8-value RunPhase state machine (arch §8:
// Pending/Claiming/Running/Paused/Canceling/Succeeded/Failed/Cancelled) onto the
// four chip tokens the journal + chart use. ok=false ⇒ the phase has no chart
// representation (the empty phase of an unrecognised durable step — PhaseOf's
// fail-closed default) and MUST NOT be emitted (it would violate the CHECK enum).
//
// The fold (documented so WS-3/WS-4 read the same intent):
//   - Pending, Claiming → claiming   (the run is being set up, not yet executing;
//     there is no separate "pending" band, so admitted-but-unclaimed folds into
//     the earliest setup band)
//   - Running, Paused   → running     (actively executing, or a non-terminal hold
//     mid-execution — still an in-flight run, never a terminal bucket)
//   - Succeeded         → complete    (terminal success; anchors the chart base)
//   - Failed, Canceling, Cancelled → failed  (terminal failure, or an operator
//     kill in progress/landed — grouped with the failure band exactly as the
//     console's run-hue authority groups canceling/canceled/failed, SquadOverview
//     phaseTone)
func phaseToken(p api.RunPhase) (string, bool) {
	switch p {
	case api.RunPhasePending, api.RunPhaseClaiming:
		return phaseTokenClaiming, true
	case api.RunPhaseRunning, api.RunPhasePaused:
		return phaseTokenRunning, true
	case api.RunPhaseSucceeded:
		return phaseTokenComplete, true
	case api.RunPhaseFailed, api.RunPhaseCanceling, api.RunPhaseCancelled:
		return phaseTokenFailed, true
	default:
		return "", false
	}
}

// PhaseEdge is one observed Run phase edge the projector hands the journal
// seam. It carries the chip TOKENS (already folded by phaseToken), the Run CR's
// owning tenancy UIDs (derived from the Run CR, NOT any linked work_item — not
// every Run has one, ADR-0032), and the observed time. FromPhase is empty on the
// Run's first observed transition (stored NULL, like coord.audit_log.from_state).
type PhaseEdge struct {
	RunUID    string
	TeamID    string
	ProjectID string
	FromPhase string
	ToPhase   string
	TS        time.Time
}

// PhaseJournal appends one observed Run phase edge to the durable append-only
// phase-transition journal (WS-1 coord.run_phase_transition, via
// pkg/coord.RecordRunPhaseTransition). It is an OPTIONAL projector seam, exactly
// like Settlement: nil disables emission (unit tests of the pure projection, or a
// deployment without the journal wired) so the controller is strictly
// non-regressing without it. A write failure is logged, never returned — the
// journal is observability history, and a failed append MUST NOT fail or requeue
// the status reconcile that owns the live phase.
type PhaseJournal interface {
	RecordRunPhaseTransition(ctx context.Context, edge PhaseEdge) error
}
