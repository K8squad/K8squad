//go:build discussion_integration

// ProdStore integration test against a REAL Postgres (ISI-2900, cursor review on
// PR #88): covers the SQL text, column order, and scan types of the production
// artifact-store binding against the SHIPPED migrations — plus the two behaviours
// only a real store can prove:
//
//   - the per-row digest verification (two rows sharing a uri; the row whose
//     registered sha256 does NOT match the bytes at the uri must fail closed),
//   - the (run_id, created_at, id) serving index from 0008 actually exists.
//
// Build-tag gated exactly like internal/discussion: CI provisions Postgres and runs
//
//	go test -tags=discussion_integration ./internal/artifactbrowser/...
//
// When DATABASE_URL is unset the test SKIPS, so a developer without Postgres is
// not blocked.
package artifactbrowser

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL unset — skipping the artifactbrowser ProdStore integration test (needs real Postgres)")
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

// applyShippedMigrations resets coord and applies the SHIPPED 0001 + 0008 files —
// not inline DDL — so drift between migration and reader goes RED here.
func applyShippedMigrations(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS coord CASCADE`); err != nil {
		t.Fatalf("reset coord schema: %v", err)
	}
	files := []string{"0001_coord_schema.sql", "0008_artifact_run_index.sql"}
	for _, name := range files {
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
			t.Fatalf("could not read shipped migration %s (cwd %s)", name, mustWd())
		}
		if _, err := db.ExecContext(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
}

func mustWd() string {
	wd, _ := os.Getwd()
	return wd
}

// seedWorkItem inserts one work item and returns its id.
func seedWorkItem(t *testing.T, db *sql.DB) string {
	t.Helper()
	var id string
	err := db.QueryRow(`
		INSERT INTO coord.work_item (project_id, title, created_by)
		VALUES (gen_random_uuid(), 'artifact-browser IT', 'user:it')
		RETURNING id::text`).Scan(&id)
	if err != nil {
		t.Fatalf("seed work_item: %v", err)
	}
	return id
}

// seedArtifact registers a coord.artifact row (and its backing audit_log payload
// when uri is a coord+audit:// pointer) with sha256 set to the digest OF RECORD —
// the jsonb-canonical payload::text bytes Postgres returns, exactly what
// ProdHandoffWriter.WriteHandoff hashes at registration — or to a deliberately
// wrong digest when corrupt=true.
func seedArtifact(t *testing.T, db *sql.DB, wi, run, kind, payload string, corrupt bool) Artifact {
	t.Helper()
	// jsonb canonicalizes whitespace/key order, so hashing the raw Go string
	// would register a digest nothing at the uri can ever hash to; read the
	// canonical bytes back from Postgres itself, like the real writer does.
	var auditID int64
	var canonical []byte
	err := db.QueryRow(`
		INSERT INTO coord.audit_log (work_item_id, run_id, event_type, principal, payload)
		VALUES ($1::uuid, $2::uuid, 'artifact_registered', 'user:it', $3::jsonb)
		RETURNING id, payload::text`, wi, run, payload).Scan(&auditID, &canonical)
	if err != nil {
		t.Fatalf("seed audit_log: %v", err)
	}
	sum := sha256.Sum256(canonical)
	digest := hex.EncodeToString(sum[:])
	if corrupt {
		bad := sha256.Sum256([]byte("tampered-bytes"))
		digest = hex.EncodeToString(bad[:])
	}
	uri := fmt.Sprintf("coord+audit://%d", auditID)
	var a Artifact
	err = db.QueryRow(`
		INSERT INTO coord.artifact (work_item_id, run_id, kind, uri, sha256, created_at)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, now() + $6::interval)
		RETURNING id::text, work_item_id::text, run_id::text, kind, uri, sha256, created_at`,
		wi, run, kind, uri, digest, "1 second").Scan(
		&a.ID, &a.WorkItemID, &a.RunID, &a.Kind, &a.URI, &a.SHA256, &a.CreatedAt)
	if err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	return a
}

func TestProdStore_ListGetContent(t *testing.T) {
	db := openTestDB(t)
	applyShippedMigrations(t, db)
	ctx := context.Background()

	wi := seedWorkItem(t, db)
	run := "11111111-1111-4111-8111-111111111111"
	run2 := "22222222-2222-4222-8222-222222222222"

	doc := map[string]any{"did": []string{"integration"}}
	raw, _ := json.Marshal(doc)
	first := seedArtifact(t, db, wi, run, "handoff", string(raw), false)
	second := seedArtifact(t, db, wi, run, "report", `{"n":2}`, false)
	otherRun := seedArtifact(t, db, wi, run2, "handoff", `{"run":2}`, false)

	store, err := NewProdStore(db)
	if err != nil {
		t.Fatalf("NewProdStore: %v", err)
	}

	// List is scoped to the run and deterministic (created_at, id).
	rows, err := store.ListByRun(ctx, run)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(rows) != 2 || rows[0].ID != first.ID || rows[1].ID != second.ID {
		t.Fatalf("ListByRun = %+v, want [%s %s] in created_at order", rows, first.ID, second.ID)
	}

	// GetByRunAndID is a targeted single-row read scoped to the run.
	got, found, err := store.GetByRunAndID(ctx, run, second.ID)
	if err != nil || !found || got.ID != second.ID {
		t.Fatalf("GetByRunAndID = (%+v, %v, %v)", got, found, err)
	}
	if _, found, _ := store.GetByRunAndID(ctx, run, otherRun.ID); found {
		t.Fatalf("cross-run id resolved inside run %s", run)
	}

	// Content verifies the bytes against THIS row's registered sha256.
	payload, err := store.Content(ctx, first)
	if err != nil {
		t.Fatalf("Content: %v", err)
	}
	var back map[string]any
	if json.Unmarshal(payload, &back) != nil || len(back) != 1 {
		t.Fatalf("Content payload = %s", payload)
	}
}

// TestProdStore_SharedURIDigestFailClosed — the cursor finding: two rows can
// share one uri (the schema's uniqueness is (work_item, run, kind), NOT uri), and
// AuditHandoffContent's uri-join may verify against EITHER row's digest. The
// per-row check in ProdStore.Content must fail closed for the row whose
// registered sha256 does not match the bytes — regardless of which row the
// resolver's join picked.
func TestProdStore_SharedURIDigestFailClosed(t *testing.T) {
	db := openTestDB(t)
	applyShippedMigrations(t, db)
	ctx := context.Background()

	wi := seedWorkItem(t, db)
	run := "33333333-3333-4333-8333-333333333333"

	// One good row, then a second row pointing at the SAME uri but registered
	// with a digest of different bytes (corrupt=true).
	good := seedArtifact(t, db, wi, run, "handoff", `{"did":["x"]}`, false)
	bad := seedArtifact(t, db, wi, run, "report", `{"did":["x"]}`, true)
	bad.URI = good.URI // same coord+audit:// pointer, sha256 of "tampered-bytes"

	store, err := NewProdStore(db)
	if err != nil {
		t.Fatalf("NewProdStore: %v", err)
	}
	if _, err := store.Content(ctx, bad); err == nil {
		t.Fatalf("Content on a digest-mismatched row must fail closed, got nil error")
	}
	// The good row still resolves (its digest matches the bytes at the uri).
	if _, err := store.Content(ctx, good); err != nil {
		t.Fatalf("Content on the matching row: %v", err)
	}
}

// TestProdStore_NonUUIDShortCircuit — non-uuid ids never reach Postgres (which
// would 500 on the ::uuid cast); they answer not-found on both read shapes.
func TestProdStore_NonUUIDShortCircuit(t *testing.T) {
	db := openTestDB(t)
	applyShippedMigrations(t, db)
	store, err := NewProdStore(db)
	if err != nil {
		t.Fatalf("NewProdStore: %v", err)
	}
	if rows, err := store.ListByRun(context.Background(), "dev-run"); err != nil || rows != nil {
		t.Fatalf("ListByRun(dev-run) = (%v, %v), want (nil, nil)", rows, err)
	}
	if _, found, err := store.GetByRunAndID(context.Background(), "dev-run", "x"); err != nil || found {
		t.Fatalf("GetByRunAndID(dev-run, x) = (_, %v, %v), want found=false err=nil", found, err)
	}
}

// TestProdStore_ServingIndex — the 0008 index exists with run_id leading, so the
// list/content reads ride it rather than a sequential scan.
func TestProdStore_ServingIndex(t *testing.T) {
	db := openTestDB(t)
	applyShippedMigrations(t, db)
	var def string
	err := db.QueryRow(`
		SELECT indexdef FROM pg_indexes
		 WHERE schemaname = 'coord' AND tablename = 'artifact'
		   AND indexname = 'idx_artifact_run_created'`).Scan(&def)
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("idx_artifact_run_created missing — was 0008 applied?")
	}
	if err != nil {
		t.Fatalf("read indexdef: %v", err)
	}
	want := "CREATE INDEX idx_artifact_run_created ON coord.artifact USING btree (run_id, created_at, id)"
	if def != want {
		t.Fatalf("indexdef =\n  %s\nwant\n  %s", def, want)
	}
}
