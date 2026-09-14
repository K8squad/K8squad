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

// a2areattach.go — the read side of the leader-elect follow re-attach
// (ADR-0020 §5 option (a), ISI-4348-S4). It answers one question: which
// dispatch laps have an in-flight agent whose background follow this process
// must re-open?
//
// WHY (ADR-0020 §4 last row, §5): the durable marker (S1) + reaper (S2) close
// every restart case EXCEPT "restart during work, agent finishes post-restart".
// There the follow that would write the marker died with the old process and
// the new one never re-attaches, so the marker is never written and the pod
// leaks. S4 closes it: on leader-elect, re-open Client.Follow against the
// still-serving pod supervisor (pods keep serving after runtime exit,
// ADR-0007). That restores the live OnDone → pool.Release path AND eventually
// writes the S1 marker. This file only SELECTS the laps to re-attach; the
// re-open itself is the operator's a2a.Dispatcher.Submit (idempotent C1
// reattach — never a second execution), driven by rundrive.ReattachFollows.
package coord

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ReattachTarget identifies one unsettled dispatch lap whose background follow
// must be re-opened after a leader-elect. The pair is exactly what
// a2a.Dispatcher.Submit needs: a2aTaskID keys the transport reattach (C1) and
// runID rebuilds the Task deterministically (the Run CR survives the restart).
type ReattachTarget struct {
	A2ATaskID string
	RunID     string
}

// ProdReattachReader lists the unsettled dispatch laps eligible for follow
// re-attach. Read-only; safe for concurrent use.
type ProdReattachReader struct {
	db *sql.DB
}

// NewProdReattachReader binds the reader to the coord Postgres.
func NewProdReattachReader(db *sql.DB) (*ProdReattachReader, error) {
	if db == nil {
		return nil, errors.New("coord.NewProdReattachReader: nil db")
	}
	return &ProdReattachReader{db: db}, nil
}

// UnsettledDispatches returns the latest unsettled dispatch lap per work item
// whose agent may still be working — the leak-candidate set S4 re-attaches.
//
// The filter is the precise "settled_at IS NULL AND run non-terminal-agent" of
// ADR-0020 §5(a), read against the coord schema:
//
//   - settled_at IS NULL — the follow was never durably observed to complete.
//     This is the only durable "agent not proven finished" signal (S1): a
//     succeeded RECONCILE step does NOT mean the agent is done (dispatch is
//     fire-and-forget, so the ledger reaches succeeded while the agent still
//     works — exactly the leak case), so this filter must NOT look at
//     reconcile_step == 'succeeded'.
//   - reconcile_step NOT IN ('cancelled','failed') — these terminal paths
//     already OWN the sandbox teardown (KillRun's cancel path; the driver's
//     §9.3 dead-run retry teardown), so re-attaching would fight them and
//     re-spawn a shim subprocess for a pod that is being reclaimed. 'succeeded'
//     and every in-flight step (dispatching/running/…) stay in the set.
//   - dispatched_at within window — the supervisor's session-retention bound
//     (ADR-0007): a supervisor that stopped serving long ago is unreachable, so
//     a re-attach would only fail Begin. window should be >= the max run
//     duration so a genuinely long-running agent is never skipped.
//
// One work item has at most one live agent; retry laps share work_item_id with
// a later a2a_task_id ('run_id#lapN', §8), so DISTINCT ON (work_item_id) picks
// the newest lap — the one that would carry the live follow. A window <= 0
// returns no rows (re-attach disabled) rather than scanning the whole table.
func (r *ProdReattachReader) UnsettledDispatches(ctx context.Context, window time.Duration) ([]ReattachTarget, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if window <= 0 {
		return nil, nil
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT ON (d.work_item_id) d.a2a_task_id, d.run_id::text
		  FROM coord.a2a_dispatch d
		  JOIN coord.claim c ON c.work_item_id = d.work_item_id
		 WHERE d.settled_at IS NULL
		   AND d.dispatched_at > now() - make_interval(secs => $1)
		   AND c.reconcile_step NOT IN ('cancelled', 'failed')
		 ORDER BY d.work_item_id, d.dispatched_at DESC, d.a2a_task_id DESC`,
		window.Seconds())
	if err != nil {
		return nil, fmt.Errorf("coord.ProdReattachReader.UnsettledDispatches: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ReattachTarget
	for rows.Next() {
		var t ReattachTarget
		if err := rows.Scan(&t.A2ATaskID, &t.RunID); err != nil {
			return nil, fmt.Errorf("coord.ProdReattachReader.UnsettledDispatches: scan: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("coord.ProdReattachReader.UnsettledDispatches: rows: %w", err)
	}
	return out, nil
}
