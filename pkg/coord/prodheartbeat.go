// prodheartbeat.go — §6.2 run_id-guarded periodic lease heartbeater (Story 2.3 / ISI-2879).
// RunHeartbeater drives ProdClaimer.Renew on a timer so a long-running holder
// keeps its lease alive without the caller managing the renewal loop itself.
package coord

import (
	"context"
	"time"
)

// RunHeartbeater periodically renews the lease for (itemID, principal, runID,
// fence) using claimer.Renew until one of three things happens:
//
//  1. ctx is cancelled (clean external stop — the run is done or shutting down).
//  2. Renew returns false (the lease has lapsed or the fence advanced — the run
//     no longer holds the item; it must stop work immediately). In this case
//     the returned channel receives false, alerting the caller to abort.
//  3. The interval fires but ctx is already done (same as case 1).
//
// The caller reads the returned channel exactly once. It receives true if the
// heartbeater exited because ctx was cancelled (normal shutdown), or false if
// it exited because the lease was lost (the caller must treat its work as
// forfeit). The channel is always closed after the send so select-based callers
// do not block if they read after the goroutine exits.
//
// interval should be comfortably less than cfg.LeaseInterval (e.g. one-third)
// so a transient DB hiccup doesn't expire the lease before the next renewal.
// The first renewal fires after one interval, not immediately — the just-acquired
// lease is already fresh.
func RunHeartbeater(ctx context.Context, claimer *ProdClaimer, itemID, principal, runID string, fence int64, interval time.Duration) <-chan bool {
	ch := make(chan bool, 1)
	go func() {
		defer close(ch)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				ch <- true // clean cancel
				return
			case <-ticker.C:
				if !claimer.Renew(ctx, itemID, principal, runID, fence) {
					ch <- false // lease lost
					return
				}
			}
		}
	}()
	return ch
}
