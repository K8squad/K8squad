//go:build search_integration

// Board-hide search CONTRACT (ADR-0024b §4.2, ISI-5116): the global work-item search (GET /api/search)
// is a human board surface, so a dispatch-on-mention thread-run (coord.work_item.source='discussion')
// must NOT surface to a searcher — not even an admin AllTeams query — exactly as it is hidden from the
// Kanban list. Runs on the same real-Postgres gate as TestSearchIntegration.
//
//	go test -tags=search_integration ./pkg/search/ -run TestSearchBoardHide
//
// DATABASE_URL unset ⇒ SKIP.

package search

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSearchBoardHidesDiscussionSourcedItems(t *testing.T) {
	db := openSearchTestDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS coord CASCADE`); err != nil {
		t.Fatalf("reset coord schema: %v", err)
	}
	// 0001 (work_item) + 0012 (search_tsv/GIN) + 0027 (source column) — the shipped files, so drift
	// between a migration and the searcher's source filter goes RED here.
	for _, name := range []string{"0001_coord_schema.sql", "0012_work_item_search.sql", "0027_dispatch_on_mention_run_minting.sql"} {
		var mig []byte
		var err error
		for _, c := range []string{
			filepath.Join("..", "..", "db", "migrations", name),
			filepath.Join("db", "migrations", name),
		} {
			if mig, err = os.ReadFile(c); err == nil {
				break
			}
		}
		if mig == nil {
			t.Fatalf("could not read shipped migration %s", name)
		}
		// 0027 also creates discussion.mention_dispatch; ensure the schema exists for a coord-only reset.
		if _, err := db.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS discussion`); err != nil {
			t.Fatalf("ensure discussion schema: %v", err)
		}
		if _, err := db.ExecContext(ctx, string(mig)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}

	s, err := NewPostgresSearcher(db)
	if err != nil {
		t.Fatal(err)
	}

	const team = "aaaaaaaa-0000-0000-0000-000000000001"
	const proj = "aaaaaaaa-1111-0000-0000-000000000001"

	// A board item and a discussion thread-run that BOTH match the query "checkout".
	if _, err := db.ExecContext(ctx, `
		INSERT INTO coord.work_item (project_id, team_id, title, body, state, created_by)
		VALUES ($1::uuid, $2::uuid, 'Fix checkout latency', 'board body', 'todo', 'user:henrik')`,
		proj, team); err != nil {
		t.Fatalf("seed board item: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO coord.work_item (project_id, team_id, title, body, state, created_by, source)
		VALUES ($1::uuid, $2::uuid, 'Reply about checkout in room', 'thread-run body', 'todo', 'discussion-dispatch', 'discussion')`,
		proj, team); err != nil {
		t.Fatalf("seed discussion item: %v", err)
	}

	// Admin AllTeams query — the widest board surface. It must still hide the discussion thread-run.
	got, err := s.Search(ctx, Query{Text: "checkout", AllTeams: true, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("admin search returned %d hits, want exactly 1 (the discussion thread-run must be hidden): %+v", len(got), got)
	}
	if got[0].Title != "Fix checkout latency" {
		t.Fatalf("search surfaced %q, want only the board item", got[0].Title)
	}
}
