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

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/reconcile"
	"github.com/K8squad/K8squad/pkg/sandbox"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// runExistenceChecker answers "does the backing Run CR still exist?" (ISI-5184).
// Blind renewal kept zombie claims alive for Run CRs deleted out from under the
// sweep — the verifier refuses to renew custody whose provenance is gone and
// hands the row to the orphan-release path instead.
type runExistenceChecker interface {
	RunExists(ctx context.Context, runID string) (bool, error)
}

// LiveRuns is the production runExistenceChecker: an uncached API-reader Get
// keyed on the Run's UID. Delete-then-recreate is indistinguishable from
// existence by name, so the check is UID-exact — a recreated Run with the same
// name is a DIFFERENT run and must not legitimize the old claim.
type LiveRuns struct {
	Reader client.Reader
}

// RunExists reports whether a Run CR with the given UID exists in any watched
// namespace. A malformed runID is treated as non-existent (a claim with broken
// provenance is an orphan by definition).
func (l *LiveRuns) RunExists(ctx context.Context, runID string) (bool, error) {
	if l == nil || l.Reader == nil || runID == "" {
		return false, nil
	}
	runs := &ksquadv1alpha1.RunList{}
	if err := l.Reader.List(ctx, runs); err != nil {
		return false, fmt.Errorf("rundrive.heartbeat: list runs: %w", err)
	}
	for i := range runs.Items {
		if string(runs.Items[i].UID) == runID {
			return true, nil
		}
	}
	return false, nil
}

// runLivenessChecker answers "does this Run still have a live sandbox pod?" for
// the zombie-run gate (ISI-5438). A Run CR can outlive its sandbox — the pod is
// evicted, the node is lost, or a bind never completed — yet the Run stays in a
// non-terminal step with its coord.claim lease renewed. The ISI-5184 RunExists
// gate only catches a DELETED Run CR; a zombie whose CR still exists but whose
// pod is gone was renewed forever, so the 3.2 death detector never fired and the
// workspace stayed wedged (the 34 live Claiming/Running zombies on bmad-demo-project
// with no backing pod). Nil keeps the pre-ISI-5438 behavior (renew blindly).
type runLivenessChecker interface {
	HasLiveSandbox(ctx context.Context, runID string) (bool, error)
}

// HasLiveSandbox reports whether any non-terminating, non-terminal sandbox pod
// carries the ksquad.io/run label for the given Run UID. Zero matching live pods
// ⇒ the Run's sandbox is gone. A list error is returned to the caller, which
// fails SAFE by keeping the claim (ISI-5438: never release a zombie on a maybe —
// the warm-pool reaper's "never reap on a maybe" discipline applied to custody).
func (l *LiveRuns) HasLiveSandbox(ctx context.Context, runID string) (bool, error) {
	if l == nil || l.Reader == nil || runID == "" {
		return false, nil
	}
	pods := &corev1.PodList{}
	if err := l.Reader.List(ctx, pods, client.MatchingLabels{sandbox.LabelRun: runID}); err != nil {
		return false, fmt.Errorf("rundrive.heartbeat: list sandbox pods for run %s: %w", runID, err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue // terminating — not live
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue // terminated — not live
		}
		// Pending (scheduling/pulling) and Running both count as live: a pod that
		// physically exists is NOT a zombie, so a capacity-starved Claiming run is
		// never wrongly reaped (only a Run with NO pod at all is a zombie).
		return true, nil
	}
	return false, nil
}

// TerminalOrGone reports whether the Run owning a sandbox pod has reached a
// terminal phase (Succeeded/Failed/Cancelled) or no longer exists (ISI-5438).
// A sandbox whose Run is terminal is LEAKED — the a2a-settlement reaper
// (warmpool.reapIfSettled) misses it because a Cancelled run never writes an
// a2a settled_at, so the in-memory Pool.Release is the only teardown and it
// no-ops after an operator restart (the live 2d5h leaked `sandbox-a16f11a0…`
// held the project RWO PVC hostage). The warm-pool terminal reaper uses this to
// garbage-collect such pods. The UID is read from the pod's AnnRunID stamp, so
// a delete-then-recreate with the same name is a DIFFERENT run and the stale
// pod is correctly judged gone. Fails CLOSED (false,nil) on an empty runID.
func (l *LiveRuns) TerminalOrGone(ctx context.Context, runID string) (bool, error) {
	if l == nil || l.Reader == nil || runID == "" {
		return false, nil // no provable run id ⇒ cannot decide ⇒ keep
	}
	runs := &ksquadv1alpha1.RunList{}
	if err := l.Reader.List(ctx, runs); err != nil {
		return false, fmt.Errorf("rundrive.heartbeat: list runs: %w", err)
	}
	for i := range runs.Items {
		if string(runs.Items[i].UID) != runID {
			continue
		}
		switch runs.Items[i].Status.Phase {
		case ksquadv1alpha1.RunPhaseSucceeded,
			ksquadv1alpha1.RunPhaseFailed,
			ksquadv1alpha1.RunPhaseCancelled:
			return true, nil // terminal: its sandbox is leaked
		default:
			return false, nil // Pending/Claiming/Running/Paused/Canceling: keep
		}
	}
	return true, nil // Run CR with this UID is gone: the sandbox is orphaned
}

// HeartbeatInterval is the keepalive tick. It is comfortably inside the 30s
// claim lease (a single missed tick must not lapse a healthy run's lease),
// while the sweep itself is one cheap indexed query over the small claim
// table.
const HeartbeatInterval = 10 * time.Second

// DefaultZombieStallGrace is how long a non-terminal held claim may go WITHOUT a
// live sandbox pod before the sweep treats it as a zombie and stops renewing
// (ISI-5438). It must comfortably exceed a cold sandbox boot (image pull +
// schedule + Ready) so a legitimately-Claiming Run is never reaped mid-boot; the
// zombies this targets have been pod-less for days, so a generous grace costs
// nothing. Measured from coord.claim.acquired_at.
const DefaultZombieStallGrace = 10 * time.Minute

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
	// Runs verifies the backing Run CR still exists before renewal (ISI-5184).
	// Nil keeps the pre-ISI-5184 blind-renew behavior (tests, legacy wiring).
	Runs runExistenceChecker
	// Live verifies the backing Run still has a live sandbox pod before renewal
	// (ISI-5438). Nil keeps the pre-ISI-5438 behavior (renew any live Run CR).
	Live runLivenessChecker
	// ZombieStallGrace overrides DefaultZombieStallGrace when > 0 (tests shrink
	// it). A pod-less in-flight claim older than this is released as a zombie.
	ZombieStallGrace time.Duration
	// Now overrides the clock for the stall measurement (nil ⇒ time.Now).
	Now func() time.Time
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
	// acquiredAt is when custody was taken; the zombie stall grace (ISI-5438) is
	// measured from it. Zero when NULL (legacy rows) — such a row is never
	// flagged as a zombie (the stall check fails closed without an anchor).
	acquiredAt time.Time
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
			// In flight: renew the lease — but only if the backing Run CR
			// still exists (ISI-5184). Blind renewal wedged workspaces behind
			// zombie claims whose Run had been deleted with no terminal step
			// ever committed: the sweep renewed forever, the lane stayed
			// in_progress, and the busy fallback served an empty listing.
			if s.Runs != nil && hc.holderRun != "" {
				live, err := s.Runs.RunExists(ctx, hc.holderRun)
				if err != nil {
					// Verification unavailable: fail safe by NOT renewing —
					// a skipped tick costs delay (the 3.2 death detector can
					// still reclaim a lapsed lease), renewing a zombie does
					// not. Retry is the next level-triggered tick.
					s.logf("rundrive.heartbeat: verify %s run %s: %v — renewal skipped this tick",
						hc.workItemID, hc.holderRun, err)
					continue
				}
				if !live {
					if err := s.releaseOrphan(ctx, hc); err != nil {
						s.logf("rundrive.heartbeat: orphan-release %s: %v", hc.workItemID, err)
					} else {
						s.logf("rundrive.heartbeat: released orphan claim %s (run %s no longer exists, step %s)",
							hc.workItemID, hc.holderRun, hc.step)
					}
					continue
				}
			}
			// ISI-5438 zombie gate: the Run CR exists (or verification is
			// off) — but does it still have a live sandbox pod? A Run wedged in a
			// non-terminal step with NO pod past the stall grace is a zombie
			// whose lease blind renewal keeps fresh forever — the 3.2 death
			// detector never fires because the lease never lapses. Stop
			// renewing and release it as failed (lane → todo, reclaimable).
			// Paused runs are skipped: park() keeps their pod and a long
			// credential/rate-limit wait is intentional, not a zombie.
			if s.Live != nil && hc.holderRun != "" && !isPausedStep(hc.step) && s.stalled(hc) {
				live, err := s.Live.HasLiveSandbox(ctx, hc.holderRun)
				if err != nil {
					// Pod liveness unknown: never release on a maybe — fall
					// through to renew and retry next level-triggered tick.
					s.logf("rundrive.heartbeat: sandbox liveness for %s run %s: %v — renewing, zombie check deferred",
						hc.workItemID, hc.holderRun, err)
				} else if !live {
					if err := s.releaseZombie(ctx, hc); err != nil {
						s.logf("rundrive.heartbeat: zombie-release %s: %v", hc.workItemID, err)
					} else {
						s.logf("rundrive.heartbeat: released zombie claim %s (run %s has no live sandbox pod, step %s, held %s)",
							hc.workItemID, hc.holderRun, hc.step, s.clock().Sub(hc.acquiredAt).Round(time.Second))
					}
					continue
				}
			}
			// Renew's own SQL guard (lease_expires_at > clock_timestamp())
			// refuses an already-lapsed lease — the authoritative liveness
			// check — and a lapsed one is the 3.2 death detector's business
			// on the Run's next pass.
			s.renew(ctx, hc)
		}
	}
}

// due lists every held claim row with its machine step.
func (s *HeartbeatSweeper) due(ctx context.Context) ([]heldClaim, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT work_item_id::text, NULLIF(run_id::text,''), holder_principal, fence_token, reconcile_step, acquired_at
		  FROM coord.claim
		 WHERE holder_principal IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("rundrive.heartbeat: list held claims: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []heldClaim
	for rows.Next() {
		var hc heldClaim
		var acquired sql.NullTime
		if err := rows.Scan(&hc.workItemID, &hc.holderRun, &hc.principal, &hc.fence, &hc.step, &acquired); err != nil {
			return nil, fmt.Errorf("rundrive.heartbeat: scan held claim: %w", err)
		}
		if acquired.Valid {
			hc.acquiredAt = acquired.Time
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
// co-committed. The LANE returns to todo for failed and moves to the cancelled
// terminal for cancelled (ISI-4489), and stays in_progress for succeeded (M1.5's
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
		// Failure/cancel moves the item off in_progress via the SAME
		// terminal-step → lane mapping the settle uses (coord.SettleLaneOf):
		// failed → todo (claimable again), cancelled → cancelled (terminal,
		// ISI-4489). Idempotent, guarded on in_progress — a human-moved lane is
		// never touched.
		if lane := coord.SettleLaneOf(string(hc.step)); lane != "" {
			if _, err := tx.ExecContext(ctx, `
				UPDATE coord.work_item
				   SET state = $2, updated_at = now()
				 WHERE id = $1::uuid AND state = 'in_progress'`, hc.workItemID, lane); err != nil {
				return fmt.Errorf("lane return: %w", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	s.logf("rundrive.heartbeat: released terminal claim %s (run %s, step %s)",
		hc.workItemID, hc.holderRun, hc.step)
	return nil
}

// releaseOrphan clears a held claim whose backing Run CR no longer exists
// (ISI-5184): custody (holder/lease/run_id) released in one transaction with a
// claim_released audit row whose to_state records the orphan verdict. The
// reconcile_step of a dead run is untrustworthy provenance, so the LANE is
// deliberately NOT settled here — items whose machine state cannot be
// re-derived fall back to in_progress=false only via the guard below: the lane
// returns to todo (reclaimable) exactly as the terminal-failed path does,
// because a Run that vanished without a terminal commit is, from the machine's
// point of view, a failed holder. Human-moved lanes are never disturbed.
func (s *HeartbeatSweeper) releaseOrphan(ctx context.Context, hc heldClaim) error {
	return s.releaseDead(ctx, hc, "orphan_run_deleted")
}

// releaseZombie clears a held claim whose backing Run CR still EXISTS but whose
// sandbox pod is gone past the stall grace (ISI-5438). Identical custody release
// to releaseOrphan — the only difference is the audit/outbox reason, so the two
// kinds of dead holder are distinguishable in the §6.5 trail. The lane returns
// to todo (reclaimable); the wedged Run's reconcile_step is forced to failed so
// the projector terminalizes it and the 3.2 machine stops re-entering it.
func (s *HeartbeatSweeper) releaseZombie(ctx context.Context, hc heldClaim) error {
	return s.releaseDead(ctx, hc, "zombie_no_sandbox")
}

// releaseDead is the shared custody-release transaction for a dead holder
// (ISI-5184 orphan: Run CR gone; ISI-5438 zombie: Run CR present but pod gone).
// It nulls the custody triple, forces reconcile_step to failed, co-commits a
// §6.5 claim_released audit + outbox row carrying `reason`, and returns the lane
// to todo (guarded on in_progress so a human-moved lane is never touched). The
// release is idempotent: a re-sweep of an already-cleared row matches 0 rows and
// commits a no-op without re-auditing.
func (s *HeartbeatSweeper) releaseDead(ctx context.Context, hc heldClaim, reason string) error {
	if hc.holderRun == "" {
		return nil // no run provenance to release under
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	res, err := tx.ExecContext(ctx, `
		UPDATE coord.claim
		   SET holder_principal = NULL,
		       run_id           = NULL,
		       lease_expires_at = NULL,
		       reconcile_step   = 'failed'
		 WHERE work_item_id = $1::uuid
		   AND run_id        = $2::uuid
		   AND holder_principal IS NOT NULL`,
		hc.workItemID, hc.holderRun)
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Already cleared by a concurrent sweep / release — nothing to audit.
		return tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.audit_log
		       (work_item_id, run_id, event_type, principal, fence_token, to_state)
		VALUES ($1::uuid, $2::uuid, 'claim_released', $3, $4, $5)`,
		hc.workItemID, hc.holderRun, hc.principal, hc.fence, reason); err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.outbox
		       (entity, project_id, squad, event_type, work_item_id, run_id, payload)
		SELECT 'run', wi.project_id, wi.team_id::text, 'claim_released',
		       wi.id, $2::uuid,
		       jsonb_build_object('fence_token', $3::bigint, 'reason', $4::text)
		  FROM coord.work_item wi WHERE wi.id = $1::uuid`,
		hc.workItemID, hc.holderRun, hc.fence, reason); err != nil {
		return fmt.Errorf("outbox: %w", err)
	}

	// The dead holder never committed a terminal step, so from the machine's
	// perspective it failed: the lane returns to todo (reclaimable), guarded on
	// in_progress so a human-moved lane is never touched.
	if _, err := tx.ExecContext(ctx, `
		UPDATE coord.work_item
		   SET state = $2, updated_at = now()
		 WHERE id = $1::uuid AND state = 'in_progress'`,
		hc.workItemID, "todo"); err != nil {
		return fmt.Errorf("lane return: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// isPausedStep reports whether a step is one of the intentional 3.7 park states
// (rate-limit AND all credential/endpoint variants — via the canonical PhaseOf
// projection, so a new paused variant is covered automatically). A paused Run's
// wait is deliberate, so the ISI-5438 zombie gate must never reap it.
func isPausedStep(s reconcile.Step) bool {
	return reconcile.PhaseOf(s) == reconcile.PhasePaused
}

// clock is the sweep's time source (Now override, else time.Now).
func (s *HeartbeatSweeper) clock() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// stalled reports whether a held claim has gone without progress long enough to
// qualify for the ISI-5438 zombie check: custody taken more than the stall grace
// ago. A row with no acquired_at anchor is never stalled (fail closed).
func (s *HeartbeatSweeper) stalled(hc heldClaim) bool {
	if hc.acquiredAt.IsZero() {
		return false
	}
	grace := s.ZombieStallGrace
	if grace <= 0 {
		grace = DefaultZombieStallGrace
	}
	return s.clock().Sub(hc.acquiredAt) > grace
}

func (s *HeartbeatSweeper) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}
