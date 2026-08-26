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

package apiserver

import (
	"context"
	"database/sql"

	"github.com/K8squad/K8squad/pkg/coord"
)

// ProdRunKiller is the production RunKiller: the coord ProdCancelStore
// CancelEnter over the host DB, with a bounded re-read loop for fence
// conflicts (a retry lap raced the kill — re-read the new fence and enter
// again; the terminal guard makes double-entry safe).
type ProdRunKiller struct {
	store *coord.ProdCancelStore
}

// NewProdRunKiller binds the kill seam over the coord pool.
func NewProdRunKiller(db *sql.DB) *ProdRunKiller {
	return &ProdRunKiller{store: coord.NewProdCancelStore(db)}
}

// killConflictBudget bounds the fence-conflict retry loop (three fresh reads
// covers a racing retry lap; more is a hot loop the operator should see).
const killConflictBudget = 3

// Kill implements RunKiller: read the claim, CancelEnter fence-first; on
// conflict re-read and retry within budget. Idempotent by outcome — a Run
// already cancelling re-enters harmlessly only if the fence still holds,
// otherwise reports the observed phase.
func (k *ProdRunKiller) Kill(ctx context.Context, workItemID, initiatedBy string) (string, error) {
	for attempt := 0; attempt < killConflictBudget; attempt++ {
		cs, found, err := k.store.State(ctx, workItemID)
		if err != nil {
			return "", err
		}
		if !found {
			return "Missing", ErrKillNotFound
		}
		outcome, err := k.store.CancelEnter(ctx, workItemID, "", "ksquad-apiserver", initiatedBy, cs.Fence)
		if err != nil {
			return "", err
		}
		switch outcome {
		case coord.CancelAccepted:
			return "Canceling", nil
		case coord.CancelTerminal:
			return cs.Step, nil // already terminal — report, never resurrect (AC5)
		case coord.CancelConflict:
			continue // fence moved: re-read and retry
		}
	}
	return "", ErrKillConflict
}

var _ RunKiller = (*ProdRunKiller)(nil)
