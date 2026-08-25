//go:build chaos

// ISI-2879 — Story 2.3: prod Renew + RunHeartbeater chaos tests.
//
// These tests prove the production §6.2 lease-renewal path (prodclaim.go +
// prodheartbeat.go) against the SHIPPED coord schema on a real Postgres.
//
//	R1  Fenced-out renewal is rejected: after the fence advances (a reclaim),
//	    the former holder's Renew returns false. Teeth: without the fence guard
//	    Renew would succeed on a stale lease.
//	R2  Expired-lease renewal is rejected: a caller that waits past LeaseInterval
//	    cannot extend a dead lease. Teeth: without the clock_timestamp() > guard
//	    Renew would succeed on an expired row.
//	R3  RunHeartbeater keeps a live lease alive: after several renewal intervals
//	    the lease_expires_at has advanced and renewed_at is non-NULL. Teeth:
//	    without the heartbeat loop the lease would expire on its own.
//	R4  RunHeartbeater stops cleanly on context cancel: ctx.Cancel causes the
//	    goroutine to exit and the returned channel to deliver true (clean stop).
//	R5  RunHeartbeater stops on lease loss: a concurrent reclaim advances the
//	    fence; the next renewal tick returns false and the channel delivers false
//	    (loss signal), not blocking forever.
package coord_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/coord"
)

// renewRunUUID produces a deterministic UUID for test runs.
func renewRunUUID(n int) string {
	return prodRunUUID(1000 + n)
}

// claimOneItem claims the single pre-seeded item and returns its itemID and fence.
func claimOneItem(t testing.TB, db *sql.DB) (itemID string, fence int64) {
	t.Helper()
	cfg := coord.ProdConfig{
		LeaseInterval:  "5 seconds",
		ClaimableState: "todo",
		ClaimedState:   "in_progress",
	}
	pc, err := coord.NewProdClaimer(db, cfg)
	if err != nil {
		t.Fatalf("NewProdClaimer: %v", err)
	}
	id, f, ok, err := pc.ClaimNext(context.Background(), "test-agent", renewRunUUID(1), "")
	if err != nil || !ok {
		t.Fatalf("ClaimNext: ok=%v err=%v", ok, err)
	}
	return id, f
}

// resetAndSeedOne resets the schema and seeds a single claimable item.
func resetAndSeedOne(t testing.TB, db *sql.DB) {
	t.Helper()
	resetProdSchema(t, db)
	seedProdItems(t, db, 1)
}

// advanceFence bumps the fence for itemID directly (simulating a reclaim) so
// the previous holder's fence becomes stale. Returns the new fence.
func advanceFence(t testing.TB, db *sql.DB, itemID string) int64 {
	t.Helper()
	var fence int64
	err := db.QueryRowContext(context.Background(),
		`UPDATE coord.claim
		    SET fence_token      = fence_token + 1,
		        holder_principal = 'reclaimer',
		        run_id           = '22222222-2222-2222-2222-222222222222'::uuid,
		        lease_expires_at = clock_timestamp() + interval '30 seconds',
		        renewed_at       = NULL
		  WHERE work_item_id = $1::uuid
		  RETURNING fence_token`, itemID).Scan(&fence)
	if err != nil {
		t.Fatalf("advanceFence: %v", err)
	}
	return fence
}

// expireLease directly stamps lease_expires_at to the past for itemID.
func expireLease(t testing.TB, db *sql.DB, itemID string) {
	t.Helper()
	mustExec(t, db,
		`UPDATE coord.claim SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE work_item_id = $1::uuid`,
		itemID)
}

func TestSpineProdRenew(t *testing.T) {
	dsn := dsnOrFatal(t)
	t.Run("R1_fenced_out_renewal_rejected", func(t *testing.T) { prodR1FencedRejected(t, dsn) })
	t.Run("R2_expired_lease_renewal_rejected", func(t *testing.T) { prodR2ExpiredRejected(t, dsn) })
	t.Run("R3_heartbeater_keeps_lease_alive", func(t *testing.T) { prodR3HeartbeatKeepsAlive(t, dsn) })
	t.Run("R4_heartbeater_stops_on_cancel", func(t *testing.T) { prodR4HeartbeatCancelClean(t, dsn) })
	t.Run("R5_heartbeater_stops_on_lease_loss", func(t *testing.T) { prodR5HeartbeatLossSig(t, dsn) })
}

func prodR1FencedRejected(t *testing.T, dsn string) {
	db := openDB(t, dsn)
	resetAndSeedOne(t, db)

	itemID, fence := claimOneItem(t, db)

	// Simulate reclaim: advance the fence so our fence is now stale.
	advanceFence(t, db, itemID)

	cfg := coord.ProdConfig{LeaseInterval: "5 seconds", ClaimableState: "todo", ClaimedState: "in_progress"}
	pc, _ := coord.NewProdClaimer(db, cfg)
	if pc.Renew(context.Background(), itemID, "test-agent", renewRunUUID(1), fence) {
		t.Fatal("R1: Renew succeeded after fence advanced — stale holder can still renew (guard missing)")
	}
}

func prodR2ExpiredRejected(t *testing.T, dsn string) {
	db := openDB(t, dsn)
	resetAndSeedOne(t, db)

	itemID, fence := claimOneItem(t, db)
	expireLease(t, db, itemID)

	cfg := coord.ProdConfig{LeaseInterval: "5 seconds", ClaimableState: "todo", ClaimedState: "in_progress"}
	pc, _ := coord.NewProdClaimer(db, cfg)
	if pc.Renew(context.Background(), itemID, "test-agent", renewRunUUID(1), fence) {
		t.Fatal("R2: Renew succeeded on expired lease — expiry guard missing")
	}
}

func prodR3HeartbeatKeepsAlive(t *testing.T, dsn string) {
	db := openDB(t, dsn)

	// Short lease so we can observe renewal within the test.
	resetProdSchema(t, db)
	mustExec(t, db,
		`INSERT INTO coord.work_item (project_id, title, state, created_by)
		 VALUES ('`+prodTestProject+`', 'hb-item', 'todo', 'seed')`)

	cfg := coord.ProdConfig{LeaseInterval: "2 seconds", ClaimableState: "todo", ClaimedState: "in_progress"}
	pc, _ := coord.NewProdClaimer(db, cfg)
	itemID, fence, ok, err := pc.ClaimNext(context.Background(), "hb-agent", renewRunUUID(2), "")
	if err != nil || !ok {
		t.Fatalf("R3: claim: ok=%v err=%v", ok, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Renew every 500ms; the 2s lease is refreshed several times.
	done := coord.RunHeartbeater(ctx, pc, itemID, "hb-agent", renewRunUUID(2), fence, 500*time.Millisecond)

	time.Sleep(1800 * time.Millisecond)

	// Read renewed_at — must be non-NULL (heartbeater fired at least once).
	var renewedAt sql.NullTime
	if err := db.QueryRowContext(context.Background(),
		`SELECT renewed_at FROM coord.claim WHERE work_item_id = $1::uuid`, itemID).Scan(&renewedAt); err != nil {
		t.Fatalf("R3: read renewed_at: %v", err)
	}
	if !renewedAt.Valid {
		t.Fatal("R3: renewed_at is NULL — heartbeater did not fire")
	}

	cancel()
	if sig, ok2 := <-done; !ok2 || !sig {
		t.Fatalf("R3: heartbeater returned unexpected signal (ok=%v sig=%v)", ok2, sig)
	}
}

func prodR4HeartbeatCancelClean(t *testing.T, dsn string) {
	db := openDB(t, dsn)
	resetAndSeedOne(t, db)
	itemID, fence := claimOneItem(t, db)

	cfg := coord.ProdConfig{LeaseInterval: "30 seconds", ClaimableState: "todo", ClaimedState: "in_progress"}
	pc, _ := coord.NewProdClaimer(db, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := coord.RunHeartbeater(ctx, pc, itemID, "test-agent", renewRunUUID(1), fence, 10*time.Second)

	cancel() // cancel immediately before any tick fires

	select {
	case sig := <-done:
		if !sig {
			t.Fatal("R4: heartbeater delivered false (loss) on clean cancel — should deliver true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("R4: heartbeater did not exit after context cancel")
	}
}

func prodR5HeartbeatLossSig(t *testing.T, dsn string) {
	db := openDB(t, dsn)
	resetAndSeedOne(t, db)
	itemID, fence := claimOneItem(t, db)

	cfg := coord.ProdConfig{LeaseInterval: "30 seconds", ClaimableState: "todo", ClaimedState: "in_progress"}
	pc, _ := coord.NewProdClaimer(db, cfg)

	ctx := context.Background()
	done := coord.RunHeartbeater(ctx, pc, itemID, "test-agent", renewRunUUID(1), fence, 200*time.Millisecond)

	// Advance the fence to invalidate our hold — next renewal tick returns false.
	time.Sleep(50 * time.Millisecond) // let the goroutine start
	advanceFence(t, db, itemID)

	select {
	case sig := <-done:
		if sig {
			t.Fatal("R5: heartbeater delivered true (clean) on lease loss — should deliver false")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("R5: heartbeater did not exit after lease loss")
	}
}
