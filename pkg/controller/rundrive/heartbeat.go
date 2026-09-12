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

// heartbeat.go — the §6.2/§6.3 claim hygiene sweep (ISI-4183, M1.3). With the
// drive loop now claiming work items, two follow-on duties keep the checkout
// world honest — both level-triggered sweeps in the CancelSweeper shape, so a
// missed tick costs delay, never correctness:
//
//   - RENEW: a held, in-flight claim's lease is short (30s, DefaultProdConfig)
//     so a dead holder cannot wedge an item for long. Real agent runs outlive
//     it by minutes — without renewal, the 3.2 death detector would re-enter
//     every healthy long run as a "dead holder" retry lap. The sweep renews
//     every live in-flight lease each tick.
//   - RELEASE: a Run that reached a terminal step still holds its checkout
//     until someone clears it (the machine commits the step; custody release
//     is bookkeeping after the fact). The sweep releases held terminal rows —
//     custody only: the LANE stays in_progress on success (M1.5's reporting
//     owns lane moves on completion) and returns to todo on failure/cancel
//     (the item is reclaimable — the same lane-return the re-enter paths
//     co-commit).
package rundrive

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/K8squad/K8squad/pkg/reconcile"
)

// HeartbeatInterval is the keepalive tick. It is comfortably inside the 30s
// claim lease (a single missed tick must not lapse a healthy run's lease),
// while the sweep itself is one cheap indexed query over the small claim
// table.
const HeartbeatInterval = 10 * time.Second

// renewer is the §6.2 lease-renewal seam the sweep depends on (satisfied by
// *coord.ProdClaimer; faked in tests).
type renewer interface {
	Renew(ctx context.Context, itemID, principal, runID string, fence int64) bool
}

// HeartbeatSweeper is a manager Runnable: every tick, renew the leases of
// in-flight held claims and release the checkouts of terminal held claims.
type HeartbeatSweeper struct {
	// DB is the coordination Postgres (required).
	DB *sql.DB
	// Claimer executes the §6.2 renew (required).
	Claimer renewer
	// Tick overrides HeartbeatInterval when > 0 (tests shrink it).
	Tick time.Duration
	// Log receives diagnostics (nil discards).
	Log func(format string, args ...any)
}

// heldClaim is one row of the sweep's backlog.
type heldClaim struct {
	workItemID string
	holderRun  string
	principal  string
	fence      int64
	step       reconcile.Step
}

// Start runs the sweep until ctx is done. Errors are logged and retried next
// tick — a transient DB stall must never kill the sweeper (nor the operator:
// the §6.2 Renew contract panics on infrastructure failure, so each call is
// contained and reported as a log line instead).
func (s *HeartbeatSweeper) Start(ctx context.Context) error {
	tick := s.Tick
	if tick <= 0 {
		tick = HeartbeatInterval
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.sweep(ctx)
		}
	}
}

// sweep is one level-triggered pass over every held claim row.
func (s *HeartbeatSweeper) sweep(ctx context.Context) {
	held, err := s.due(ctx)
	if err != nil {
		s.logf("rundrive.heartbeat: %v", err)
		return
	}
	for _, hc := range held {
		switch {
		case reconcile.IsTerminal(hc.step):
			// Terminal Run still holding its checkout: release custody. The
			// guard is the holder run id + terminal step, so the release is
			// idempotent and crash-safe — a re-sweep of an already-released
			// row simply matches nothing.
			if err := s.releaseTerminal(ctx, hc); err != nil {
				s.logf("rundrive.heartbeat: release %s: %v", hc.workItemID, err)
			}
		default:
			// In flight: renew the lease. Renew's own SQL guard
			// (lease_expires_at > clock_timestamp()) refuses an already-lapsed
			// lease — the authoritative liveness check — and a lapsed one is
			// the 3.2 death detector's business on the Run's next pass.
			s.renew(ctx, hc)
		}
	}
}

// due lists every held claim row with its machine step.
func (s *HeartbeatSweeper) due(ctx context.Context) ([]heldClaim, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT work_item_id::text, NULLIF(run_id::text,''), holder_principal, fence_token, reconcile_step
		  FROM coord.claim
		 WHERE holder_principal IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("rundrive.heartbeat: list held claims: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []heldClaim
	for rows.Next() {
		var hc heldClaim
		if err := rows.Scan(&hc.workItemID, &hc.holderRun, &hc.principal, &hc.fence, &hc.step); err != nil {
			return nil, fmt.Errorf("rundrive.heartbeat: scan held claim: %w", err)
		}
		out = append(out, hc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rundrive.heartbeat: held claims cursor: %w", err)
	}
	return out, nil
}

// renew extends one in-flight lease. The §6.2 Renew panics on infrastructure
// failure (its run-loop contract); the sweep contains that panic — a sweeper
// goroutine dying would take the whole operator down over a transient DB
// stall, and the level-triggered next tick is the honest retry.
func (s *HeartbeatSweeper) renew(ctx context.Context, hc heldClaim) {
	defer func() {
		if p := recover(); p != nil {
			s.logf("rundrive.heartbeat: renew %s panicked: %v", hc.workItemID, p)
		}
	}()
	if hc.holderRun == "" {
		return // provenance gap: nothing Renew's (run_id, fence) guard accepts
	}
	if !s.Claimer.Renew(ctx, hc.workItemID, hc.principal, hc.holderRun, hc.fence) {
		// Lost the lease (fence moved / foreign holder / lapsed): the 3.2
		// death detector owns the follow-up on the Run's next pass.
		s.logf("rundrive.heartbeat: lease for %s not renewable (fence %d) — death detection owns it",
			hc.workItemID, hc.fence)
	}
}

// releaseTerminal clears a terminal Run's checkout in one transaction:
// custody (holder/lease/run_id) released, §6.5 claim_released audit + outbox
// co-committed. The LANE returns to todo for failed/cancelled (reclaimable,
// the re-enter paths' discipline) and stays in_progress for succeeded (M1.5's
// reporting owns completion lane moves) — guarded on in_progress, so items a
// human already moved are never disturbed.
func (s *HeartbeatSweeper) releaseTerminal(ctx context.Context, hc heldClaim) error {
	if hc.holderRun == "" {
		return nil // no run provenance to release under
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	if _, err := tx.ExecContext(ctx, `
		UPDATE coord.claim
		   SET holder_principal = NULL,
		       run_id           = NULL,
		       lease_expires_at = NULL
		 WHERE work_item_id = $1::uuid
		   AND run_id        = $2::uuid
		   AND reconcile_step IN ('succeeded','failed','cancelled')`,
		hc.workItemID, hc.holderRun); err != nil {
		return fmt.Errorf("release: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.audit_log
		       (work_item_id, run_id, event_type, principal, fence_token, to_state)
		VALUES ($1::uuid, $2::uuid, 'claim_released', $3, $4, $5)`,
		hc.workItemID, hc.holderRun, hc.principal, hc.fence, string(hc.step)); err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.outbox
		       (entity, project_id, squad, event_type, work_item_id, run_id, payload)
		SELECT 'run', wi.project_id, wi.team_id::text, 'claim_released',
		       wi.id, $2::uuid,
		       jsonb_build_object('fence_token', $3::bigint, 'step', $4::text)
		  FROM coord.work_item wi WHERE wi.id = $1::uuid`,
		hc.workItemID, hc.holderRun, hc.fence, string(hc.step)); err != nil {
		return fmt.Errorf("outbox: %w", err)
	}

	if hc.step != reconcile.StepSucceeded {
		// Failure/cancel returns the item to the claimable lane (idempotent,
		// guarded on in_progress — a human-moved lane is never touched).
		if _, err := tx.ExecContext(ctx, `
			UPDATE coord.work_item
			   SET state = 'todo', updated_at = now()
			 WHERE id = $1::uuid AND state = 'in_progress'`, hc.workItemID); err != nil {
			return fmt.Errorf("lane return: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	s.logf("rundrive.heartbeat: released terminal claim %s (run %s, step %s)",
		hc.workItemID, hc.holderRun, hc.step)
	return nil
}

func (s *HeartbeatSweeper) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}
