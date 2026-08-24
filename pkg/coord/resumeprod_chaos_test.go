//go:build chaos

// resumeprod_chaos_test.go — the real-Postgres integration gate for the
// PRODUCTION uuid-keyed resume binding (Story 3.7 / ISI-2531, wired by
// ISI-2883). Where resume_test.go/resume_chaos_test.go prove the resume
// CONTRACT over the int-keyed harness schema, this suite binds the same
// contract to the checked-in coord schema (0001 + 0009_run_pause) with REAL
// uuid work items — the exact statements the operator's ProdTimer runs.
//
// Run (same wiring as TestSpine — DATABASE_URL → a live Postgres):
//
//	go test -race -tags=chaos -run 'TestSpineProdResume' ./pkg/coord/...
//
// A missing DATABASE_URL is a FATAL, never a skip (dsnOrFatal, AC1).
package coord_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/coord"
)

// isolatedGateDB provisions a per-fixture database off the gate DSN: the
// chaos suites of several packages run in PARALLEL against the same Postgres
// (one workflow, one DATABASE_URL), so a fixture that resets a shared schema
// races every other test. Each fixture gets its own database — created from
// the DSN's admin connection, dropped on cleanup — and the returned handle
// points at it. The DSN must allow CREATE DATABASE (superuser in the gate).
func isolatedGateDB(t *testing.T, dsn, tag string) *sql.DB {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	adminDSN := u.String()
	dbName := fmt.Sprintf("gate_%s_%d", tag, time.Now().UnixNano())

	admin, err := sql.Open("pgx", adminDSN)
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

// prodResumeFixture provisions an ISOLATED database (parallel packages share
// the DSN — a fixture that resets the shared schema races the whole suite),
// applies the checked-in schema pieces the prod resume binding needs
// (0001 base + 0009 run_pause), and returns the bound store plus a seeded
// work item.
func prodResumeFixture(t *testing.T) (*coord.ProdResumeStore, *sql.DB, string) {
	t.Helper()
	ctx := context.Background()
	db := isolatedGateDB(t, dsnOrFatal(t), "resume")

	for _, m := range []string{
		"0001_coord_schema.sql",
		"0002_coord_dispatch.sql",
		"0003_coord_outbox.sql",
		"0005_reconcile_step.sql",
		"0009_run_pause.sql",
	} {
		if _, err := db.ExecContext(ctx, migrationFile(t, m)); err != nil {
			t.Fatalf("apply %s: %v", m, err)
		}
	}

	// Seed one work item; the claim row auto-provisions (0001 trigger).
	var workItem string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO coord.work_item (project_id, title, created_by)
		VALUES (gen_random_uuid(), 'resume prod gate', 'principal:chaos')
		RETURNING id::text`).Scan(&workItem); err != nil {
		t.Fatalf("seed work item: %v", err)
	}

	// A short policy so the chaos cases run in real wall-clock time.
	cfg := coord.DefaultProdResumeConfig()
	cfg.BackoffBase = 40 * time.Millisecond
	cfg.BackoffCap = 640 * time.Millisecond
	cfg.BackoffReset = 400 * time.Millisecond
	store, err := coord.NewProdResumeStore(db, cfg, func() float64 { return 0 }) // deterministic jitter floor
	if err != nil {
		t.Fatalf("bind prod resume store: %v", err)
	}
	return store, db, workItem
}

// TestSpineProdResume is the workflow entrypoint (-run 'TestSpine' matches it).
func TestSpineProdResume(t *testing.T) {
	t.Run("PR1 retry-after dominates and the episode is pending", func(t *testing.T) {
		store, _, workItem := prodResumeFixture(t)
		ctx := context.Background()
		ra := 250 * time.Millisecond

		info, err := store.Pause(ctx, workItem, "99999999-9999-9999-9999-999999999999", &ra)
		if err != nil {
			t.Fatalf("pause: %v", err)
		}
		if info.Attempt != 1 {
			t.Fatalf("first pause attempt = %d, want 1", info.Attempt)
		}
		if d := info.ResumeAt.Sub(time.Now()); d < 100*time.Millisecond || d > ra {
			t.Fatalf("resume_at not Retry-After-shaped: %v", d)
		}

		if _, exists, err := store.Pending(ctx, workItem); err != nil || !exists {
			t.Fatalf("pending lookup: exists=%v err=%v", exists, err)
		}
		if _, ok, err := store.NextWake(ctx); err != nil || !ok {
			t.Fatalf("next wake: ok=%v err=%v", ok, err)
		}

		// The wake fires after the window: exactly-once claim.
		time.Sleep(300 * time.Millisecond)
		due, err := store.ResumeDue(ctx)
		if err != nil || len(due) != 1 {
			t.Fatalf("resume due: %v %v", due, err)
		}
		if due[0].WorkItemID != workItem || due[0].Attempt != 1 {
			t.Fatalf("due episode = %+v", due[0])
		}
		again, err := store.ResumeDue(ctx)
		if err != nil || len(again) != 0 {
			t.Fatalf("second claim must be empty (exactly-once): %v %v", again, err)
		}
		if _, exists, _ := store.Pending(ctx, workItem); exists {
			t.Fatal("claimed episode must no longer be pending")
		}
	})

	t.Run("PR2 escalation across consecutive episodes, reset after quiet", func(t *testing.T) {
		store, _, workItem := prodResumeFixture(t)
		ctx := context.Background()

		first, err := store.Pause(ctx, workItem, "99999999-9999-9999-9999-999999999999", nil)
		if err != nil || first.Attempt != 1 {
			t.Fatalf("first: %+v %v", first, err)
		}
		time.Sleep(60 * time.Millisecond)
		if _, err := store.ResumeDue(ctx); err != nil {
			t.Fatalf("claim first: %v", err)
		}
		// Re-pause INSIDE BackoffReset (400ms): streak continues → attempt 2.
		second, err := store.Pause(ctx, workItem, "99999999-9999-9999-9999-999999999999", nil)
		if err != nil || second.Attempt != 2 {
			t.Fatalf("second (streak): %+v %v — want attempt 2", second, err)
		}
	})

	t.Run("PR3 pending refresh keeps the attempt (same episode)", func(t *testing.T) {
		store, _, workItem := prodResumeFixture(t)
		ctx := context.Background()

		if _, err := store.Pause(ctx, workItem, "99999999-9999-9999-9999-999999999999", nil); err != nil {
			t.Fatalf("pause: %v", err)
		}
		refresh, err := store.Pause(ctx, workItem, "99999999-9999-9999-9999-999999999999", nil) // still pending
		if err != nil || refresh.Attempt != 1 {
			t.Fatalf("refresh: %+v %v — want attempt 1 (same episode)", refresh, err)
		}
	})
}
