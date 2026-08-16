//go:build integration

// Integration proof of the capture half of the event seam against a REAL
// Postgres with the shipped coord schema (0001) + the outbox migration (0003).
// Run in CI with DATABASE_URL set:
//
//	go test -tags=integration ./pkg/events/ -run TestOutbox
//
// These cases exercise what the pure-Go unit tests (relay_test.go) model but
// cannot prove about the actual DDL: the same-txn atomicity of Capture (C1), the
// schema's set-once published_at guard (C2), and the append-only immutability
// guard (C4). The relay→NATS publish half (C2 subject / C3 at-least-once
// end-to-end) is proved in pkg/events/jetstream against a real bus.
package events

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"
)

const testProject = "11111111-1111-1111-1111-111111111111"

func integrationDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL unset — skipping outbox integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS coord CASCADE`); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	for _, f := range []string{"0001_coord_schema.sql", "0003_coord_outbox.sql"} {
		sqlText, err := os.ReadFile(filepath.Join("..", "..", "db", "migrations", f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := db.ExecContext(ctx, string(sqlText)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	return db
}

// insertWorkItem creates one work_item in-txn and returns its id (a state
// change the seam must capture atomically).
func insertWorkItem(ctx context.Context, tx *sql.Tx) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx,
		`INSERT INTO coord.work_item (project_id, title, state, created_by)
		 VALUES ($1, 'itest', 'todo', 'seed') RETURNING id`, testProject).Scan(&id)
	return id, err
}

func outboxCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM coord.outbox`).Scan(&n); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	return n
}

// C1: a rolled-back state change captures NO event (no phantom); a committed one
// captures EXACTLY one (no loss). The append is atomic with the state change.
func TestOutbox_CaptureIsAtomicWithStateChange(t *testing.T) {
	db := integrationDB(t)
	ctx := context.Background()

	// (a) ROLLBACK arm — state change + capture both vanish.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	id, err := insertWorkItem(ctx, tx)
	if err != nil {
		t.Fatalf("insert work_item: %v", err)
	}
	if err := CaptureForWorkItem(ctx, tx, id, "", "claimed", nil); err != nil {
		t.Fatalf("capture: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if n := outboxCount(t, db); n != 0 {
		t.Fatalf("rolled-back capture left %d outbox rows, want 0 (phantom event)", n)
	}

	// (b) COMMIT arm — exactly one event, tenancy derived, published_at NULL.
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	id, err = insertWorkItem(ctx, tx)
	if err != nil {
		t.Fatalf("insert work_item: %v", err)
	}
	if err := CaptureForWorkItem(ctx, tx, id, "", "claimed", []byte(`{"v":1}`)); err != nil {
		t.Fatalf("capture: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var entity, project, eventType string
	var published sql.NullTime
	if err := db.QueryRow(
		`SELECT entity, project_id::text, event_type, published_at FROM coord.outbox`).
		Scan(&entity, &project, &eventType, &published); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	if entity != "work_item" || project != testProject || eventType != "claimed" {
		t.Fatalf("captured event = %s/%s/%s, want work_item/%s/claimed", entity, project, eventType, testProject)
	}
	if published.Valid {
		t.Fatalf("published_at should start NULL (unflushed), got %v", published.Time)
	}
}

// C2 (schema teeth): published_at is set-once — a second, different stamp is
// rejected by the outbox_guard trigger, so a re-flush cannot rewrite it.
func TestOutbox_PublishedAtIsSetOnce(t *testing.T) {
	db := integrationDB(t)
	ctx := context.Background()
	id := captureOne(t, db)

	if _, err := db.ExecContext(ctx, `UPDATE coord.outbox SET published_at = now() WHERE id=$1`, id); err != nil {
		t.Fatalf("first stamp should succeed: %v", err)
	}
	// A second stamp to a DIFFERENT value must be rejected by the guard.
	_, err := db.ExecContext(ctx,
		`UPDATE coord.outbox SET published_at = now() + interval '1 hour' WHERE id=$1`, id)
	if err == nil {
		t.Fatal("re-stamping published_at should be rejected (set-once §17.4)")
	}
}

// C4 (schema teeth): event columns are immutable and rows are undeletable.
func TestOutbox_AppendOnly(t *testing.T) {
	db := integrationDB(t)
	ctx := context.Background()
	id := captureOne(t, db)

	if _, err := db.ExecContext(ctx, `UPDATE coord.outbox SET event_type='tampered' WHERE id=$1`, id); err == nil {
		t.Fatal("mutating a committed event column should be rejected (append-only)")
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM coord.outbox WHERE id=$1`, id); err == nil {
		t.Fatal("deleting a committed event should be rejected (append-only)")
	}
}

// SQLStore round-trip: Unpublished → MarkPublished → Depth.
func TestOutbox_SQLStoreRoundTrip(t *testing.T) {
	db := integrationDB(t)
	ctx := context.Background()
	id := captureOne(t, db)

	store := NewSQLStore(db)
	rows, err := store.Unpublished(ctx, 0)
	if err != nil || len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("Unpublished = %+v err=%v, want the one row", rows, err)
	}
	if rows[0].Entity != "work_item" || rows[0].EventType != "claimed" {
		t.Fatalf("row taxonomy wrong: %+v", rows[0])
	}
	if err := store.MarkPublished(ctx, id); err != nil {
		t.Fatalf("MarkPublished: %v", err)
	}
	total, unflushed, err := store.Depth(ctx)
	if err != nil || total != 1 || unflushed != 0 {
		t.Fatalf("Depth = %d/%d err=%v, want 1/0 after flush", total, unflushed, err)
	}
	// A second MarkPublished is an idempotent no-op (not a guard violation).
	if err := store.MarkPublished(ctx, id); err != nil {
		t.Fatalf("idempotent re-mark should be a no-op, got %v", err)
	}
}

// captureOne commits one work_item + claimed event and returns the outbox id.
func captureOne(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	id, err := insertWorkItem(ctx, tx)
	if err != nil {
		t.Fatalf("insert work_item: %v", err)
	}
	if err := CaptureForWorkItem(ctx, tx, id, "", "claimed", nil); err != nil {
		t.Fatalf("capture: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var oid int64
	if err := db.QueryRow(`SELECT id FROM coord.outbox ORDER BY id DESC LIMIT 1`).Scan(&oid); err != nil {
		t.Fatalf("read outbox id: %v", err)
	}
	return oid
}
