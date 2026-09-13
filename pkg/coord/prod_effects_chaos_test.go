//go:build chaos

// prod_effects_chaos_test.go — the real-Postgres integration gate for the reconcile
// machine's production side-effect binding (coord.ProdEffects, Story 3.1 / ISI-2655,
// child ISI-2802). Where machine_test.go falsifies the effect SEMANTICS over
// reconcile.World (the in-memory model) and prod_reconcile_chaos_test.go gates the
// Store, this suite binds reconcile.Effects to coord.ProdEffects over a LIVE Postgres
// carrying the checked-in schema (0001 + 0002 + 0003_coord_outbox + 0005 + 0007) and
// proves the §6.4 at-most-once contract holds against the real INSERT … ON CONFLICT
// statements — including the machine driving the REAL Store + REAL Effects together
// (E5), the "wire + prove" integration of the ISI-2655 slices.
//
// Run (same wiring as TestProdReconcile — DATABASE_URL → a live CNPG/Postgres):
//   go test -race -tags=chaos -run 'TestProdEffects' ./pkg/coord/...
//
// It reuses dsnOrFatal/openDB (spine_chaos_test.go), migrationFile + seedItem
// (prod_reconcile_chaos_test.go) — same coord_test package. A missing DATABASE_URL is
// a FATAL, never a skip — a required gate fails loud.
//
// Case ↔ invariant map:
//   E1 Collect upsert idempotency        re-collect republishes, one artifact row     §6.1/§6.4
//   E2 Dispatch dedup                    re-drive reattaches, one a2a_dispatch + submit §6.4/§10.1
//   E3 BindSandbox keyed idempotency     re-drive reattaches, one sandbox_bind + bind   §6.2/§9/§6.4
//   E4 Terminal record                   one §6.5 audit row for the terminal step       §6.5
//   E5 machine over REAL Store+Effects    full drive → succeeded, each effect fires once §6.4 AC1/AC2

package coord_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/reconcile"
)

// countingBinder / countingDispatcher are the physical-port stubs: they record how
// many times the physical warm-pool bind / A2A submit actually fired, so a re-drive
// that must REATTACH (not re-invoke) is asserted structurally, not just via row counts.
type countingBinder struct {
	calls int
	runs  []string
}

func (b *countingBinder) Bind(_ context.Context, runID string) (string, error) {
	b.calls++
	b.runs = append(b.runs, runID)
	return "sbx-" + runID, nil
}

type countingDispatcher struct {
	calls int
	tasks []string
}

func (d *countingDispatcher) Submit(_ context.Context, a2aTaskID, _ string) error {
	d.calls++
	d.tasks = append(d.tasks, a2aTaskID)
	return nil
}

// flakyDispatcher fails the first failFirst submits, then succeeds — modelling a
// transient builder/shim error (ISI-3617). It records every attempt so the test can
// assert the failed submit left NO marker and the recovery re-submitted.
type flakyDispatcher struct {
	failFirst int
	calls     int
	tasks     []string
}

func (d *flakyDispatcher) Submit(_ context.Context, a2aTaskID, _ string) error {
	d.calls++
	d.tasks = append(d.tasks, a2aTaskID)
	if d.calls <= d.failFirst {
		return fmt.Errorf("flaky submit: transient failure %d", d.calls)
	}
	return nil
}

// seedEffects seeds a fresh coord schema + one claimed work_item (reusing seedItem's
// reset/migrate/claim path, extended with 0007) and returns a ProdEffects bound to it
// plus the physical-port stubs and the work_item/run uuids for row assertions.
func seedEffects(t *testing.T, ctx context.Context, dsn string) (*coord.ProdEffects, *countingBinder, *countingDispatcher, string, string) {
	t.Helper()
	// seedItem applies 0001/0002/0003/0005; 0007 adds coord.sandbox_bind on top.
	store, wi := seedItem(t, ctx, dsn)
	db := openDB(t, dsn)
	if _, err := db.ExecContext(ctx, migrationFile(t, "0007_reconcile_effects.sql")); err != nil {
		t.Fatalf("apply 0007_reconcile_effects.sql: %v", err)
	}
	// The Run uuid seedItem stamped on the claim row (holder + fence).
	var run string
	if err := db.QueryRowContext(ctx,
		`SELECT run_id::text FROM coord.claim WHERE work_item_id=$1::uuid`, wi).Scan(&run); err != nil {
		t.Fatalf("read seeded run_id: %v", err)
	}
	binder := &countingBinder{}
	dispatcher := &countingDispatcher{}
	eff, err := coord.NewProdEffects(ctx, db, wi, run, "principal:test", "", binder, dispatcher)
	if err != nil {
		t.Fatalf("NewProdEffects: %v", err)
	}
	_ = store // the Store is re-derived per case where E5 needs it
	return eff, binder, dispatcher, wi, run
}

func countEffectRows(t *testing.T, ctx context.Context, dsn, q, wi string) int {
	t.Helper()
	var n int
	if err := openDB(t, dsn).QueryRowContext(ctx, q, wi).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", q, err)
	}
	return n
}

// boardLane reads the current §8.6 board lane (coord.work_item.state) for wi —
// the projection a Kanban card and the BFF /api/work-items read (ISI-4226).
func boardLane(t *testing.T, ctx context.Context, dsn, wi string) string {
	t.Helper()
	var lane string
	if err := openDB(t, dsn).QueryRowContext(ctx,
		`SELECT state FROM coord.work_item WHERE id=$1::uuid`, wi).Scan(&lane); err != nil {
		t.Fatalf("read board lane: %v", err)
	}
	return lane
}

func TestProdEffects(t *testing.T) {
	dsn := dsnOrFatal(t)
	ctx := context.Background()

	// E1: content-addressed Collect is idempotent — a re-driven collecting step
	// (upsert=true) republishes the SAME UNIQUE(work_item,run,kind) row, never a
	// duplicate, and audits the first publish exactly once (§6.1/§6.4).
	t.Run("E1_collect_upsert_idempotent", func(t *testing.T) {
		eff, _, _, wi, _ := seedEffects(t, ctx, dsn)
		for i := 0; i < 3; i++ {
			eff.Collect(reconcile.RunID+"/patch", "diff-bytes", true)
		}
		if err := eff.Err(); err != nil {
			t.Fatalf("collect error: %v", err)
		}
		if got := countEffectRows(t, ctx, dsn,
			`SELECT count(*) FROM coord.artifact WHERE work_item_id=$1::uuid AND kind='patch'`, wi); got != 1 {
			t.Fatalf("artifact rows = %d after 3 upserts, want 1 (content-addressed republish)", got)
		}
		if got := countEffectRows(t, ctx, dsn,
			`SELECT count(*) FROM coord.audit_log WHERE work_item_id=$1::uuid AND event_type='artifact_registered'`, wi); got != 1 {
			t.Fatalf("artifact audit rows = %d, want 1 (first publish only)", got)
		}
	})

	// E2: Dispatch dedups on the deterministic a2a_task_id — a re-drive reattaches to
	// the in-flight task: exactly one coord.a2a_dispatch row AND exactly one physical
	// shim submit, never a second agent execution (§6.4/§10.1).
	t.Run("E2_dispatch_dedup", func(t *testing.T) {
		eff, _, dispatcher, wi, run := seedEffects(t, ctx, dsn)
		for i := 0; i < 3; i++ {
			eff.Dispatch(reconcile.RunID, true)
		}
		if err := eff.Err(); err != nil {
			t.Fatalf("dispatch error: %v", err)
		}
		if got := countEffectRows(t, ctx, dsn,
			`SELECT count(*) FROM coord.a2a_dispatch WHERE work_item_id=$1::uuid`, wi); got != 1 {
			t.Fatalf("a2a_dispatch rows = %d after 3 dispatches, want 1 (dedup)", got)
		}
		if dispatcher.calls != 1 {
			t.Fatalf("physical submit fired %d times, want 1 (re-drive must reattach)", dispatcher.calls)
		}
		// The dedup key is the bound Run's uuid, not the fixture "run-1".
		if dispatcher.tasks[0] != run {
			t.Fatalf("a2a_task_id = %q, want the bound run uuid %q", dispatcher.tasks[0], run)
		}
	})

	// E2b: submit-then-mark (ISI-3617). A submit that FAILS must leave NO
	// coord.a2a_dispatch marker — otherwise a re-drive reattaches on the phantom
	// marker and skips the dispatch forever. First pass: the shim errors, eff.Err()
	// is set, zero markers. Recovery pass (fresh eff, shim now healthy): re-submits
	// and writes exactly one marker + one audit row. Proves the marker records a
	// SUCCESSFUL dispatch, never a merely-attempted one.
	t.Run("E2b_failed_submit_leaves_no_marker_then_resubmits", func(t *testing.T) {
		store, wi := seedItem(t, ctx, dsn)
		db := openDB(t, dsn)
		if _, err := db.ExecContext(ctx, migrationFile(t, "0007_reconcile_effects.sql")); err != nil {
			t.Fatalf("apply 0007: %v", err)
		}
		var run string
		if err := db.QueryRowContext(ctx,
			`SELECT run_id::text FROM coord.claim WHERE work_item_id=$1::uuid`, wi).Scan(&run); err != nil {
			t.Fatalf("read run_id: %v", err)
		}
		_ = store

		// First pass: the shim rejects the submit.
		flaky := &flakyDispatcher{failFirst: 1}
		eff, err := coord.NewProdEffects(ctx, db, wi, run, "principal:test", "", nil, flaky)
		if err != nil {
			t.Fatalf("NewProdEffects: %v", err)
		}
		eff.Dispatch(reconcile.RunID, true)
		if eff.Err() == nil {
			t.Fatal("ISI-3617: a failed submit must surface a sticky error, got nil")
		}
		if flaky.calls != 1 {
			t.Fatalf("submit fired %d times on the failing pass, want 1", flaky.calls)
		}
		if got := countEffectRows(t, ctx, dsn,
			`SELECT count(*) FROM coord.a2a_dispatch WHERE work_item_id=$1::uuid`, wi); got != 0 {
			t.Fatalf("ISI-3617: a2a_dispatch rows = %d after a FAILED submit, want 0 "+
				"(marker must not commit before the shim accepts the task)", got)
		}

		// Recovery: fresh eff (production rebuilds per drive), shim now healthy.
		eff2, err := coord.NewProdEffects(ctx, db, wi, run, "principal:test", "", nil, flaky)
		if err != nil {
			t.Fatalf("NewProdEffects (recovery): %v", err)
		}
		eff2.Dispatch(reconcile.RunID, true)
		if err := eff2.Err(); err != nil {
			t.Fatalf("recovery dispatch error: %v", err)
		}
		if flaky.calls != 2 {
			t.Fatalf("submit fired %d times total, want 2 (the re-drive must re-submit)", flaky.calls)
		}
		if got := countEffectRows(t, ctx, dsn,
			`SELECT count(*) FROM coord.a2a_dispatch WHERE work_item_id=$1::uuid`, wi); got != 1 {
			t.Fatalf("a2a_dispatch rows = %d after recovery, want 1", got)
		}
		if got := countEffectRows(t, ctx, dsn,
			`SELECT count(*) FROM coord.audit_log WHERE work_item_id=$1::uuid AND event_type='a2a_dispatched'`, wi); got != 1 {
			t.Fatalf("a2a_dispatched audit rows = %d, want 1 (first successful dispatch only)", got)
		}
	})

	// E3: BindSandbox keyed is idempotent — a re-driven claiming_sandbox step reattaches
	// on run_id: one coord.sandbox_bind row AND exactly one physical warm-pool bind,
	// never a second sandbox (§6.2/§9/§6.4).
	t.Run("E3_bind_keyed_idempotent", func(t *testing.T) {
		eff, binder, _, wi, run := seedEffects(t, ctx, dsn)
		for i := 0; i < 3; i++ {
			eff.BindSandbox(reconcile.RunID, true)
		}
		if err := eff.Err(); err != nil {
			t.Fatalf("bind error: %v", err)
		}
		if got := countEffectRows(t, ctx, dsn,
			`SELECT count(*) FROM coord.sandbox_bind WHERE work_item_id=$1::uuid`, wi); got != 1 {
			t.Fatalf("sandbox_bind rows = %d after 3 binds, want 1 (reattach)", got)
		}
		if binder.calls != 1 {
			t.Fatalf("physical bind fired %d times, want 1 (re-drive must reattach)", binder.calls)
		}
		// The stamped handle is the physical binder's, keyed on the bound run uuid.
		var ref string
		if err := openDB(t, dsn).QueryRowContext(ctx,
			`SELECT sandbox_ref FROM coord.sandbox_bind WHERE run_id=$1::uuid`, run).Scan(&ref); err != nil {
			t.Fatalf("read sandbox_ref: %v", err)
		}
		if ref != "sbx-"+run {
			t.Fatalf("sandbox_ref = %q, want %q (physical handle stamped on first provision)", ref, "sbx-"+run)
		}
	})

	// E4: Terminal records exactly one §6.5 audit row carrying the terminal step
	// AND advances the board card off the claimed lane to the terminal projection
	// (ISI-4226): a succeeded run lands the card in `done`, not stuck `in_progress`.
	t.Run("E4_terminal_records", func(t *testing.T) {
		eff, _, _, wi, _ := seedEffects(t, ctx, dsn)
		eff.Terminal(reconcile.StepSucceeded)
		if err := eff.Err(); err != nil {
			t.Fatalf("terminal error: %v", err)
		}
		if got := countEffectRows(t, ctx, dsn,
			`SELECT count(*) FROM coord.audit_log WHERE work_item_id=$1::uuid AND event_type='run_terminal' AND to_state='succeeded'`, wi); got != 1 {
			t.Fatalf("terminal audit rows = %d, want 1", got)
		}
		// ISI-4226: the card must leave in_progress on settle, without human help.
		if got := boardLane(t, ctx, dsn, wi); got != "done" {
			t.Fatalf("board lane after succeeded Terminal = %q, want done (card stuck in claimed lane)", got)
		}
	})

	// E4b: a terminally-FAILED run surfaces the card in `in_review` for human
	// triage (ISI-4226) — never falsely `done`, never left in `in_progress`.
	t.Run("E4b_terminal_failed_lane", func(t *testing.T) {
		eff, _, _, wi, _ := seedEffects(t, ctx, dsn)
		eff.Terminal(reconcile.StepFailed)
		if err := eff.Err(); err != nil {
			t.Fatalf("terminal error: %v", err)
		}
		if got := boardLane(t, ctx, dsn, wi); got != "in_review" {
			t.Fatalf("board lane after failed Terminal = %q, want in_review", got)
		}
	})

	// E4c: the board advance is guarded to the claimed lane and idempotent — a
	// re-drive of Terminal after the card already moved (done) leaves it there and
	// never resurrects it into a non-terminal lane.
	t.Run("E4c_terminal_advance_idempotent", func(t *testing.T) {
		eff, _, _, wi, _ := seedEffects(t, ctx, dsn)
		eff.Terminal(reconcile.StepSucceeded)
		eff.Terminal(reconcile.StepSucceeded)
		if err := eff.Err(); err != nil {
			t.Fatalf("terminal error: %v", err)
		}
		if got := boardLane(t, ctx, dsn, wi); got != "done" {
			t.Fatalf("board lane after re-driven Terminal = %q, want done", got)
		}
	})

	// E5: the WIRE + PROVE integration — the machine drives the REAL ProdReconcileStore
	// AND the REAL ProdEffects together over Postgres. A full durable drive reaches
	// succeeded, and each side effect fired exactly once physically (bind + dispatch +
	// collect) plus the terminal record — the machine's happy path against real coord
	// I/O, not the in-memory model (§6.4 AC1/AC2, ISI-2655 scope item 5).
	t.Run("E5_machine_over_real_store_and_effects", func(t *testing.T) {
		store, wi := seedItem(t, ctx, dsn)
		db := openDB(t, dsn)
		if _, err := db.ExecContext(ctx, migrationFile(t, "0007_reconcile_effects.sql")); err != nil {
			t.Fatalf("apply 0007: %v", err)
		}
		var run string
		if err := db.QueryRowContext(ctx,
			`SELECT run_id::text FROM coord.claim WHERE work_item_id=$1::uuid`, wi).Scan(&run); err != nil {
			t.Fatalf("read run_id: %v", err)
		}
		binder := &countingBinder{}
		dispatcher := &countingDispatcher{}
		eff, err := coord.NewProdEffects(ctx, db, wi, run, "principal:test", "", binder, dispatcher)
		if err != nil {
			t.Fatalf("NewProdEffects: %v", err)
		}

		fence := store.Fence()
		if err := reconcile.Reconcile(eff, store, reconcile.Options{Durable: true, Fence: fence}); err != nil {
			t.Fatalf("durable drive: %v", err)
		}
		if store.Err() != nil {
			t.Fatalf("store error: %v", store.Err())
		}
		if eff.Err() != nil {
			t.Fatalf("effects error: %v", eff.Err())
		}
		if got := store.Step(); got != reconcile.StepSucceeded {
			t.Fatalf("final step = %q, want succeeded", got)
		}
		// Each idempotent effect fired its physical mechanism exactly once over the drive.
		if binder.calls != 1 {
			t.Fatalf("physical bind calls = %d, want 1", binder.calls)
		}
		if dispatcher.calls != 1 {
			t.Fatalf("physical dispatch calls = %d, want 1", dispatcher.calls)
		}
		if got := countEffectRows(t, ctx, dsn,
			`SELECT count(*) FROM coord.sandbox_bind WHERE work_item_id=$1::uuid`, wi); got != 1 {
			t.Fatalf("sandbox_bind rows = %d, want 1", got)
		}
		if got := countEffectRows(t, ctx, dsn,
			`SELECT count(*) FROM coord.a2a_dispatch WHERE work_item_id=$1::uuid`, wi); got != 1 {
			t.Fatalf("a2a_dispatch rows = %d, want 1", got)
		}
		if got := countEffectRows(t, ctx, dsn,
			`SELECT count(*) FROM coord.artifact WHERE work_item_id=$1::uuid`, wi); got != 1 {
			t.Fatalf("artifact rows = %d, want 1", got)
		}
		if got := countEffectRows(t, ctx, dsn,
			`SELECT count(*) FROM coord.audit_log WHERE work_item_id=$1::uuid AND event_type='run_terminal'`, wi); got != 1 {
			t.Fatalf("terminal audit rows = %d, want 1", got)
		}
		// ISI-4226: the full happy-path drive lands the card in `done` — the
		// "progress visible" half of the M1.6 AC, end to end over real coord I/O.
		if got := boardLane(t, ctx, dsn, wi); got != "done" {
			t.Fatalf("board lane after full drive = %q, want done", got)
		}
	})
}
