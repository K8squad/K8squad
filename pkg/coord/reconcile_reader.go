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

package coord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/K8squad/K8squad/pkg/reconcile"
)

// ReconcileStepReader is the READ side of the §6.4 durable step: it reports the
// committed reconcile_step for a work item WITHOUT taking the fence/audit/outbox
// write path (that is ProdReconcileStore.Advance). The Run status controller
// (pkg/controller/run, ISI-2655 slice-2) uses it to project the durable step onto
// Run.status — a read-only projection must never advance the machine or emit an
// outbox event, so it deliberately shares none of ProdReconcileStore's mutating
// surface. It holds only a *sql.DB and is safe to share across reconciles.
type ReconcileStepReader struct {
	db *sql.DB
}

// NewReconcileStepReader binds a read-only step reader to the coord Postgres pool.
func NewReconcileStepReader(db *sql.DB) *ReconcileStepReader {
	return &ReconcileStepReader{db: db}
}

// StepForWorkItem returns the committed reconcile_step for the coord.claim row
// keyed by workItemID (a Run's spec.workItemRef — the opaque coordination-DB
// pointer, ADR-001; NOT the Run's k8s uid). The contract mirrors the status
// controller's needs:
//
//   - found=false with a nil error means no claim row exists yet — the Run is
//     admitted but not enrolled in coord — which the caller projects as the
//     initial Pending step rather than an error (never a terminal read).
//   - an empty workItemID is treated as not-found without touching the DB (a "" is
//     never a valid uuid key, and the ::uuid cast would otherwise error).
//   - any other failure is returned so the reconciler requeues rather than reading
//     a stalled step as terminal.
func (r *ReconcileStepReader) StepForWorkItem(ctx context.Context, workItemID string) (reconcile.Step, bool, error) {
	if r == nil || r.db == nil {
		return "", false, errors.New("coord.ReconcileStepReader: nil db")
	}
	if workItemID == "" {
		return "", false, nil
	}
	var step string
	err := r.db.QueryRowContext(ctx,
		`SELECT reconcile_step FROM coord.claim WHERE work_item_id = $1::uuid`,
		workItemID).Scan(&step)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("coord.ReconcileStepReader.StepForWorkItem: %w", err)
	}
	return reconcile.Step(step), true, nil
}
