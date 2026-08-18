//go:build chaos

// reclaim_fence_ordering_test.go — the resource-layer-fence gate for §6.3 reclaim
// (ISI-2399 items 3 & 4, closing the C5 ordering limitation the spine chaos suite
// documents). spineC5FenceBeforeRelease proves the COMMITTED post-condition of
// fence-before-release but explicitly cannot prove statement ORDER, because the
// stamp+release run in one transaction and a survivor only ever observes the final
// committed state. The ResourceFencer seam makes the ordering externally
// observable: the fencer's confirmed success is the resource-layer kill/cordon,
// and it is invoked STRICTLY BETWEEN the reclaim_fenced_at stamp and the release.
//
// This gives items 3 & 4 real teeth without a live cluster:
//   - item 3 (real resource-layer fencing, fail-closed): a fencer that cannot
//     confirm the kill aborts the whole reclaim — the claim keeps its original
//     holder, the fence is NOT bumped, and reclaim_fenced_at is NOT durably
//     stamped. Release can never happen while the zombie may be live.
//   - item 4 (release cannot become visible before fencing succeeds): the fencer
//     records, at call time, whether the release is already visible in a SEPARATE
//     connection. It never is — the release sits in the still-open reclaim
//     transaction and only commits AFTER Fence returns nil.
//
// The concrete K8s pod-kill/cordon fencer (controller-runtime client) is supplied
// by the operator wiring that owns the live resource layer; this proves the
// coordinator honors the fail-closed ordering contract that fencer plugs into.
package coord_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/K8squad/K8squad/pkg/coord"
)

// recordingFencer is a test ResourceFencer. It captures the holder it was asked
// to fence, and — reading through a SEPARATE *sql.DB connection (never the
// reclaim's own transaction) — whether the release is already visible at the
// instant it is called. failWith, when non-nil, makes the confirm fail so we can
// assert the coordinator fails closed.
type recordingFencer struct {
	db       *sql.DB
	failWith error

	called          int
	gotItem         int
	gotPrincipal    string
	gotRun          string
	releaseVisible  bool // was the fence bumped / holder swapped when Fence ran?
	fenceAtFenceRun int64
}

func (f *recordingFencer) Fence(ctx context.Context, item int, principal, run string) error {
	f.called++
	f.gotItem = item
	f.gotPrincipal = principal
	f.gotRun = run

	// Read committed state on a DIFFERENT connection: the reclaim's release is in a
	// still-open transaction, so a committed-read observer must see the OLD holder
	// and OLD fence. If the release were ordered before fencing, we would see the
	// new holder / bumped fence here.
	var holder sql.NullString
	var fence int64
	row := f.db.QueryRowContext(ctx,
		`SELECT holder_principal, fence_token FROM claim WHERE work_item_id=$1`, item)
	if err := row.Scan(&holder, &fence); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	f.fenceAtFenceRun = fence
	// "Release visible" = a holder other than the dying one, i.e. the swap landed.
	f.releaseVisible = holder.Valid && holder.String != principal

	return f.failWith
}

// TestReclaimResourceFenceOrdering proves ReclaimFenced gates its release on a
// CONFIRMED resource-layer fence (ISI-2399 items 3 & 4).
func TestReclaimResourceFenceOrdering(t *testing.T) {
	dsn := dsnOrFatal(t)
	ctx := context.Background()
	db := openDB(t, dsn)

	// A2: fail-closed. A fencer that cannot confirm the kill aborts the reclaim.
	t.Run("A2_fence_unconfirmed_fails_closed", func(t *testing.T) {
		freshSchema(t, db, 1)
		fencer := &recordingFencer{db: db, failWith: errors.New("pod-kill unconfirmed")}
		sut := coord.NewForTest(db).WithFencer(fencer)

		f1, ok := sut.Acquire(ctx, 0, "H1", "run-H1")
		if !ok {
			t.Fatal("setup: H1 acquire failed")
		}
		waitLeaseExpiry(t)

		if _, err := sut.ReclaimFenced(ctx, 0, "H2", "run-H2"); err == nil {
			t.Fatal("ReclaimFenced returned nil when the resource-layer fence was " +
				"unconfirmed — it released the claim while the zombie may be live")
		}
		if fencer.called != 1 {
			t.Fatalf("fencer called %d times, want 1", fencer.called)
		}
		if fencer.gotPrincipal != "H1" || fencer.gotRun != "run-H1" {
			t.Fatalf("fencer got holder %q/%q, want H1/run-H1 (the DYING holder)",
				fencer.gotPrincipal, fencer.gotRun)
		}

		// Fail-closed post-conditions: NO release. Original holder + fence intact,
		// reclaim_fenced_at NOT durably stamped (the whole tx rolled back).
		var holder sql.NullString
		var fence int64
		var fencedAt sql.NullTime
		mustQueryRow(t, db,
			`SELECT holder_principal, fence_token, reclaim_fenced_at FROM claim WHERE work_item_id=0`).
			Scan(&holder, &fence, &fencedAt)
		if holder.String != "H1" {
			t.Fatalf("holder=%q after failed fence, want H1 (claim must NOT be released)", holder.String)
		}
		if fence != f1 {
			t.Fatalf("fence_token=%d after failed fence, want %d (unbumped)", fence, f1)
		}
		if fencedAt.Valid {
			t.Fatal("reclaim_fenced_at was durably stamped despite an unconfirmed " +
				"fence — the abort must roll back the marker too (fail-closed)")
		}

		// H2 never became a holder; the still-live-fence H1 slot is what remains.
		if sut.Complete(ctx, 0, "H2", f1+1) {
			t.Fatal("H2 completed after a failed reclaim — it was never installed")
		}
	})

	// A1: happy path — the fence is CONFIRMED before the release, and no observer
	// can see the release ordered ahead of the fence.
	t.Run("A1_fence_confirmed_before_release", func(t *testing.T) {
		freshSchema(t, db, 1)
		fencer := &recordingFencer{db: db} // failWith nil → confirms
		sut := coord.NewForTest(db).WithFencer(fencer)

		f1, ok := sut.Acquire(ctx, 0, "H1", "run-H1")
		if !ok {
			t.Fatal("setup: H1 acquire failed")
		}
		waitLeaseExpiry(t)

		f2, err := sut.ReclaimFenced(ctx, 0, "H2", "run-H2")
		if err != nil {
			t.Fatalf("ReclaimFenced: %v", err)
		}
		if f2 != f1+1 {
			t.Fatalf("fence: got %d want %d", f2, f1+1)
		}
		if fencer.called != 1 {
			t.Fatalf("fencer called %d times, want 1", fencer.called)
		}
		// ORDER (item 4): at the instant Fence ran, the release was NOT visible on a
		// separate connection — H1 was still the holder and the fence was un-bumped.
		if fencer.releaseVisible {
			t.Fatal("release was visible to a separate connection at fence time — the " +
				"release was ordered BEFORE the resource-layer fence (§6.3 violated)")
		}
		if fencer.fenceAtFenceRun != f1 {
			t.Fatalf("fence_token seen at fence time=%d, want %d (pre-release value)",
				fencer.fenceAtFenceRun, f1)
		}

		// After commit, the release is durable and the surviving zombie H1 is fenced.
		if sut.Complete(ctx, 0, "H1", f1) {
			t.Fatal("surviving zombie H1 wrote after a confirmed reclaim")
		}
		if !sut.Complete(ctx, 0, "H2", f2) {
			t.Fatal("current holder H2's live-fence write was rejected")
		}
	})

	// A3: default (no fencer wired) is unchanged — the SQL-order-only reclaim the
	// chaos gate proves still holds, so nothing regresses for the harness path.
	t.Run("A3_no_fencer_unchanged", func(t *testing.T) {
		freshSchema(t, db, 1)
		sut := coord.NewForTest(db) // no WithFencer

		f1, _ := sut.Acquire(ctx, 0, "H1", "run-H1")
		waitLeaseExpiry(t)
		f2, err := sut.ReclaimFenced(ctx, 0, "H2", "run-H2")
		if err != nil {
			t.Fatalf("ReclaimFenced (no fencer): %v", err)
		}
		if f2 != f1+1 {
			t.Fatalf("fence: got %d want %d", f2, f1+1)
		}
		var fencedAt sql.NullTime
		mustQueryRow(t, db, `SELECT reclaim_fenced_at FROM claim WHERE work_item_id=0`).Scan(&fencedAt)
		if !fencedAt.Valid {
			t.Fatal("reclaim_fenced_at not stamped on the no-fencer path")
		}
	})
}
