//go:build workitemindex_integration

package workitemindex

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx" for the coord source
)

// TestSQLSource_Integration exercises the actual SQL the production indexer runs against a real
// Postgres (DATABASE_URL set, -tags workitemindex_integration). It pins: the source='board' allowlist
// (a 'discussion' thread-run is excluded, matching the WS-B FTS corpus), the comment-thread
// aggregation, the GREATEST(updated_at, last-comment) activity watermark (a NEW comment advances the
// item even though coord.comment has no trigger on the parent), and the teamless passthrough.
func TestSQLSource_Integration(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set - skipping integration test")
	}
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// Minimal coord schema (the production tables; integration lane provisions them directly).
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS coord`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS coord.work_item (
			id uuid PRIMARY KEY,
			project_id uuid NOT NULL,
			team_id uuid,
			title text NOT NULL,
			body text,
			state text NOT NULL,
			created_by text NOT NULL,
			created_at timestamptz NOT NULL,
			updated_at timestamptz NOT NULL,
			source text NOT NULL DEFAULT 'board'
		)`); err != nil {
		t.Fatalf("work_item table: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS coord.comment (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			work_item_id uuid NOT NULL,
			author_principal text NOT NULL,
			body text NOT NULL,
			created_at timestamptz NOT NULL
		)`); err != nil {
		t.Fatalf("comment table: %v", err)
	}
	// Clean slate for a deterministic assertion.
	if _, err := db.ExecContext(ctx, `TRUNCATE coord.work_item, coord.comment`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	base := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	const (
		wiBoard     = "10000000-0000-0000-0000-000000000001"
		wiDiscuss   = "10000000-0000-0000-0000-000000000002"
		wiTeamless  = "10000000-0000-0000-0000-000000000003"
		wiCommented = "10000000-0000-0000-0000-000000000004"
		project     = "20000000-0000-0000-0000-000000000000"
		team        = "30000000-0000-0000-0000-000000000000"
	)
	_, err = db.ExecContext(ctx, `
		INSERT INTO coord.work_item (id, project_id, team_id, title, body, state, created_by, created_at, updated_at, source) VALUES
		($1,$9,$10,'Board ticket','needs SSO fix','todo','agent:coder',$5,$5,'board'),
		($2,$9,$10,'Thread run','ephemeral','in_progress','agent:intake',$6,$6,'discussion'),
		($3,$9,NULL,'Orphan','no team yet','backlog','alice@corp',$7,$7,'board'),
		($4,$9,$10,'Commented ticket','base body','in_review','bob@corp',$8,$8,'board')`,
		wiBoard, wiDiscuss, wiTeamless, wiCommented,
		base, base.Add(time.Minute), base.Add(2*time.Minute), base.Add(3*time.Minute),
		project, team)
	if err != nil {
		t.Fatalf("insert work items: %v", err)
	}
	// A comment on wiCommented lands AFTER its updated_at — the activity watermark must reflect it.
	commentAt := base.Add(10 * time.Minute)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO coord.comment (work_item_id, author_principal, body, created_at) VALUES
		($1,'carol@corp','repro confirmed on staging',$2)`, wiCommented, commentAt); err != nil {
		t.Fatalf("insert comment: %v", err)
	}

	src := NewSQLSource(db)
	rows, err := src.AllForMemoryIndex(ctx, base.Add(-time.Hour), 100)
	if err != nil {
		t.Fatalf("AllForMemoryIndex: %v", err)
	}

	byID := map[string]WorkItemIndexable{}
	for _, r := range rows {
		byID[r.WorkItemID] = r
	}
	// source='discussion' is excluded (board allowlist).
	if _, ok := byID[wiDiscuss]; ok {
		t.Errorf("discussion-sourced thread-run must NOT be indexed (board allowlist)")
	}
	// The three board items are present (including the teamless one — the indexer, not the source,
	// decides teamless is unindexable; the source still surfaces it).
	for _, id := range []string{wiBoard, wiTeamless, wiCommented} {
		if _, ok := byID[id]; !ok {
			t.Errorf("board item %s missing from source rows", id)
		}
	}
	if teamless := byID[wiTeamless]; teamless.TeamID != "" {
		t.Errorf("teamless item TeamID = %q, want empty", teamless.TeamID)
	}
	// The commented item's activity reflects the comment time (later than its updated_at) and its
	// comment thread is aggregated.
	c := byID[wiCommented]
	if !c.ActivityAt.Equal(commentAt) {
		t.Errorf("commented item ActivityAt = %v, want the comment time %v", c.ActivityAt, commentAt)
	}
	if c.CommentCount != 1 || !strings.Contains(c.CommentBodies, "carol@corp: repro confirmed on staging") {
		t.Errorf("comment aggregation = count %d bodies %q — want 1 / carol's line", c.CommentCount, c.CommentBodies)
	}
	// Rows are ordered oldest-activity-first; the commented item (activity = base+10m) sorts last.
	if rows[len(rows)-1].WorkItemID != wiCommented {
		t.Errorf("ordering: last row = %s, want the most-recently-active %s", rows[len(rows)-1].WorkItemID, wiCommented)
	}
	// Watermark advance past the first item re-fetches only newer activity.
	since := byID[wiBoard].ActivityAt.Add(time.Nanosecond)
	newer, err := src.AllForMemoryIndex(ctx, since, 100)
	if err != nil {
		t.Fatalf("AllForMemoryIndex (watermark): %v", err)
	}
	for _, r := range newer {
		if r.WorkItemID == wiBoard {
			t.Errorf("watermark past wiBoard still returned it")
		}
	}
}
