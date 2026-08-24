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

// driveFixture provisions an ISOLATED database with the full checked-in coord
// schema (0001→0009), seeds one work item, and returns the DB handle + the
// item's uuid.
func driveFixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	ctx := context.Background()
	db := isolatedGateDB(t, dsnOrFatal(t), "drive")
	for _, m := range []string{
		"0001_coord_schema.sql",
		"0002_coord_dispatch.sql",
		"0003_coord_outbox.sql",
		"0005_reconcile_step.sql",
		"0007_reconcile_effects.sql",
		"0009_run_pause.sql",
	} {
		if _, err := db.ExecContext(ctx, migrationFile(t, m)); err != nil {
			t.Fatalf("apply %s: %v", m, err)
		}
	}

	var item string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO coord.work_item (project_id, title, created_by)
		VALUES (gen_random_uuid(), 'drive gate item', 'principal:chaos')
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

		// The requeued drive completes the lap: terminal succeeded.
		if _, err := driver.Reconcile(ctx, request("run-1")); err != nil {
			t.Fatalf("lap drive: %v", err)
		}
		if got := stepOf(t, db, item); got != reconcile.StepSucceeded {
			t.Fatalf("lap step = %q, want succeeded", got)
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
}

func request(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}}
}
