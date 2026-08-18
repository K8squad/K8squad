//go:build chaos

// reconcile_reader_chaos_test.go — the real-Postgres integration gate for the
// READ side of the §6.4 durable step (ISI-2655 slice-3, Story 2.7-style gate).
// Where prod_reconcile_chaos_test.go proves the WRITE path (ProdReconcileStore
// Advance/Reclaim) against a live coord schema, this suite proves that
// coord.ReconcileStepReader — the read-only projector the Run status controller
// uses — reports the committed reconcile_step from the same real coord.claim rows.
//
// Run (same wiring as the reconcile write gate — DATABASE_URL → a live Postgres):
//   go test -race -tags=chaos -run 'TestReconcileStepReader' ./pkg/coord/...
//
// It reuses dsnOrFatal/openDB/migrationFile/seedItem from the other coord_test
// chaos files (same package). A missing DATABASE_URL is a FATAL, never a skip.
//
// Case ↔ invariant map:
//   RR1 reads the committed step of a freshly seeded claim   (found=true, pending)
//   RR2 reflects an advance read-only                        (running, no write side-effects)
//   RR3 unknown work item → (found=false, nil err)           projected as Pending, never an error
//   RR4 empty id → not-found without hitting the ::uuid cast

package coord_test

import (
	"context"
	"testing"

	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/reconcile"
)

func TestReconcileStepReader(t *testing.T) {
	ctx := context.Background()
	dsn := dsnOrFatal(t)
	// seedItem resets the coord schema, inserts one work_item (its trigger
	// auto-provisions the claim at step 'pending', fence 0) and returns its uuid.
	_, workItemID := seedItem(t, ctx, dsn)
	db := openDB(t, dsn)
	reader := coord.NewReconcileStepReader(db)

	t.Run("RR1_reads_committed_step", func(t *testing.T) {
		step, found, err := reader.StepForWorkItem(ctx, workItemID)
		if err != nil {
			t.Fatalf("StepForWorkItem: %v", err)
		}
		if !found {
			t.Fatalf("found=false for a seeded work_item")
		}
		if step != reconcile.StepPending {
			t.Fatalf("step = %q, want pending", step)
		}
	})

	t.Run("RR2_reflects_advance_readonly", func(t *testing.T) {
		if _, err := db.ExecContext(ctx,
			`UPDATE coord.claim SET reconcile_step = 'running' WHERE work_item_id = $1::uuid`,
			workItemID); err != nil {
			t.Fatalf("advance step: %v", err)
		}
		step, found, err := reader.StepForWorkItem(ctx, workItemID)
		if err != nil || !found {
			t.Fatalf("StepForWorkItem after advance: found=%v err=%v", found, err)
		}
		if step != reconcile.StepRunning {
			t.Fatalf("step = %q, want running", step)
		}
	})

	t.Run("RR3_unknown_work_item_is_not_found", func(t *testing.T) {
		step, found, err := reader.StepForWorkItem(ctx, "99999999-9999-9999-9999-999999999999")
		if err != nil {
			t.Fatalf("unknown work_item must not error (projects as Pending): %v", err)
		}
		if found {
			t.Fatalf("unknown work_item should be found=false")
		}
		if step != "" {
			t.Fatalf("unknown step = %q, want empty", step)
		}
	})

	t.Run("RR4_empty_id_is_not_found", func(t *testing.T) {
		step, found, err := reader.StepForWorkItem(ctx, "")
		if err != nil || found || step != "" {
			t.Fatalf("empty id: step=%q found=%v err=%v, want empty/false/nil", step, found, err)
		}
	})
}
