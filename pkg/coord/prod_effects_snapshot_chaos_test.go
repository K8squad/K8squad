//go:build chaos

// prod_effects_snapshot_chaos_test.go — the real-Postgres integration gate for the 8.7c build-snapshot
// emit (ISI-2903). It reuses the prod_effects_chaos_test.go harness (seedEffects / dsnOrFatal / openDB
// / migrationFile / countEffectRows, same coord_test package) and applies 0010_build_snapshot.sql on
// top of the seeded schema, then proves the Collect-time build-snapshot contract:
//
//	S1 emit success        Collect with a snapshotter upserts one kind='build-snapshot' artifact
//	                       carrying uri/sha256 + the summary meta, audited once.
//	S2 re-entry idempotent re-driving Collect republishes the SAME row (DO NOTHING), never a dupe.
//	S3 capture failure     a failing snapshotter is a LEGIBLE 'build_snapshot_unavailable' audit and
//	                       NO run failure (Err()==nil) and NO artifact row — never a silent absence.
//	S4 snapshot-off        no snapshotter → no build-snapshot row (pre-8.7c behavior preserved).

package coord_test

import (
	"context"
	"errors"
	"testing"

	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/reconcile"
)

// fakeSnapshotter is a deterministic BuildSnapshotter for the emit gate.
type fakeSnapshotter struct {
	snap coord.BuildSnapshot
	err  error
}

func (f fakeSnapshotter) Snapshot(context.Context) (coord.BuildSnapshot, error) {
	return f.snap, f.err
}

func applySnapshotMigration(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()
	if _, err := openDB(t, dsn).ExecContext(ctx, migrationFile(t, "0010_build_snapshot.sql")); err != nil {
		t.Fatalf("apply 0010_build_snapshot.sql: %v", err)
	}
}

func TestProdEffects_BuildSnapshot(t *testing.T) {
	dsn := dsnOrFatal(t)
	ctx := context.Background()

	okSnap := coord.BuildSnapshot{
		URI:    "sha256:cafef00d",
		SHA256: "cafef00d",
		Meta: map[string]any{
			"base": "aaaa1111", "runRef": "run", "commit": "bbbb2222",
			"fileCount": 3, "totalAdditions": 12, "totalDeletions": 4, "truncated": false,
		},
	}

	// S1 + S2: emit success is a single content-addressed, re-entry-idempotent row + one audit.
	t.Run("S1_S2_emit_idempotent", func(t *testing.T) {
		eff, _, _, wi, _ := seedEffects(t, ctx, dsn)
		applySnapshotMigration(t, ctx, dsn)
		eff.WithSnapshotter(fakeSnapshotter{snap: okSnap})

		for i := 0; i < 3; i++ {
			eff.Collect(reconcile.RunID+"/patch", "diff-bytes", true)
		}
		if err := eff.Err(); err != nil {
			t.Fatalf("collect error: %v", err)
		}
		if got := countEffectRows(t, ctx, dsn,
			`SELECT count(*) FROM coord.artifact WHERE work_item_id=$1::uuid AND kind='build-snapshot'`, wi); got != 1 {
			t.Fatalf("build-snapshot rows = %d after 3 collects, want 1 (idempotent republish)", got)
		}
		if got := countEffectRows(t, ctx, dsn,
			`SELECT count(*) FROM coord.audit_log WHERE work_item_id=$1::uuid AND event_type='build_snapshot_registered'`, wi); got != 1 {
			t.Fatalf("build_snapshot_registered audit rows = %d, want 1 (first publish only)", got)
		}
		// The summary meta round-trips into the jsonb column.
		var uri string
		var fileCount int
		if err := openDB(t, dsn).QueryRowContext(ctx,
			`SELECT uri, (meta->>'fileCount')::int FROM coord.artifact WHERE work_item_id=$1::uuid AND kind='build-snapshot'`,
			wi).Scan(&uri, &fileCount); err != nil {
			t.Fatalf("read build-snapshot row: %v", err)
		}
		if uri != "sha256:cafef00d" {
			t.Errorf("uri = %q, want sha256:cafef00d", uri)
		}
		if fileCount != 3 {
			t.Errorf("meta.fileCount = %d, want 3", fileCount)
		}
	})

	// S3: a capture failure degrades legibly — audit row, no run failure, no artifact.
	t.Run("S3_capture_failure_legible", func(t *testing.T) {
		eff, _, _, wi, _ := seedEffects(t, ctx, dsn)
		applySnapshotMigration(t, ctx, dsn)
		eff.WithSnapshotter(fakeSnapshotter{err: errors.New("worktree gone")})

		eff.Collect(reconcile.RunID+"/patch", "diff-bytes", true)

		if err := eff.Err(); err != nil {
			t.Fatalf("a snapshot capture failure must NOT fail the run, got Err()=%v", err)
		}
		if got := countEffectRows(t, ctx, dsn,
			`SELECT count(*) FROM coord.artifact WHERE work_item_id=$1::uuid AND kind='build-snapshot'`, wi); got != 0 {
			t.Fatalf("build-snapshot rows = %d after capture failure, want 0", got)
		}
		if got := countEffectRows(t, ctx, dsn,
			`SELECT count(*) FROM coord.audit_log WHERE work_item_id=$1::uuid AND event_type='build_snapshot_unavailable'`, wi); got != 1 {
			t.Fatalf("build_snapshot_unavailable audit = %d, want 1 (legible 'no build view')", got)
		}
		// The base patch artifact still lands — the run's other effects are unaffected.
		if got := countEffectRows(t, ctx, dsn,
			`SELECT count(*) FROM coord.artifact WHERE work_item_id=$1::uuid AND kind='patch'`, wi); got != 1 {
			t.Fatalf("patch artifact rows = %d, want 1 (snapshot failure must not block it)", got)
		}
	})

	// S4: snapshot-off (no snapshotter) preserves the pre-8.7c behavior — no build-snapshot row.
	t.Run("S4_snapshot_off", func(t *testing.T) {
		eff, _, _, wi, _ := seedEffects(t, ctx, dsn)
		applySnapshotMigration(t, ctx, dsn)

		eff.Collect(reconcile.RunID+"/patch", "diff-bytes", true)
		if err := eff.Err(); err != nil {
			t.Fatalf("collect error: %v", err)
		}
		if got := countEffectRows(t, ctx, dsn,
			`SELECT count(*) FROM coord.artifact WHERE work_item_id=$1::uuid AND kind='build-snapshot'`, wi); got != 0 {
			t.Fatalf("build-snapshot rows = %d with snapshot-off, want 0", got)
		}
	})
}
