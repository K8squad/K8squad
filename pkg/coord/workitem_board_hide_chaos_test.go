//go:build chaos

/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// workitem_board_hide_chaos_test.go — the ADR-0024b §4.2 board-hide CONTRACT, on the shipped schema and
// a real Postgres (ISI-5116). It is the authoritative fail-closed guarantee the ruling asks for
// (mirroring frb3_no_chat_contract_test.go's discipline): a dispatch-on-mention thread-run item
// (source='discussion') MUST be ABSENT from every human board-list surface, even though it physically
// lives in coord.work_item so Intake can scan it. This test — not the individual WHERE clauses — is the
// backstop for a future board-list endpoint that forgets the source='board' allowlist filter.
package coord_test

import (
	"context"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/coord"
)

func TestBoardListsHideDiscussionSourcedItems(t *testing.T) {
	dsn := dsnOrFatal(t)
	ctx := context.Background()
	db := openDB(t, dsn)

	if _, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS coord CASCADE`); err != nil {
		t.Fatalf("reset coord schema: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS discussion CASCADE`); err != nil {
		t.Fatalf("reset discussion schema: %v", err)
	}
	for _, name := range []string{
		"0001_coord_schema.sql",
		"0002_coord_dispatch.sql",
		"0004_discussion_schema.sql", // 0027 creates discussion.mention_dispatch
		"0015_work_item_change_ref.sql",
		"0018_claim_assignee.sql",
		"0020_work_item_create_fields.sql",
		"0027_dispatch_on_mention_run_minting.sql",
	} {
		if _, err := db.ExecContext(ctx, migrationFile(t, name)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}

	const (
		project = "88888888-8888-8888-8888-888888888888"
		team    = "99999999-9999-9999-9999-999999999999"
	)
	// One human/board item (default source='board') and one dispatch-on-mention thread-run
	// (source='discussion'). Both are real rows in coord.work_item.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO coord.work_item (id, project_id, team_id, title, created_by, state)
		VALUES ('aaaaaaaa-0000-0000-0000-000000000001', $1::uuid, $2::uuid, 'human board ticket', 'user:henrik', 'todo')`,
		project, team); err != nil {
		t.Fatalf("seed board item: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO coord.work_item (id, project_id, team_id, title, created_by, state, source)
		VALUES ('bbbbbbbb-0000-0000-0000-000000000002', $1::uuid, $2::uuid, 'thread-run reply', 'discussion-dispatch', 'todo', 'discussion')`,
		project, team); err != nil {
		t.Fatalf("seed discussion item: %v", err)
	}

	// Sanity: the discussion row physically exists — so any absence below is the FILTER at work, not a
	// failed insert.
	var discussionRows int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM coord.work_item WHERE source = 'discussion'`).Scan(&discussionRows); err != nil {
		t.Fatalf("count discussion rows: %v", err)
	}
	if discussionRows != 1 {
		t.Fatalf("discussion-sourced row count = %d, want 1 (the item must exist so Intake can mint its Run)", discussionRows)
	}

	rd, err := coord.NewWorkItemReadStore(db)
	if err != nil {
		t.Fatalf("NewWorkItemReadStore: %v", err)
	}

	// (1) Kanban board card list (GET /api/projects/{id}/work-items) — admin scope (empty teamID) sees
	// every board card in the Project, but NEVER a discussion thread-run.
	cards, err := rd.ListWorkItems(ctx, "", project, "")
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(cards) != 1 {
		t.Fatalf("board list returned %d cards, want exactly 1 (the discussion thread-run must be hidden)", len(cards))
	}
	if cards[0].Title != "human board ticket" {
		t.Fatalf("board list surfaced %q, want only the human board ticket", cards[0].Title)
	}

	// (2) Overview status chart (GET /api/projects/{id}/overview) — the discussion item must not inflate
	// the board metrics. Sum the final snapshot: exactly one item is counted.
	from := time.Now().Add(-time.Hour)
	to := time.Now().Add(time.Hour)
	snaps, err := rd.ProjectStatusSnapshots(ctx, "", project, from, to)
	if err != nil {
		t.Fatalf("ProjectStatusSnapshots: %v", err)
	}
	if len(snaps) == 0 {
		t.Fatal("expected at least one status snapshot")
	}
	total := 0
	for _, c := range snaps[len(snaps)-1].Counts {
		total += c
	}
	if total != 1 {
		t.Fatalf("overview counted %d items in the final snapshot, want 1 (discussion thread-run excluded)", total)
	}
}
