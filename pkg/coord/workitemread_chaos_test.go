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

// workitemread_chaos_test.go — the ISI-4536 acceptance, on the shipped schema
// and a real Postgres: ListWorkItems with a parentID must return ONLY that
// item's DIRECT children (never the whole-Project card list the bug rendered
// as "Sub-tickets 28 of 85 done"), and with an empty parentID must keep
// returning the full card set the board/kanban views draw from.
package coord_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/K8squad/K8squad/pkg/coord"
)

// seedParentTree inserts into one Project: a parent with two direct children
// (one done — the progress-bar numerator), a GRANDCHILD hanging off child-a
// (must NOT match ?parentId=parent), and two unrelated roots. Returns the
// parent's id and a title→id map for assertions.
func seedParentTree(t *testing.T, ctx context.Context, db *sql.DB) (string, map[string]string) {
	t.Helper()
	const project = "77777777-7777-7777-7777-777777777777"
	rows := []struct {
		title  string
		parent string // title of the parent row, "" for roots
		state  string
	}{
		{"parent", "", "in_progress"},
		{"child-a", "parent", "done"},
		{"child-b", "parent", "todo"},
		{"grandchild", "child-a", "todo"}, // direct child of child-a, NOT of parent
		{"unrelated-1", "", "backlog"},
		{"unrelated-2", "", "done"},
	}
	ids := map[string]string{}
	var parent string
	for _, r := range rows {
		var pid any // sql.Null-ish: nil = root
		if r.parent != "" {
			pid = ids[r.parent]
		}
		var id string
		if err := db.QueryRowContext(ctx, `
			INSERT INTO coord.work_item (project_id, parent_id, title, state, created_by)
			VALUES ($1::uuid, $2::uuid, $3, $4, 'principal:test')
			RETURNING id::text`, project, pid, r.title, r.state).Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", r.title, err)
		}
		ids[r.title] = id
		if r.title == "parent" {
			parent = id
		}
	}
	return parent, ids
}

func TestListWorkItemsParentFilter(t *testing.T) {
	dsn := dsnOrFatal(t)
	ctx := context.Background()
	db := openDB(t, dsn)

	// The shipped read surface: work_item + claim (0002 provisions a claim row
	// per insert) + change_ref (0015) + claim.assignee_agent (0018).
	if _, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS coord CASCADE`); err != nil {
		t.Fatalf("reset coord schema: %v", err)
	}
	for _, name := range []string{
		"0001_coord_schema.sql",
		"0002_coord_dispatch.sql",
		"0015_work_item_change_ref.sql",
		"0018_claim_assignee.sql",
	} {
		if _, err := db.ExecContext(ctx, migrationFile(t, name)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	parent, ids := seedParentTree(t, ctx, db)
	const project = "77777777-7777-7777-7777-777777777777"

	rd, err := coord.NewWorkItemReadStore(db)
	if err != nil {
		t.Fatalf("NewWorkItemReadStore: %v", err)
	}

	// (1) ?parentId=parent ⇒ exactly the two DIRECT children, nothing else:
	// the grandchild and the unrelated roots must not leak into the
	// sub-ticket card, and the parent itself must not list itself.
	kids, err := rd.ListWorkItems(ctx, "", project, parent)
	if err != nil {
		t.Fatalf("ListWorkItems(parent): %v", err)
	}
	got := map[string]string{}
	for _, it := range kids {
		got[it.ID] = it.State
	}
	if len(kids) != 2 {
		t.Fatalf("parent filter returned %d cards (%v), want exactly the 2 direct children", len(kids), got)
	}
	if got[ids["child-a"]] != "done" || got[ids["child-b"]] != "todo" {
		t.Fatalf("direct children wrong: %v", got)
	}
	for _, leak := range []string{"parent", "grandchild", "unrelated-1", "unrelated-2"} {
		if _, ok := got[ids[leak]]; ok {
			t.Fatalf("non-direct-child %q leaked into the ?parentId= list", leak)
		}
	}

	// (2) No param ⇒ the full-Project card set, unchanged (board/kanban AC).
	all, err := rd.ListWorkItems(ctx, "", project, "")
	if err != nil {
		t.Fatalf("ListWorkItems(all): %v", err)
	}
	if len(all) != 6 {
		t.Fatalf("unfiltered list returned %d cards, want all 6 (board/kanban behavior must not change)", len(all))
	}
}
