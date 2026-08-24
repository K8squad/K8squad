//go:build discussion_integration

// PostgresAuditTrailReader integration test against a REAL Postgres with the
// SHIPPED migrations (story 2.6 / ISI-2881): proves the SQL text, column order,
// scan types, cursor pagination, and the jsonb payload round-trip of the audit
// read model — the pieces a fake reader cannot.
//
// Same gate as internal/artifactbrowser: CI provisions Postgres and runs
//
//	go test -tags=discussion_integration ./internal/apiserver/ -run TestAuditTrailIntegration
//
// DATABASE_URL unset ⇒ SKIP (a developer without Postgres is not blocked).
package apiserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"
)

func openAuditTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL unset — skipping the audit-trail integration test (needs real Postgres)")
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

// applyAuditMigrations resets coord and applies the SHIPPED 0001 file (audit_log
// + work_item FK target live there) — not inline DDL — so drift between the
// migration and the reader goes RED here.
func applyAuditMigrations(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS coord CASCADE`); err != nil {
		t.Fatalf("reset coord schema: %v", err)
	}
	var mig []byte
	var err error
	for _, c := range []string{
		filepath.Join("..", "..", "db", "migrations", "0001_coord_schema.sql"),
		filepath.Join("db", "migrations", "0001_coord_schema.sql"),
	} {
		if mig, err = os.ReadFile(c); err == nil {
			break
		}
	}
	if mig == nil {
		t.Fatalf("could not read shipped migration 0001_coord_schema.sql (cwd %s)", mustWd())
	}
	if _, err := db.ExecContext(ctx, string(mig)); err != nil {
		t.Fatalf("apply 0001: %v", err)
	}
}

func mustWd() string {
	wd, _ := os.Getwd()
	return wd
}

// seedAuditRow inserts one audit event (work item rows pre-exist for the FK).
func seedAuditRow(t *testing.T, db *sql.DB, q AuditTrailQuery, eventType, principal string, payload string) int64 {
	t.Helper()
	var id int64
	err := db.QueryRow(`
		INSERT INTO coord.audit_log (work_item_id, run_id, event_type, principal, payload, from_state, to_state, fence_token, created_at)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, $9)
		RETURNING id`,
		uuidToStr(q.WorkItemID), uuidToStr(q.RunID), eventType, principal, payload,
		nil, nil, nil, time.Now().UTC(),
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed audit row: %v", err)
	}
	return id
}

func uuidToStr(u *uuid.UUID) any {
	if u == nil {
		return nil
	}
	return u.String()
}

func TestAuditTrailIntegrationQueriesAndCursor(t *testing.T) {
	db := openAuditTestDB(t)
	applyAuditMigrations(t, db)

	// One project + work item A and B for FK-targeted seeding.
	ctx := context.Background()
	project := uuid.New()
	wiA, wiB := uuid.New(), uuid.New()
	for _, wi := range []uuid.UUID{wiA, wiB} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO coord.work_item (id, project_id, title, created_by) VALUES ($1, $2, $3, $4)`,
			wi, project, "seed item", "user:seeder"); err != nil {
			t.Fatalf("seed work item: %v", err)
		}
	}

	reader := NewPostgresAuditTrailReader(db)

	// Seed across two actors and two work items; ids are monotonic (bigserial).
	idA1 := seedAuditRow(t, db, AuditTrailQuery{WorkItemID: &wiA}, "claim_acquired", "agent:coder", `{"detail":"fence 1"}`)
	idA2 := seedAuditRow(t, db, AuditTrailQuery{WorkItemID: &wiA}, "comment_added", "user:jane", `{"body":"hi"}`)
	_ = seedAuditRow(t, db, AuditTrailQuery{WorkItemID: &wiB}, "claim_acquired", "agent:coder", `{"detail":"fence 2"}`)

	// (1) work-item filter: exactly A's two rows, newest first.
	page, err := reader.Query(ctx, AuditTrailQuery{WorkItemID: &wiA, Limit: 50})
	if err != nil {
		t.Fatalf("query by work item: %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("work-item filter: want 2 events, got %d", len(page.Events))
	}
	if page.Events[0].ID != idA2 || page.Events[1].ID != idA1 {
		t.Fatalf("events not newest-first: got [%d %d], want [%d %d]", page.Events[0].ID, page.Events[1].ID, idA2, idA1)
	}
	if page.NextBefore != nil {
		t.Fatalf("partial-tail page must not carry a cursor, got %v", *page.NextBefore)
	}
	if page.Events[1].Payload == nil || string(page.Events[1].Payload) == "" {
		t.Fatalf("jsonb payload not round-tripped: %+v", page.Events[1].Payload)
	}
	var detail map[string]any
	if err := json.Unmarshal(page.Events[1].Payload, &detail); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}

	// (2) actor filter.
	page, err = reader.Query(ctx, AuditTrailQuery{Actor: "user:jane", Limit: 50})
	if err != nil {
		t.Fatalf("query by actor: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].EventType != "comment_added" {
		t.Fatalf("actor filter: got %+v", page.Events)
	}

	// (3) time window: a future `from` excludes everything seeded now.
	future := time.Now().UTC().Add(time.Hour)
	page, err = reader.Query(ctx, AuditTrailQuery{From: &future, Limit: 50})
	if err != nil {
		t.Fatalf("query by from: %v", err)
	}
	if len(page.Events) != 0 {
		t.Fatalf("future from: want 0 events, got %d", len(page.Events))
	}

	// (4) cursor pagination over the whole trail: limit 2 → page one holds the
	// two newest rows + a cursor; page two holds the oldest, cursor nil.
	page, err = reader.Query(ctx, AuditTrailQuery{Limit: 2})
	if err != nil {
		t.Fatalf("page one: %v", err)
	}
	if len(page.Events) != 2 || page.NextBefore == nil {
		t.Fatalf("page one shape wrong: %d events, cursor %v", len(page.Events), page.NextBefore)
	}
	if page.Events[0].ID == idA1 {
		t.Fatalf("page one must start at the newest row, got %d", page.Events[0].ID)
	}
	next, err := reader.Query(ctx, AuditTrailQuery{Limit: 2, Before: *page.NextBefore})
	if err != nil {
		t.Fatalf("page two: %v", err)
	}
	if len(next.Events) != 1 || next.Events[0].ID != idA1 || next.NextBefore != nil {
		t.Fatalf("page two shape wrong: %d events (first %d), cursor %v", len(next.Events), next.Events[0].ID, next.NextBefore)
	}

	// (5) run filter isolates run-scoped events.
	run := uuid.New()
	_ = seedAuditRow(t, db, AuditTrailQuery{RunID: &run}, "run_terminal", "agent:coder", `{"to":"succeeded"}`)
	page, err = reader.Query(ctx, AuditTrailQuery{RunID: &run, Limit: 50})
	if err != nil {
		t.Fatalf("query by run: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].EventType != "run_terminal" {
		t.Fatalf("run filter: got %+v", page.Events)
	}
}
