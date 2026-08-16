//go:build chaos

// prod_reconcile_chaos_test.go — the real-Postgres integration gate for the
// reconcile machine's production Store binding (Story 3.1 / ISI-2655, the Story
// 2.7-style gate). Where machine_test.go falsifies the state-machine LOGIC over
// MemStore in the shipping language, this suite binds that same machine to
// coord.ProdReconcileStore over a LIVE Postgres carrying the checked-in schema
// (db/migrations 0001 + 0003) and proves the §6.4/§6.6 durability contract holds
// against the real UPDATE/INSERT statements, not a model.
//
// Run (same wiring as TestSpine — DATABASE_URL → a live CNPG/Postgres):
//   go test -race -tags=chaos -run 'TestProdReconcile' ./pkg/coord/...
//
// It reuses dsnOrFatal/openDB from spine_chaos_test.go (same coord_test package).
// A missing DATABASE_URL is a FATAL, never a skip — a required gate fails loud.
//
// Case ↔ invariant map:
//   R1 durable happy-path drive        machine end-to-end over Postgres   §6.4 AC1/AC2
//   R2 co-commit exactly one audit+outbox per advance                     §6.5/§6.6 AC6
//   R3 double-advance dedup (step-CAS) same-expected re-drive is a no-op   §6.4 AC3
//   R4 fence-guard rejects the reclaim zombie                             §6.3/§6.4 AC4
//   R5 monotonic fence-first reclaim + reclaim_fenced_at marker           §6.3

package coord_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/reconcile"
)

// recordingEffects is a stub of the reconcile.Effects seam — the real side-effect
// impl (warm-pool bind, A2A dispatch, artifact upsert) is Story 3.2. This gate
// exercises the STORE, so it only needs the effects to be observable no-ops.
type recordingEffects struct {
	binds      int
	dispatches int
	collects   int
	terminals  []reconcile.Step
}

func (e *recordingEffects) BindSandbox(string, bool)     { e.binds++ }
func (e *recordingEffects) Dispatch(string, bool)        { e.dispatches++ }
func (e *recordingEffects) Collect(string, string, bool) { e.collects++ }
func (e *recordingEffects) Terminal(s reconcile.Step)    { e.terminals = append(e.terminals, s) }

// migrationFile locates a checked-in migration by name (repo-root relative from
// ./pkg/coord, overridable via COORD_MIGRATIONS_DIR), FATALing if absent — a gate
// never passes without its real precondition.
func migrationFile(t *testing.T, name string) string {
	t.Helper()
	dir := os.Getenv("COORD_MIGRATIONS_DIR")
	candidates := []string{}
	if dir != "" {
		candidates = append(candidates, filepath.Join(dir, name))
	}
	candidates = append(candidates,
		filepath.Join("..", "..", "db", "migrations", name),
		filepath.Join("db", "migrations", name),
	)
	for _, p := range candidates {
		if b, err := os.ReadFile(p); err == nil {
			return string(b)
		}
	}
	t.Fatalf("ISI-2655 gate: cannot locate %s (looked in %v)", name, candidates)
	return ""
}

// seedItem resets the coord schema (0001 + 0003), inserts one work_item (its
// trigger auto-provisions the claim row at step 'pending', fence 0) and returns a
// Store bound to it plus its uuid.
func seedItem(t *testing.T, ctx context.Context, dsn string) (*coord.ProdReconcileStore, string) {
	t.Helper()
	db := openDB(t, dsn)
	if _, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS coord CASCADE`); err != nil {
		t.Fatalf("reset coord schema: %v", err)
	}
	for _, name := range []string{"0001_coord_schema.sql", "0003_reconcile_step.sql"} {
		if _, err := db.ExecContext(ctx, migrationFile(t, name)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	var wi string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO coord.work_item (project_id, title, state, created_by)
		     VALUES (gen_random_uuid(), 'reconcile gate item', 'in_progress', 'principal:test')
		  RETURNING id`).Scan(&wi); err != nil {
		t.Fatalf("seed work_item: %v", err)
	}
	// Give the claim a real holder + fence so the Advance fence-equality guard has a
	// non-zero token to match (mirrors a claimed Run).
	run := "22222222-2222-2222-2222-222222222222"
	if _, err := db.ExecContext(ctx,
		`UPDATE coord.claim SET holder_principal='principal:test', run_id=$2::uuid, fence_token=7 WHERE work_item_id=$1::uuid`,
		wi, run); err != nil {
		t.Fatalf("seed claim holder: %v", err)
	}
	s, err := coord.NewProdReconcileStore(ctx, db, wi, run, "principal:test", "")
	if err != nil {
		t.Fatalf("NewProdReconcileStore: %v", err)
	}
	return s, wi
}

func TestProdReconcile(t *testing.T) {
	dsn := dsnOrFatal(t)
	ctx := context.Background()

	// R1 + R2: a full durable drive advances pending→succeeded, and each of the
	// five happy-path transitions co-commits EXACTLY ONE audit row and ONE outbox
	// row (§6.5/§6.6 AC6).
	t.Run("R1_R2_durable_drive_cocommit", func(t *testing.T) {
		s, _ := seedItem(t, ctx, dsn)
		fence := s.Fence()
		eff := &recordingEffects{}
		if err := reconcile.Reconcile(eff, s, reconcile.Options{Durable: true, Fence: fence}); err != nil {
			t.Fatalf("durable drive: %v", err)
		}
		if s.Err() != nil {
			t.Fatalf("store error during drive: %v", s.Err())
		}
		if got := s.Step(); got != reconcile.StepSucceeded {
			t.Fatalf("final step = %q, want succeeded", got)
		}
		// pending→claiming_sandbox→dispatching→running→collecting→succeeded = 5 advances.
		if a, o := s.AuditRows(), s.OutboxRows(); a != 5 || o != 5 {
			t.Fatalf("co-commit count: audit=%d outbox=%d, want 5/5", a, o)
		}
		if len(eff.terminals) != 1 || eff.terminals[0] != reconcile.StepSucceeded {
			t.Fatalf("terminal effect = %v, want [succeeded]", eff.terminals)
		}
	})

	// R3: double-advance dedup — advancing off the SAME expected step twice commits
	// exactly once. The second attempt's step-CAS (reconcile_step=:expected) matches
	// 0 rows, so it neither re-advances nor writes a second audit/outbox row (AC3).
	t.Run("R3_double_advance_dedup", func(t *testing.T) {
		s, _ := seedItem(t, ctx, dsn)
		fence := s.Fence()
		if !s.Advance(reconcile.StepPending, reconcile.StepClaimingSandbox, &fence) {
			t.Fatalf("first advance should commit")
		}
		if s.Advance(reconcile.StepPending, reconcile.StepClaimingSandbox, &fence) {
			t.Fatalf("second advance off the stale expected step must NOT commit")
		}
		if s.Err() != nil {
			t.Fatalf("store error: %v", s.Err())
		}
		if got := s.Step(); got != reconcile.StepClaimingSandbox {
			t.Fatalf("step = %q after dedup, want claiming_sandbox", got)
		}
		if a, o := s.AuditRows(), s.OutboxRows(); a != 1 || o != 1 {
			t.Fatalf("dedup co-commit: audit=%d outbox=%d, want 1/1", a, o)
		}
	})

	// R4: the fence-equality guard rejects the reclaim zombie. After a fence-first
	// reclaim bumps the token, an Advance carrying the OLD fence commits nothing —
	// even though its expected step still matches (AC4: fencing covers the reclaim
	// zombie the step-CAS alone would admit).
	t.Run("R4_fence_guard_rejects_zombie", func(t *testing.T) {
		s, _ := seedItem(t, ctx, dsn)
		stale := s.Fence()
		if !s.Reclaim(stale + 1) {
			t.Fatalf("reclaim should raise the fence")
		}
		if s.Advance(reconcile.StepPending, reconcile.StepClaimingSandbox, &stale) {
			t.Fatalf("advance with the fenced-out (stale) fence must NOT commit")
		}
		if got := s.Step(); got != reconcile.StepPending {
			t.Fatalf("zombie advance must leave step at pending, got %q", got)
		}
		if a, o := s.AuditRows(), s.OutboxRows(); a != 0 || o != 0 {
			t.Fatalf("zombie advance must write nothing: audit=%d outbox=%d", a, o)
		}
		// The current-fence holder still advances.
		cur := s.Fence()
		if !s.Advance(reconcile.StepPending, reconcile.StepClaimingSandbox, &cur) {
			t.Fatalf("advance with the current fence should commit")
		}
	})

	// R5: fence-first reclaim is monotonic and stamps reclaim_fenced_at (§6.3).
	t.Run("R5_monotonic_reclaim_marker", func(t *testing.T) {
		s, wi := seedItem(t, ctx, dsn)
		base := s.Fence()
		if !s.Reclaim(base + 5) {
			t.Fatalf("reclaim to a higher fence should succeed")
		}
		if s.Reclaim(base + 5) {
			t.Fatalf("reclaim that does not RAISE the fence must be a no-op")
		}
		if got := s.Fence(); got != base+5 {
			t.Fatalf("fence = %d after reclaim, want %d", got, base+5)
		}
		var stamped bool
		if err := openDB(t, dsn).QueryRowContext(ctx,
			`SELECT reclaim_fenced_at IS NOT NULL FROM coord.claim WHERE work_item_id=$1::uuid`,
			wi).Scan(&stamped); err != nil {
			t.Fatalf("read reclaim_fenced_at: %v", err)
		}
		if !stamped {
			t.Fatalf("reclaim_fenced_at must be stamped by a fence-first reclaim (§6.3)")
		}
	})
}
