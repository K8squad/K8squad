//go:build chaos

// chaos_test.go — the real-Postgres end-to-end gate for the Run DRIVE loop
// (Story 3.1/3.2/3.7, ISI-2883): the Driver, over the checked-in coord schema
// and a fake Kubernetes client, drives REAL durable state — the machine's
// advance co-commits, the retry lap's fence-first re-entry, and the 3.7
// park→wake→requeue cycle. This is the wiring Henrik's cluster test exercises,
// proven against real Postgres before it ships.
//
// Run (same wiring as TestSpine — DATABASE_URL → a live Postgres):
//
//	go test -race -tags=chaos -run 'TestSpineDrive' ./pkg/controller/rundrive/...
//
// A missing DATABASE_URL is a FATAL, never a skip.
package rundrive_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx" for the gate

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/contextasm"
	"github.com/K8squad/K8squad/pkg/controller/contextsource"
	"github.com/K8squad/K8squad/pkg/controller/rundrive"
	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/reconcile"
)

func dsnOrFatal(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Fatal("DATABASE_URL unset under -tags=chaos: the drive-loop gate " +
			"requires a live Postgres. Refusing to pass silently.")
	}
	return dsn
}

func migrationFile(t *testing.T, name string) string {
	t.Helper()
	dir := os.Getenv("COORD_MIGRATIONS_DIR")
	candidates := []string{}
	if dir != "" {
		candidates = append(candidates, filepath.Join(dir, name))
	}
	candidates = append(candidates,
		filepath.Join("..", "..", "..", "db", "migrations", name),
		filepath.Join("db", "migrations", name),
	)
	for _, p := range candidates {
		if b, err := os.ReadFile(p); err == nil {
			return string(b)
		}
	}
	t.Fatalf("drive gate: cannot locate migration %s", name)
	return ""
}

// isolatedGateDB provisions a per-fixture database off the gate DSN (parallel
// packages share the workflow's single Postgres — a shared-schema fixture
// races them; each fixture owns its database instead).
func isolatedGateDB(t *testing.T, dsn, tag string) *sql.DB {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	dbName := fmt.Sprintf("gate_%s_%d", tag, time.Now().UnixNano())

	admin, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	if _, err := admin.Exec(fmt.Sprintf(`CREATE DATABASE %q`, dbName)); err != nil {
		t.Fatalf("create %s: %v", dbName, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, dbName))
		_ = admin.Close()
	})

	u.Path = "/" + dbName
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open %s: %v", dbName, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// applyMigrations applies EVERY checked-in migration in filename order — the
// same discipline the apiserver migration runner uses (db/migrations README:
// "applied once, in order") — skipping the *_test.sql self-check companions.
// The gate schema therefore always matches prod instead of a hand-picked
// subset that drifts: the ISI-4298 fixture gap, where 98eb5b8's whole-row
// no-op guard verified green locally PRECISELY because the fixture applied
// only 0001..0009 and lacked 0012's GENERATED ALWAYS search_tsv — the column
// that defeats a whole-row NEW/OLD compare inside a BEFORE trigger live.
func applyMigrations(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	dir := os.Getenv("COORD_MIGRATIONS_DIR")
	if dir == "" {
		for _, c := range []string{
			filepath.Join("..", "..", "..", "db", "migrations"),
			filepath.Join("db", "migrations"),
		} {
			if st, err := os.Stat(c); err == nil && st.IsDir() {
				dir = c
				break
			}
		}
	}
	if dir == "" {
		t.Fatal("drive gate: cannot locate db/migrations (set COORD_MIGRATIONS_DIR)")
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("drive gate: read migrations dir %s: %v", dir, err)
	}
	for _, e := range ents { // ReadDir: sorted by filename — migration order
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") || strings.HasSuffix(name, "_test.sql") {
			continue
		}
		if _, err := db.ExecContext(ctx, migrationFile(t, name)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
}

// driveFixture provisions an ISOLATED database with the full checked-in
// migration set (0001→NNNN, see applyMigrations — prod schema parity),
// seeds one work item IN THE TODO LANE (the §13 claimable lane intake
// dispatches from — the M1.3 §6.2 acquire advances todo → in_progress, so a
// backlog-seeded fixture would absorb by design), and returns the DB handle
// + the item's uuid.
func driveFixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	ctx := context.Background()
	db := isolatedGateDB(t, dsnOrFatal(t), "drive")
	applyMigrations(t, db)

	var item string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO coord.work_item (project_id, title, created_by, state)
		VALUES (gen_random_uuid(), 'drive gate item', 'principal:chaos', 'todo')
		RETURNING id::text`).Scan(&item); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	return db, item
}

func driveScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := api.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return s
}

func newDriveRun(t *testing.T, cl client.Client, uid, name, workItem string) *api.Run {
	t.Helper()
	run := &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(uid)},
		Spec: api.RunSpec{
			TeamRef:     api.ObjectRef{Name: "t"},
			ProjectRef:  api.ObjectRef{Name: "p"},
			WorkItemRef: workItem,
		},
	}
	if err := cl.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	return run
}

func newTestDriver(cl client.Client, db *sql.DB, resumeCfg coord.ResumeConfig) (*rundrive.Driver, *coord.ProdResumeStore) {
	store, err := coord.NewProdResumeStore(db, resumeCfg, func() float64 { return 0 })
	if err != nil {
		panic(err)
	}
	driver := rundrive.NewDriver(cl,
		rundrive.NewProdClaims(db, ""),
		rundrive.NewProdPauses(store),
		rundrive.NewProdRunner(db, "", nil, nil))
	driver.Rand = func() float64 { return 0 }
	return driver, store
}

func stepOf(t *testing.T, db *sql.DB, item string) reconcile.Step {
	t.Helper()
	var step string
	if err := db.QueryRowContext(context.Background(),
		`SELECT reconcile_step FROM coord.claim WHERE work_item_id=$1::uuid`, item).
		Scan(&step); err != nil {
		t.Fatalf("read step: %v", err)
	}
	return reconcile.Step(step)
}

func countOf(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// TestSpineDrive is the workflow entrypoint (-run 'TestSpine' matches it).
func TestSpineDrive(t *testing.T) {
	ctx := context.Background()

	t.Run("D1 end-to-end drive: Run CR → durable machine → succeeded", func(t *testing.T) {
		db, item := driveFixture(t)
		cl := fake.NewClientBuilder().WithScheme(driveScheme(t)).
			WithIndex(&api.Run{}, ".spec.workItemRef", func(obj client.Object) []string {
				return []string{obj.(*api.Run).Spec.WorkItemRef}
			}).Build()
		newDriveRun(t, cl, "11111111-1111-1111-1111-111111111111", "run-1", item)

		driver, _ := newTestDriver(cl, db, coord.DefaultProdResumeConfig())
		res, err := driver.Reconcile(ctx, request("run-1"))
		if err != nil {
			t.Fatalf("drive: %v", err)
		}
		if res.RequeueAfter != 0 {
			t.Fatalf("healthy drive requeued (%v) — want terminal completion", res.RequeueAfter)
		}
		if got := stepOf(t, db, item); got != reconcile.StepSucceeded {
			t.Fatalf("durable step = %q, want succeeded", got)
		}
		// The co-commit spine: audit + outbox rows for the advances, and the
		// effects' durable markers (dispatch + artifact).
		if n := countOf(t, db, `SELECT count(*) FROM coord.audit_log WHERE work_item_id=$1::uuid AND event_type='reconcile_advanced'`, item); n == 0 {
			t.Fatal("no reconcile_advanced audit rows")
		}
		if n := countOf(t, db, `SELECT count(*) FROM coord.outbox WHERE work_item_id=$1::uuid AND event_type='reconcile_advanced'`, item); n == 0 {
			t.Fatal("no reconcile_advanced outbox rows")
		}
		if n := countOf(t, db, `SELECT count(*) FROM coord.a2a_dispatch WHERE work_item_id=$1::uuid`, item); n != 1 {
			t.Fatalf("a2a dispatch markers = %d, want 1", n)
		}
		if n := countOf(t, db, `SELECT count(*) FROM coord.artifact WHERE work_item_id=$1::uuid`, item); n != 1 {
			t.Fatalf("artifacts = %d, want 1", n)
		}
		// The §6.2 claim-back half (M1.3 / ISI-4183): the drive ACQUIRED the
		// checkout before driving — board lane todo → in_progress, one
		// claim_acquired audit row, one claimed outbox event — and the terminal
		// step RELEASED it: claim_released audit row, holder/lease cleared,
		// fence strictly raised past the acquire's.
		if n := countOf(t, db, `SELECT count(*) FROM coord.audit_log WHERE work_item_id=$1::uuid AND event_type='claim_acquired'`, item); n != 1 {
			t.Fatalf("claim_acquired audit rows = %d, want 1", n)
		}
		if n := countOf(t, db, `SELECT count(*) FROM coord.outbox WHERE work_item_id=$1::uuid AND entity='work_item' AND event_type='claimed'`, item); n != 1 {
			t.Fatalf("claimed outbox rows = %d, want 1", n)
		}
		if n := countOf(t, db, `SELECT count(*) FROM coord.audit_log WHERE work_item_id=$1::uuid AND event_type='claim_released'`, item); n != 1 {
			t.Fatalf("claim_released audit rows = %d, want 1", n)
		}
		var lane string
		var holder sql.NullString
		var lease sql.NullTime
		var fence int64
		if err := db.QueryRowContext(ctx, `
			SELECT wi.state, cl.holder_principal, cl.lease_expires_at, cl.fence_token
			  FROM coord.work_item wi JOIN coord.claim cl ON cl.work_item_id = wi.id
			 WHERE wi.id=$1::uuid`, item).Scan(&lane, &holder, &lease, &fence); err != nil {
			t.Fatalf("post-terminal custody read: %v", err)
		}
		if lane != "in_progress" {
			t.Fatalf("board lane = %q, want in_progress (todo advanced at acquire)", lane)
		}
		if holder.Valid || lease.Valid {
			t.Fatalf("terminal Run left custody: holder=%v lease=%v", holder, lease)
		}
		if fence < 2 {
			t.Fatalf("fence = %d, want ≥ 2 (acquire bump + terminal release bump)", fence)
		}
		// Re-drive is idempotent: terminal → absorbing, nothing duplicated.
		if _, err := driver.Reconcile(ctx, request("run-1")); err != nil {
			t.Fatalf("re-drive: %v", err)
		}
		if n := countOf(t, db, `SELECT count(*) FROM coord.a2a_dispatch WHERE work_item_id=$1::uuid`, item); n != 1 {
			t.Fatalf("re-drive duplicated markers: %d", n)
		}
	})

	t.Run("D2 death: expired lease mid-flight → fence-first retry lap", func(t *testing.T) {
		db, item := driveFixture(t)
		cl := fake.NewClientBuilder().WithScheme(driveScheme(t)).Build()
		run := newDriveRun(t, cl, "22222222-2222-2222-2222-222222222222", "run-1", item)
		max := int32(2)
		run.Spec.RetryPolicy = &api.RetryPolicy{MaxRetries: &max}
		if err := cl.Update(ctx, run); err != nil {
			t.Fatalf("policy: %v", err)
		}

		// Park the claim in-flight under a dead holder (lease expired).
		if _, err := db.ExecContext(ctx, `
			UPDATE coord.claim SET reconcile_step='running', holder_principal='agent-x',
			       lease_expires_at = clock_timestamp() - interval '5 minutes', fence_token = 3
			 WHERE work_item_id=$1::uuid`, item); err != nil {
			t.Fatalf("stage death: %v", err)
		}

		driver, _ := newTestDriver(cl, db, coord.DefaultProdResumeConfig())
		res, err := driver.Reconcile(ctx, request("run-1"))
		if err != nil {
			t.Fatalf("death drive: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Fatal("retry lap must requeue on backoff")
		}
		var step string
		var fence int64
		var holder sql.NullString
		if err := db.QueryRowContext(ctx, `
			SELECT reconcile_step, fence_token, holder_principal FROM coord.claim
			 WHERE work_item_id=$1::uuid`, item).Scan(&step, &fence, &holder); err != nil {
			t.Fatalf("post-death read: %v", err)
		}
		if step != "claiming_sandbox" {
			t.Fatalf("post-death step = %q, want claiming_sandbox (retry lap)", step)
		}
		if fence != 4 {
			t.Fatalf("fence = %d, want 4 (bumped off the dead holder)", fence)
		}
		if holder.Valid {
			t.Fatalf("checkout not released: holder=%q", holder.String)
		}
		if n := countOf(t, db, `SELECT count(*) FROM coord.audit_log WHERE work_item_id=$1::uuid AND event_type='retry_lap_entered'`, item); n != 1 {
			t.Fatalf("retry_lap_entered audit rows = %d, want 1", n)
		}

		// The requeued drive completes the lap: terminal succeeded. The lap
		// drive first RE-ACQUIRED the checkout RetryEnter released (§6.2 —
		// the drive loop is the claim-back half here too).
		if _, err := driver.Reconcile(ctx, request("run-1")); err != nil {
			t.Fatalf("lap drive: %v", err)
		}
		if got := stepOf(t, db, item); got != reconcile.StepSucceeded {
			t.Fatalf("lap step = %q, want succeeded", got)
		}
		if n := countOf(t, db, `SELECT count(*) FROM coord.audit_log WHERE work_item_id=$1::uuid AND event_type='claim_acquired'`, item); n != 1 {
			t.Fatalf("lap claim_acquired audit rows = %d, want 1", n)
		}
		// TWO custody lifetimes ⇒ two claim_released rows (ISI-4183 §6.5):
		// (1) RetryEnter's re-entry cleared the DEAD holder's held checkout
		//     (agent-x, fence 4) — enter() owes provenance for any held
		//     release, "a released checkout can never exist without its
		//     audit row" — and (2) the terminal step released the checkout
		//     this lap RE-ACQUIRED (the claim_acquired=1 above).
		if n := countOf(t, db, `SELECT count(*) FROM coord.audit_log WHERE work_item_id=$1::uuid AND event_type='claim_released'`, item); n != 2 {
			t.Fatalf("lap claim_released audit rows = %d, want 2 (dead-holder release + terminal release)", n)
		}
	})

	t.Run("D3 death outside budget → terminal failed", func(t *testing.T) {
		db, item := driveFixture(t)
		cl := fake.NewClientBuilder().WithScheme(driveScheme(t)).Build()
		newDriveRun(t, cl, "33333333-3333-3333-3333-333333333333", "run-1", item) // no RetryPolicy ⇒ budget 0

		if _, err := db.ExecContext(ctx, `
			UPDATE coord.claim SET reconcile_step='dispatching', holder_principal='agent-x',
			       lease_expires_at = clock_timestamp() - interval '5 minutes', fence_token = 1
			 WHERE work_item_id=$1::uuid`, item); err != nil {
			t.Fatalf("stage death: %v", err)
		}

		driver, _ := newTestDriver(cl, db, coord.DefaultProdResumeConfig())
		if _, err := driver.Reconcile(ctx, request("run-1")); err != nil {
			t.Fatalf("death drive: %v", err)
		}
		if got := stepOf(t, db, item); got != reconcile.StepFailed {
			t.Fatalf("step = %q, want failed (terminal)", got)
		}
		if n := countOf(t, db, `SELECT count(*) FROM coord.audit_log WHERE work_item_id=$1::uuid AND event_type='run_failed_entered'`, item); n != 1 {
			t.Fatalf("run_failed_entered audit rows = %d, want 1", n)
		}
	})

	t.Run("D4 3.7 park → single durable wake → requeue into dispatching", func(t *testing.T) {
		db, item := driveFixture(t)
		cl := fake.NewClientBuilder().WithScheme(driveScheme(t)).
			WithIndex(&api.Run{}, ".spec.workItemRef", func(obj client.Object) []string {
				return []string{obj.(*api.Run).Spec.WorkItemRef}
			}).Build()
		newDriveRun(t, cl, "44444444-4444-4444-4444-444444444444", "run-1", item)

		// Park the durable step the way the 5.10 signal consumer will.
		if _, err := db.ExecContext(ctx, `
			UPDATE coord.claim SET reconcile_step='paused(rate_limited)'
			 WHERE work_item_id=$1::uuid`, item); err != nil {
			t.Fatalf("stage pause: %v", err)
		}

		// Fast backoff policy so the wake fires inside the test.
		cfg := coord.DefaultProdResumeConfig()
		cfg.BackoffBase = 40 * time.Millisecond
		cfg.BackoffCap = 640 * time.Millisecond

		driver, store := newTestDriver(cl, db, cfg)
		notified := 0
		driver.Notify = func() { notified++ }

		if _, err := driver.Reconcile(ctx, request("run-1")); err != nil {
			t.Fatalf("park drive: %v", err)
		}
		if n := countOf(t, db, `SELECT count(*) FROM coord.run_pause WHERE work_item_id=$1::uuid AND resumed_at IS NULL`, item); n != 1 {
			t.Fatalf("pending episodes = %d, want 1", n)
		}
		if notified != 1 {
			t.Fatalf("notify calls = %d, want 1", notified)
		}

		// The wake: fire the due batch through the Driver's OnResumeDue (what
		// the ProdTimer calls), then the kicked drive completes the Run.
		time.Sleep(80 * time.Millisecond)
		due, err := store.ResumeDue(ctx)
		if err != nil || len(due) != 1 {
			t.Fatalf("resume due: %v %v", due, err)
		}
		driver.OnResumeDue(ctx, due)
		if got := stepOf(t, db, item); got != reconcile.StepDispatching {
			t.Fatalf("post-wake step = %q, want dispatching", got)
		}
		select {
		case <-driver.ResumeEvents():
		default:
			t.Fatal("resume kick not delivered")
		}

		// The kicked drive runs the machine to terminal.
		if _, err := driver.Reconcile(ctx, request("run-1")); err != nil {
			t.Fatalf("post-wake drive: %v", err)
		}
		if got := stepOf(t, db, item); got != reconcile.StepSucceeded {
			t.Fatalf("post-wake terminal = %q, want succeeded", got)
		}
	})

	// D5 (M1.3 / ISI-4183) §6.2 re-acquire on the retry lap over an ALREADY
	// advanced lane: RetryEnter released the checkout mid-flight with the board
	// lane at in_progress — the re-acquire must not roll back on the lane (the
	// pre-ISI-4183 AcquireSpecific mark guard would have), and the drive must
	// complete under the re-acquired fence.
	t.Run("D5 retry-lap re-acquire keeps an in_progress lane claimable", func(t *testing.T) {
		db, item := driveFixture(t)
		cl := fake.NewClientBuilder().WithScheme(driveScheme(t)).Build()
		max := int32(1)
		run := newDriveRun(t, cl, "55555555-5555-5555-5555-555555555555", "run-1", item)
		run.Spec.RetryPolicy = &api.RetryPolicy{MaxRetries: &max}
		if err := cl.Update(ctx, run); err != nil {
			t.Fatalf("policy: %v", err)
		}

		// Stage a mid-flight death with the lane ALREADY advanced (the first
		// attempt had acquired): in_progress + expired lease + holder.
		if _, err := db.ExecContext(ctx, `
			UPDATE coord.work_item SET state='in_progress' WHERE id=$1::uuid`, item); err != nil {
			t.Fatalf("stage lane: %v", err)
		}
		if _, err := db.ExecContext(ctx, `
			UPDATE coord.claim SET reconcile_step='running', holder_principal='agent-x',
			       run_id='99999999-9999-9999-9999-999999999999',
			       lease_expires_at = clock_timestamp() - interval '5 minutes', fence_token = 2
			 WHERE work_item_id=$1::uuid`, item); err != nil {
			t.Fatalf("stage death: %v", err)
		}

		driver, _ := newTestDriver(cl, db, coord.DefaultProdResumeConfig())
		// Pass 1: death detected → RetryEnter (checkout released, lap re-enter).
		if _, err := driver.Reconcile(ctx, request("run-1")); err != nil {
			t.Fatalf("death drive: %v", err)
		}
		// Pass 2: the requeued lap — re-acquire over the in_progress lane, then
		// the machine runs to terminal.
		if _, err := driver.Reconcile(ctx, request("run-1")); err != nil {
			t.Fatalf("lap drive: %v", err)
		}
		if got := stepOf(t, db, item); got != reconcile.StepSucceeded {
			t.Fatalf("lap step = %q, want succeeded", got)
		}
		if n := countOf(t, db, `SELECT count(*) FROM coord.audit_log WHERE work_item_id=$1::uuid AND event_type='claim_acquired'`, item); n != 1 {
			t.Fatalf("claim_acquired audit rows = %d, want 1", n)
		}
		var lane string
		if err := db.QueryRowContext(ctx,
			`SELECT state FROM coord.work_item WHERE id=$1::uuid`, item).Scan(&lane); err != nil {
			t.Fatalf("lane read: %v", err)
		}
		if lane != "in_progress" {
			t.Fatalf("lane = %q, want in_progress (kept across the retry lap)", lane)
		}
	})

	// D6 (ISI-4217 / ISI-4298) pinned-revision round trip across a RetryEnter,
	// over the FULL prod schema (applyMigrations now applies every migration,
	// including 0012's GENERATED ALWAYS search_tsv — the column that defeated
	// 98eb5b8's whole-row no-op guard live on k8squad-test). This replays the
	// live wedge end-to-end: lap 1 dispatched with the work-item revision
	// pinned (the §8.5 snapshot the run controller persists and the dispatcher
	// re-reads), the holder dies, RetryEnter re-enters the lap, and the lap-2
	// drive RE-ACQUIRES the already-in_progress lane — the §6.2 no-op VALUE
	// mark. The pinned revision must still resolve after it
	// (deterministic-resume contract), and a REAL edit must still break it
	// (the pin keeps its teeth — the guard suppresses nothing genuine).
	t.Run("D6 retry-lap re-acquire preserves the pinned work-item revision", func(t *testing.T) {
		db, item := driveFixture(t)
		cl := fake.NewClientBuilder().WithScheme(driveScheme(t)).Build()
		max := int32(1)
		run := newDriveRun(t, cl, "66666666-6666-6666-6666-666666666666", "run-1", item)
		run.Spec.RetryPolicy = &api.RetryPolicy{MaxRetries: &max}
		if err := cl.Update(ctx, run); err != nil {
			t.Fatalf("policy: %v", err)
		}

		// The §8.5 context trio assembleSystemContext resolves for the
		// dispatch build (Agent window, Project meta, Team scope key).
		agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{
			Name: "coder", Namespace: "default", UID: types.UID("77777777-7777-7777-7777-777777777777")}}
		project := &api.Project{ObjectMeta: metav1.ObjectMeta{
			Name: "p", Namespace: "default", UID: types.UID("88888888-8888-8888-8888-888888888888")}}
		team := &api.Team{ObjectMeta: metav1.ObjectMeta{
			Name: "t", Namespace: "default", UID: types.UID("99999999-9999-9999-9999-999999999999")}}
		for _, o := range []client.Object{agent, project, team} {
			if err := cl.Create(ctx, o); err != nil {
				t.Fatalf("seed %s: %v", o.GetName(), err)
			}
		}
		asm := contextsource.Deps{DB: db, Client: cl}.For("default")
		window := contextsource.WindowForModel(agent.Spec.Model)

		// Stage the lap-1 aftermath the way D5 does: the first attempt
		// ACQUIRED (lane already advanced) and dispatched, then the holder
		// died mid-flight with the lease expired.
		if _, err := db.ExecContext(ctx, `
			UPDATE coord.work_item SET state='in_progress' WHERE id=$1::uuid`, item); err != nil {
			t.Fatalf("stage lane: %v", err)
		}
		if _, err := db.ExecContext(ctx, `
			UPDATE coord.claim SET reconcile_step='running', holder_principal='agent-x',
			       run_id='99999999-9999-9999-9999-999999999999',
			       lease_expires_at = clock_timestamp() - interval '5 minutes', fence_token = 2
			 WHERE work_item_id=$1::uuid`, item); err != nil {
			t.Fatalf("stage death: %v", err)
		}

		// PIN — the fresh assembly the run controller persists at Claiming.
		// The snapshot's WorkItemRevision encodes work_item.updated_at
		// (ADR-001 opaque token); the lap-2 dispatch re-reads it exactly.
		fresh, err := asm.Assemble(ctx, contextasm.AssembleRequest{
			Run: run, Agent: agent, Project: project,
			TeamID: string(team.UID), ContextWindow: window,
		})
		if err != nil {
			t.Fatalf("pin assembly: %v", err)
		}
		var pinnedAt time.Time
		if err := db.QueryRowContext(ctx,
			`SELECT updated_at FROM coord.work_item WHERE id=$1::uuid`, item).Scan(&pinnedAt); err != nil {
			t.Fatalf("read pinned updated_at: %v", err)
		}

		// Pass 1: death detected → RetryEnter (checkout released, lap entered).
		driver, _ := newTestDriver(cl, db, coord.DefaultProdResumeConfig())
		if _, err := driver.Reconcile(ctx, request("run-1")); err != nil {
			t.Fatalf("death drive: %v", err)
		}
		// Pass 2: the requeued lap re-acquires the in_progress lane — the
		// no-op §6.2 mark that minted the phantom revision live — and runs
		// the machine to terminal.
		if _, err := driver.Reconcile(ctx, request("run-1")); err != nil {
			t.Fatalf("lap drive: %v", err)
		}
		if got := stepOf(t, db, item); got != reconcile.StepSucceeded {
			t.Fatalf("lap step = %q, want succeeded", got)
		}

		// RESOLVE — the exact call that wedged live: the dispatch build
		// re-reads the pinned snapshot (Existing set, window pinned off the
		// snapshot the way assembleSystemContext does) and the pinned
		// revision must still match work_item.updated_at.
		resumeWindow := window
		if fresh.Snapshot != nil && fresh.Snapshot.ContextWindow != nil {
			resumeWindow = *fresh.Snapshot.ContextWindow
		}
		if _, err := asm.Assemble(ctx, contextasm.AssembleRequest{
			Run: run, Agent: agent, Project: project,
			TeamID: string(team.UID), ContextWindow: resumeWindow,
			Existing: fresh.Snapshot,
		}); err != nil {
			t.Fatalf("pinned revision no longer resolves after the no-op re-acquire — the ISI-4217 dispatch wedge (ISI-4298 guard defeated): %v", err)
		}
		var lane string
		var updatedAt time.Time
		if err := db.QueryRowContext(ctx,
			`SELECT state, updated_at FROM coord.work_item WHERE id=$1::uuid`, item).Scan(&lane, &updatedAt); err != nil {
			t.Fatalf("post-lap read: %v", err)
		}
		if lane != "in_progress" {
			t.Fatalf("lane = %q, want in_progress (kept across the retry lap)", lane)
		}
		if !updatedAt.Equal(pinnedAt) {
			t.Fatalf("no-op re-acquire moved updated_at: pinned %s, now %s — phantom revision minted (ISI-4298)",
				pinnedAt.UTC().Format(time.RFC3339Nano), updatedAt.UTC().Format(time.RFC3339Nano))
		}

		// Teeth: a REAL edit must still break the pin — deterministic resume
		// keeps failing loud on genuine edits (0012's generated column is
		// excluded, title is not).
		if _, err := db.ExecContext(ctx,
			`UPDATE coord.work_item SET title='edited after pin' WHERE id=$1::uuid`, item); err != nil {
			t.Fatalf("real edit: %v", err)
		}
		if _, err := asm.Assemble(ctx, contextasm.AssembleRequest{
			Run: run, Agent: agent, Project: project,
			TeamID: string(team.UID), ContextWindow: resumeWindow,
			Existing: fresh.Snapshot,
		}); err == nil {
			t.Fatal("real edit still resolved the pinned revision — the WorkItemRevision pin lost its teeth")
		}
	})
}

func request(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}}
}
