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

	runctrl "github.com/K8squad/K8squad/pkg/controller/run"
	"github.com/K8squad/K8squad/pkg/coord"
)

// runPhaseJournalAdapter binds the Run controller's PhaseJournal seam (WS-2 /
// ISI-5749) to the WS-1 append-only coord write path (coord.RecordRunPhaseTransition
// over the operator's coord pool). It is the only place the controller package's
// PhaseEdge is translated into coord.RunPhaseTransition — the controller stays
// decoupled from the coord write package, exactly like the StepSource /
// SettleSource adapters wrap coord readers.
type runPhaseJournalAdapter struct{ db *sql.DB }

// RecordRunPhaseTransition appends the observed edge to coord.run_phase_transition.
func (a runPhaseJournalAdapter) RecordRunPhaseTransition(ctx context.Context, edge runctrl.PhaseEdge) error {
	return coord.RecordRunPhaseTransition(ctx, a.db, coord.RunPhaseTransition{
		RunUID:    edge.RunUID,
		TeamID:    edge.TeamID,
		ProjectID: edge.ProjectID,
		FromPhase: edge.FromPhase,
		ToPhase:   edge.ToPhase,
		TS:        edge.TS,
	})
}
