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

// cancel.go — the 3.3 kill sweep (ISI-2884). A kill issued while the Run was
// HEALTHY has no pending requeue in the drive loop (death detection only
// requeues expired leases), so the driver needs an explicit wake. This is the
// same bounded background-sweep shape Story 2.4 sanctions for the reclaim
// sweeper: a short tick lists work items at cancelling and kicks their Runs
// back into the drive loop, which tears the sandbox down and finishes the
// terminal transition. The sweep is LATENCY SUGAR for the kick, not the
// correctness path — every step is level-triggered off the durable
// reconcile_step, so a missed kick costs delay, never correctness.
package rundrive

import (
	"context"
	"time"
)

// CancelSweepInterval is the kill sweep's tick. Two seconds bounds kill-to-
// Cancelled latency well inside the §5.3 "promptly" bar while the query
// itself is a cheap indexed-shape scan on the small claim table.
const CancelSweepInterval = 2 * time.Second

// CancelSweeper is a manager Runnable: every tick, list cancelling work items
// and hand them to the driver's kick.
type CancelSweeper struct {
	Claims Claims
	OnDue  func(ctx context.Context, due []string)
	Now    func() time.Time
	Tick   time.Duration
	Log    func(format string, args ...any)
}

// Start runs the sweep until ctx is done. Errors are logged and retried next
// tick — a transient DB stall must never kill the sweeper.
func (s *CancelSweeper) Start(ctx context.Context) error {
	tick := s.Tick
	if tick <= 0 {
		tick = CancelSweepInterval
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			due, err := s.Claims.CancelDue(ctx)
			if err != nil {
				if s.Log != nil {
					s.Log("rundrive: cancel sweep: %v", err)
				}
				continue
			}
			if len(due) > 0 && s.OnDue != nil {
				s.OnDue(ctx, due)
			}
		}
	}
}
