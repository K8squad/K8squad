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

// intake_singleshot_chaos_test.go — ISI-5137 / ADR-0024c §4: thread-runs are
// single-shot. On the shipped schema (all migrations, incl. 0027 source column)
// and a live Postgres, this proves the intake seam refuses to re-dispatch a
// settled/failed source='discussion' item while a source='board' item still
// re-arms (no regression to the ISI-4556 generation re-arm). Gate wiring is the
// same as chaos_test.go: DATABASE_URL → a live Postgres, -tags=chaos.
package rundrive_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/K8squad/K8squad/pkg/controller/rundrive"
)

// seedSettledItem inserts a root work item on the 'todo' lane with the given
// source and a team, then drives its auto-provisioned claim to a settled
// terminal step (reconcile_step='failed', checkout released) — the exact shape
// SettleTerminalLane leaves after a failed run, and the shape the ISI-4556
// re-arm acts on. Returns the item uuid.
func seedSettledItem(t *testing.T, db *sql.DB, source string) string {
	t.Helper()
	ctx := context.Background()
	var item string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO coord.work_item (project_id, team_id, title, created_by, state, source)
		VALUES (gen_random_uuid(), gen_random_uuid(), $1, 'principal:chaos', 'todo', $2)
		RETURNING id::text`, "single-shot "+source+" item", source).Scan(&item); err != nil {
		t.Fatalf("seed %s item: %v", source, err)
	}
	// Settle the auto-provisioned claim: terminal step, checkout released — a
	// previous generation's failed run.
	if _, err := db.ExecContext(ctx, `
		UPDATE coord.claim
		   SET reconcile_step = 'failed', holder_principal = NULL
		 WHERE work_item_id = $1::uuid`, item); err != nil {
		t.Fatalf("settle %s claim: %v", source, err)
	}
	return item
}

func stepOfItem(t *testing.T, db *sql.DB, item string) string {
	t.Helper()
	var step string
	if err := db.QueryRowContext(context.Background(),
		`SELECT reconcile_step FROM coord.claim WHERE work_item_id = $1::uuid`, item).
		Scan(&step); err != nil {
		t.Fatalf("read step: %v", err)
	}
	return step
}

func contains(items []rundrive.IntakeItem, id string) bool {
	for _, it := range items {
		if it.ID == id {
			return true
		}
	}
	return false
}

// TestIntakeSingleShotDiscussion is the ISI-5137 acceptance: a settled/failed
// source='discussion' item is neither re-selected by DueWorkItems nor re-armed
// by RearmSettled, while a source='board' item in the identical settled shape
// is re-selected AND re-armed (the ISI-4556 behavior is intact).
func TestIntakeSingleShotDiscussion(t *testing.T) {
	ctx := context.Background()
	db := isolatedGateDB(t, dsnOrFatal(t), "singleshot")
	applyMigrations(t, db)

	src, err := rundrive.NewSQLIntakeSource(db)
	if err != nil {
		t.Fatalf("NewSQLIntakeSource: %v", err)
	}

	discussion := seedSettledItem(t, db, "discussion")
	board := seedSettledItem(t, db, "board")

	// (1) DueWorkItems: the settled board item is re-selected; the settled
	// discussion item (terminal claim) is single-shot and NOT re-selected.
	items, err := src.DueWorkItems(ctx, 32)
	if err != nil {
		t.Fatalf("DueWorkItems: %v", err)
	}
	if !contains(items, board) {
		t.Fatalf("settled source='board' item %s must still be re-selected (ISI-4556); got %+v", board, items)
	}
	if contains(items, discussion) {
		t.Fatalf("settled source='discussion' item %s must NOT be re-selected (single-shot, ISI-5137); got %+v", discussion, items)
	}

	// (2) RearmSettled: the board item re-arms to 'pending' (a fresh generation
	// can drive it); the discussion item is left terminal ('failed'), never
	// re-armed — a failed reply attempt is a dead end, not a paid loop.
	if err := src.RearmSettled(ctx, board); err != nil {
		t.Fatalf("RearmSettled(board): %v", err)
	}
	if got := stepOfItem(t, db, board); got != "pending" {
		t.Fatalf("board claim after re-arm: got %q want pending (ISI-4556 re-arm)", got)
	}

	if err := src.RearmSettled(ctx, discussion); err != nil {
		t.Fatalf("RearmSettled(discussion): %v", err)
	}
	if got := stepOfItem(t, db, discussion); got != "failed" {
		t.Fatalf("discussion claim after re-arm: got %q want failed (single-shot: never re-armed)", got)
	}
}
