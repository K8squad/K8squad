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

package main

import (
	"context"
	"database/sql"
	"errors"
	"sync"

	runctrl "github.com/K8squad/K8squad/pkg/controller/run"
	"github.com/K8squad/K8squad/pkg/coord"
	ctrl "sigs.k8s.io/controller-runtime"
)

// runPhaseJournalAdapter binds the Run controller's PhaseJournal seam (WS-2 /
// ISI-5749) to the WS-1 append-only coord write path (coord.RecordRunPhaseTransition
// over the operator's coord pool). It is the only place the controller package's
// PhaseEdge is translated into coord.RunPhaseTransition — the controller stays
// decoupled from the coord write package, exactly like the StepSource /
// SettleSource adapters wrap coord readers.
//
// Same-edge suppressor (ISI-7502 / ISI-7498 WS-C): the controller already gates
// emission on a committed CR phase change (desired.Phase != prevPhase), but a Run
// whose CR phase FLAPS between two values across reconcile passes re-presents the
// same folded token edge every pass. The 16:28Z incident caught run 97a0ddc9
// storming 449 claiming→running edges in 76s (~6/s) this way. This adapter keeps
// the last folded edge it recorded per Run UID and drops a CONSECUTIVE identical
// edge, so a flapping-but-healthy run contributes one row per real token change,
// not one per reconcile. A DIFFERENT edge for the same run (a genuine progression,
// or a legitimate token back-and-forth) always records and resets the memory. The
// map is bounded to live runs: a terminal edge (complete/failed) records and then
// evicts its run's entry, since the controller emits no further edge once a Run is
// terminal (its phase stops changing).
type runPhaseJournalAdapter struct {
	db *sql.DB

	mu       sync.Mutex
	lastEdge map[string]string // run UID -> last recorded "from\x00to" folded edge
}

// terminal folded tokens mirror pkg/controller/run.phaseToken: Succeeded → complete,
// Failed/Canceling/Cancelled → failed. Duplicated (not imported) to keep the operator
// binary decoupled from the controller's internal token constants, exactly like the
// PhaseEdge translation itself.
const (
	phaseTokenComplete = "complete"
	phaseTokenFailed   = "failed"
)

func newRunPhaseJournalAdapter(db *sql.DB) *runPhaseJournalAdapter {
	return &runPhaseJournalAdapter{db: db, lastEdge: make(map[string]string)}
}

// RecordRunPhaseTransition appends the observed edge to coord.run_phase_transition,
// suppressing a consecutive identical edge for the same Run (see type doc).
func (a *runPhaseJournalAdapter) RecordRunPhaseTransition(ctx context.Context, edge runctrl.PhaseEdge) error {
	key := edge.FromPhase + "\x00" + edge.ToPhase

	a.mu.Lock()
	if a.lastEdge[edge.RunUID] == key {
		a.mu.Unlock()
		return nil // same edge as last recorded for this run — storm-guard drop
	}
	a.mu.Unlock()

	if err := coord.RecordRunPhaseTransition(ctx, a.db, coord.RunPhaseTransition{
		RunUID:     edge.RunUID,
		TeamID:     edge.TeamID,
		ProjectID:  edge.ProjectID,
		WorkItemID: edge.WorkItemID,
		FromPhase:  edge.FromPhase,
		ToPhase:    edge.ToPhase,
		TS:         edge.TS,
	}); err != nil {
		if errors.Is(err, coord.ErrRunPhaseEdgeSuppressed) {
			// ISI-7541 backward-edge guard: the edge regressed (or re-entered the
			// band the journal already holds) without a durable fence bump — a
			// crash-path flap, not retry history. Not a failure: swallow it, and
			// leave lastEdge at the last RECORDED edge so the ISI-7502 storm guard
			// above keeps dropping the flap's repeats without a DB round-trip.
			ctrl.LoggerFrom(ctx).V(1).Info("run phase journal: suppressed crash-path backward edge",
				"runUID", edge.RunUID, "from", edge.FromPhase, "to", edge.ToPhase)
			return nil
		}
		// Leave lastEdge untouched on a write failure so a retry of the same edge is
		// not suppressed — the row never landed, so it is not a duplicate.
		return err
	}

	a.mu.Lock()
	if edge.ToPhase == phaseTokenComplete || edge.ToPhase == phaseTokenFailed {
		delete(a.lastEdge, edge.RunUID) // terminal: no further edge, free the slot
	} else {
		a.lastEdge[edge.RunUID] = key
	}
	a.mu.Unlock()
	return nil
}
