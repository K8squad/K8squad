//go:build discussion_integration

// Integration test for the production Run resolution (ISI-3207) against a REAL Postgres and the
// SHIPPED coord migrations (0001 + 0010). It proves what only a real store can: the SQL text, column
// order, scan types, and the actual JOIN semantics of tenancy resolution (claim.run_id →
// holder_principal ⋈ work_item.team_id) plus the build-snapshot meta projection (0010).
//
// Build-tag gated exactly like internal/artifactbrowser: CI provisions Postgres and runs
//
//	go test -tags=discussion_integration ./internal/runsource/...
//
// When DATABASE_URL is unset the test SKIPS, so a developer without Postgres is not blocked.
package runsource

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"

	"github.com/K8squad/K8squad/internal/buildbrowser"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL unset — skipping the runsource integration test (needs real Postgres)")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// applyShippedMigrations resets coord and applies the SHIPPED 0001 + 0010 files — not inline DDL — so
// drift between migration and reader goes RED here.
func applyShippedMigrations(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS coord CASCADE`); err != nil {
		t.Fatalf("reset coord schema: %v", err)
	}
	for _, name := range []string{"0001_coord_schema.sql", "0010_build_snapshot.sql"} {
		var sqlBytes []byte
		var err error
		for _, c := range []string{
			filepath.Join("..", "..", "db", "migrations", name),
			filepath.Join("db", "migrations", name),
		} {
			if sqlBytes, err = os.ReadFile(c); err == nil {
				break
			}
		}
		if sqlBytes == nil {
			t.Fatalf("could not read shipped migration %s", name)
		}
		if _, err := db.ExecContext(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
}

// seedRun inserts a work_item (with team_id, which the 0001 trigger inherits from its project on a
// child; a root item keeps the explicit team_id we set) and claims it under holder/run. Returns the
// work_item id. The 0001 provision trigger creates the claim row; we UPDATE it into custody.
func seedRun(t *testing.T, db *sql.DB, team uuid.UUID, holder, run string) string {
	t.Helper()
	var wi string
	// Root work_item: the BEFORE trigger only inherits team_id for a CHILD (parent_id set); a root
	// keeps the team_id we provide, so set it explicitly here.
	err := db.QueryRow(`
		INSERT INTO coord.work_item (project_id, team_id, title, created_by)
		VALUES (gen_random_uuid(), $1::uuid, 'runsource IT', 'user:it')
		RETURNING id::text`, team.String()).Scan(&wi)
	if err != nil {
		t.Fatalf("seed work_item: %v", err)
	}
	if _, err := db.Exec(`
		UPDATE coord.claim SET holder_principal = $2, run_id = $3::uuid, fence_token = 1
		 WHERE work_item_id = $1::uuid`, wi, holder, run); err != nil {
		t.Fatalf("claim run: %v", err)
	}
	return wi
}

// seedSnapshot registers a kind='build-snapshot' artifact row with a summary meta object (0010).
func seedSnapshot(t *testing.T, db *sql.DB, wi, run, uri, sha string, meta map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(meta)
	if _, err := db.Exec(`
		INSERT INTO coord.artifact (work_item_id, run_id, kind, uri, sha256, meta)
		VALUES ($1::uuid, $2::uuid, 'build-snapshot', $3, $4, $5::jsonb)`,
		wi, run, uri, sha, string(raw)); err != nil {
		t.Fatalf("seed build-snapshot: %v", err)
	}
}

func TestPostgresRunSource_Lookup(t *testing.T) {
	db := openTestDB(t)
	applyShippedMigrations(t, db)
	ctx := context.Background()

	team := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	run := "11111111-1111-4111-8111-111111111111"
	wi := seedRun(t, db, team, "agent:owner", run)
	seedSnapshot(t, db, wi, run, "sha256:deadbeef", "deadbeef", map[string]any{
		"base": "base00", "runRef": "run/main", "commit": "head99", "fileCount": 5,
		"totalAdditions": 40, "totalDeletions": 3, "truncated": false,
	})

	src, err := NewPostgresRunSource(db)
	if err != nil {
		t.Fatalf("NewPostgresRunSource: %v", err)
	}

	// Known Run resolves full tenancy + git coords.
	m, found, err := src.Lookup(ctx, run)
	if err != nil || !found {
		t.Fatalf("Lookup(known) = (%+v, %v, %v)", m, found, err)
	}
	if m.RunID != run || m.TeamID != team || m.Principal != "agent:owner" {
		t.Fatalf("tenancy = %+v, want run/%s team/%s owner/agent:owner", m, run, team)
	}
	if m.HeadRef != "run/main" || m.BaseRef != "base00" {
		t.Fatalf("git coords = (head %q base %q), want (run/main, base00)", m.HeadRef, m.BaseRef)
	}
	if m.RepoPath != "" {
		t.Fatalf("RepoPath = %q, want empty (pod-local worktree unreachable)", m.RepoPath)
	}

	// Unknown Run → found=false (existence-hiding).
	if _, found, err := src.Lookup(ctx, "22222222-2222-4222-8222-222222222222"); err != nil || found {
		t.Fatalf("Lookup(unknown) = (found %v, err %v), want (false, nil)", found, err)
	}
}

// TestPostgresRunSource_NoSnapshotYet proves a claimed Run that has not reached Collecting (no
// build-snapshot artifact) still resolves its tenancy — the artifact browser needs only Team/owner —
// with empty git refs (the build reader then degrades to not-found).
func TestPostgresRunSource_NoSnapshotYet(t *testing.T) {
	db := openTestDB(t)
	applyShippedMigrations(t, db)
	ctx := context.Background()

	team := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	run := "33333333-3333-4333-8333-333333333333"
	seedRun(t, db, team, "agent:owner2", run)

	src, _ := NewPostgresRunSource(db)
	m, found, err := src.Lookup(ctx, run)
	if err != nil || !found {
		t.Fatalf("Lookup = (%+v, %v, %v)", m, found, err)
	}
	if m.TeamID != team || m.Principal != "agent:owner2" {
		t.Fatalf("tenancy = %+v", m)
	}
	if m.HeadRef != "" || m.BaseRef != "" {
		t.Fatalf("git coords = (head %q base %q), want empty", m.HeadRef, m.BaseRef)
	}
}

func TestPostgresSnapshotStore_SnapshotByRun(t *testing.T) {
	db := openTestDB(t)
	applyShippedMigrations(t, db)
	ctx := context.Background()

	team := uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	run := "44444444-4444-4444-8444-444444444444"
	wi := seedRun(t, db, team, "agent:o", run)
	seedSnapshot(t, db, wi, run, "sha256:cafe", "cafe", map[string]any{
		"base": "b0", "runRef": "run/x", "commit": "c9", "fileCount": 12,
		"totalAdditions": 100, "totalDeletions": 20, "truncated": false,
	})

	store, err := NewPostgresSnapshotStore(db)
	if err != nil {
		t.Fatalf("NewPostgresSnapshotStore: %v", err)
	}
	row, found, err := store.SnapshotByRun(ctx, run)
	if err != nil || !found {
		t.Fatalf("SnapshotByRun = (%+v, %v, %v)", row, found, err)
	}
	if row.URI != "sha256:cafe" || row.SHA256 != "cafe" {
		t.Fatalf("pointer = (uri %q sha %q)", row.URI, row.SHA256)
	}
	want := buildbrowser.SnapshotSummary{
		Base: "b0", RunRef: "run/x", Commit: "c9", FileCount: 12,
		TotalAdditions: 100, TotalDeletions: 20, Truncated: false,
	}
	if row.Summary != want {
		t.Fatalf("summary = %+v, want %+v", row.Summary, want)
	}

	// Unknown Run → found=false.
	if _, found, _ := store.SnapshotByRun(ctx, "55555555-5555-4555-8555-555555555555"); found {
		t.Fatalf("SnapshotByRun(unknown) found=true, want false")
	}
}

// TestSnapshotStoreReader_MetaFromRealStore end-to-ends the production build Meta path against a real
// store: a completed Run's summary is served (live:false) with no bundle hydration, and byte reads
// degrade to ErrNotFound because no BundleResolver is wired (v1, pre-ISI-2900).
func TestSnapshotStoreReader_MetaFromRealStore(t *testing.T) {
	db := openTestDB(t)
	applyShippedMigrations(t, db)
	ctx := context.Background()

	team := uuid.MustParse("dddddddd-dddd-4ddd-8ddd-dddddddddddd")
	run := "66666666-6666-4666-8666-666666666666"
	wi := seedRun(t, db, team, "agent:o", run)
	seedSnapshot(t, db, wi, run, "sha256:f00d", "f00d", map[string]any{
		"base": "bb", "runRef": "run/y", "commit": "cc", "fileCount": 3, "truncated": false,
	})

	store, _ := NewPostgresSnapshotStore(db)
	reader := NewSnapshotStoreReader(store, nil)

	m, err := reader.Meta(ctx, buildbrowser.RunMeta{RunID: run})
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if m.Head != "cc" || m.Base != "bb" || m.ChangedFiles != 3 || m.Live {
		t.Fatalf("Meta = %+v", m)
	}
	if _, err := reader.Diff(ctx, buildbrowser.RunMeta{RunID: run}); !errors.Is(err, buildbrowser.ErrNotFound) {
		t.Fatalf("Diff err = %v, want ErrNotFound (no blob store)", err)
	}
}
