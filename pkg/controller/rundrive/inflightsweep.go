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

// inflightsweep.go — the periodic in-flight re-enqueue sweep (ISI-7531 /
// ISI-7536, ISI-7498 die-off-recovery family). A Run mid-flight in an
// in-flight step (dispatching/running/collecting) is normally carried by the
// drive loop's in-memory 2s self-poll chain. An operator roll DROPS that
// workqueue: the post-roll informer LIST enqueues each Run exactly once, and
// if that single pass does not terminalize in one go (the sandbox died while
// the operator was down, so no pod DELETE is ever replayed; or the pass
// re-parks on a follow-settlement gate), the Run is left with no prompt
// re-trigger — only the ~10h manager SyncPeriod re-list. reapGoneSandbox is
// correct but starved of invocations.
//
// This sweep is the P1 fix: every tick, list work items whose durable
// reconcile_step is in-flight and kick their owning Runs back into the drive
// loop through the resume channel — independent of pod-watch coverage, so it
// specifically covers "sandbox reaped while the operator was down during the
// roll". It is the same bounded background-sweep shape the CancelSweeper
// sanctions: LATENCY SUGAR for the kick, not the correctness path — every
// step is level-triggered off the durable reconcile_step, so a missed tick
// costs delay (at most one sweep interval), never correctness. Kicking a
// healthy in-flight Run is a cheap no-op reconcile.
package rundrive

import (
	"context"
	"time"
)

// InFlightSweepInterval is the in-flight sweep's tick (ISI-7536). 75s bounds
// wedge-to-recovery latency after an operator roll to ~a minute while the
// query itself is a cheap scan on the small claim table — three orders of
// magnitude under the ~10h SyncPeriod backstop it replaces, and deliberately
// slower than the 2s self-poll so steady-state load stays with the in-memory
// chain (the sweep only matters when that chain was dropped).
const InFlightSweepInterval = 75 * time.Second

// InFlightSweeper is a manager Runnable: every tick, list work items whose
// durable reconcile_step is in-flight (dispatching/running/collecting) and
// hand them to the driver's kick, which re-enqueues every owning Run through
// the resume channel source. Registered via mgr.Add, so it runs leader-elected
// — only the active operator sweeps.
type InFlightSweeper struct {
	Claims Claims
	OnDue  func(ctx context.Context, due []string)
	Tick   time.Duration
	Log    func(format string, args ...any)
}

// Start runs the sweep until ctx is done. Errors are logged and retried next
// tick — a transient DB stall must never kill the sweeper.
func (s *InFlightSweeper) Start(ctx context.Context) error {
	tick := s.Tick
	if tick <= 0 {
		tick = InFlightSweepInterval
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			due, err := s.Claims.InFlightDue(ctx)
			if err != nil {
				if s.Log != nil {
					s.Log("rundrive: in-flight sweep: %v", err)
				}
				continue
			}
			if len(due) > 0 && s.OnDue != nil {
				s.OnDue(ctx, due)
			}
		}
	}
}
