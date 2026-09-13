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

// settle_chaos_test.go — the ISI-4237 acceptance, on the shipped schema and a
// real Postgres: a dispatched ticket MUST show (a) the agent as assignee the
// moment the checkout is acquired, (b) its board lane + statusHistory + a
// change-summary comment the moment the run settles, and (c) attribution that
// SURVIVES the terminal release. Before the fix the full loop left the board
// showing nothing but a silent in_progress + runId.
package coord_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/reconcile"
)

// TestSettleShowsAssigneeAndProgress drives the exact defect narrative:
// acquire-with-agent → machine drive to succeeded → the board read reflects
// agent attribution, the run_terminal progress in the statusHistory and the
// change-summary comment — WITHOUT seizing the terminal lane (a succeeded
// ticket stays in_progress; the move to done is the human's, and the engine
// must not touch work_item on success — ISI-4298 / rundrive D1/D5/D6).
func TestSettleShowsAssigneeAndProgress(t *testing.T) {
	dsn := dsnOrFatal(t)
	ctx := context.Background()
	db := openDB(t, dsn)

	resetProdSchema(t, db)
	// seedItem applies 0001/0002/0003/0005 (+0017 via the shared list) and
	// inserts one in_progress item claimed by run 2222…/fence 7.
	store, wi := seedItem(t, ctx, dsn)
	run := "22222222-2222-2222-2222-222222222222"

	// (1) The ISI-4237 acquire: attribution stamped in the acquire txn.
	pc, err := coord.NewProdClaimer(db, coord.DefaultProdConfig())
	if err != nil {
		t.Fatalf("NewProdClaimer: %v", err)
	}
	// Release seedItem's seeded hold so the directed acquire is legal.
	if _, err := db.ExecContext(ctx,
		`UPDATE coord.claim SET holder_principal = NULL, lease_expires_at = NULL WHERE work_item_id = $1::uuid`, wi); err != nil {
		t.Fatalf("release seed hold: %v", err)
	}
	if _, _, ok, err := pc.AcquireSpecific(ctx, "ksquad-operator", run, wi, "", "sam"); err != nil || !ok {
		t.Fatalf("AcquireSpecific(attribution): ok=%v err=%v", ok, err)
	}
	var assignee string
	if err := db.QueryRowContext(ctx,
		`SELECT assignee_agent FROM coord.claim WHERE work_item_id = $1::uuid`, wi).Scan(&assignee); err != nil {
		t.Fatalf("read assignee: %v", err)
	}
	if assignee != "sam" {
		t.Fatalf("assignee_agent = %q, want sam (ISI-4237: dispatch stamps the agent)", assignee)
	}

	// (2) Drive the machine to the terminal step — Advance commits the
	// succeeded step, and the Terminal effect settles the board.
	if !store.Advance(reconcile.StepPending, reconcile.StepSucceeded, nil) {
		t.Fatalf("Advance(pending → succeeded) did not commit")
	}
	effects, err := coord.NewProdEffects(ctx, db, wi, run, "ksquad-operator", "", nil, nil)
	if err != nil {
		t.Fatalf("NewProdEffects: %v", err)
	}
	effects.Terminal(reconcile.StepSucceeded)
	if err := effects.Err(); err != nil {
		t.Fatalf("Terminal: %v", err)
	}

	// (3) The board lane is NOT seized on success: the ticket stays in_progress
	// (the engine records completion but leaves the move to done to the human;
	// touching work_item on success would break the ISI-4298 resume pin).
	var state string
	if err := db.QueryRowContext(ctx,
		`SELECT state FROM coord.work_item WHERE id = $1::uuid`, wi).Scan(&state); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state != "in_progress" {
		t.Fatalf("board lane = %q, want in_progress (ISI-4237: success records progress, does NOT seize the done lane)", state)
	}

	// (4) The statusHistory read surfaces the engine narrative: the
	// claim_acquired lane move AND the run_terminal completion row.
	rd, err := coord.NewWorkItemReadStore(db)
	if err != nil {
		t.Fatalf("NewWorkItemReadStore: %v", err)
	}
	thread, err := rd.ReadWorkItemThread(ctx, wi, "")
	if err != nil {
		t.Fatalf("ReadWorkItemThread: %v", err)
	}
	var sawClaim, sawTerminal bool
	for _, sc := range thread.StatusHistory {
		if sc.EventType == "claim_acquired" && sc.ToState == "in_progress" {
			sawClaim = true
		}
		if sc.EventType == "run_terminal" && sc.ToState == "succeeded" {
			sawTerminal = true
		}
	}
	if !sawClaim || !sawTerminal {
		t.Fatalf("statusHistory missing engine progress: claim=%v terminal=%v (history=%+v)", sawClaim, sawTerminal, thread.StatusHistory)
	}

	// (5) The change summary comment landed, agent-attributed, reporting the
	// success without claiming a lane move.
	var sawSummary bool
	for _, c := range thread.Comments {
		if c.Author == "ksquad-operator" && contains(c.Body, "agent sam") && contains(c.Body, "Run succeeded") {
			sawSummary = true
		}
	}
	if !sawSummary {
		t.Fatalf("no agent-attributed change summary comment on the thread (comments=%+v)", thread.Comments)
	}

	// (6) Attribution SURVIVES the terminal release — the holder is gone, the
	// agent stays, so the settled card still answers "who worked this".
	var holder, after string
	if err := db.QueryRowContext(ctx,
		`SELECT holder_principal, assignee_agent FROM coord.claim WHERE work_item_id = $1::uuid`, wi).
		Scan(&holder, &after); err != nil {
		t.Fatalf("read claim after settle: %v", err)
	}
	if holder != "" || after != "sam" {
		t.Fatalf("post-settle claim: holder=%q assignee=%q, want released holder with retained attribution", holder, after)
	}

	// (7) The board card list carries the assignee (the console's WorkItem
	// field the Kanban card renders).
	items, err := rd.ListWorkItems(ctx, "", mustProjectOf(t, db, wi))
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	found := false
	for _, it := range items {
		if it.ID == wi {
			found = true
			if it.Assignee != "sam" || it.State != "in_progress" {
				t.Fatalf("board card = %+v, want assignee sam in lane in_progress", it)
			}
		}
	}
	if !found {
		t.Fatalf("settled item %s missing from its board list", wi)
	}
}

// TestSettleFailureReturnsTicketToTodo pins the failure mapping: FailEnter's
// terminal re-entry writes todo (dispatchable again), the state_transition
// audit and the summary — via the same shared SettleTerminalLane the machine
// path uses.
func TestSettleFailureReturnsTicketToTodo(t *testing.T) {
	dsn := dsnOrFatal(t)
	ctx := context.Background()
	db := openDB(t, dsn)

	resetProdSchema(t, db)
	_, wi := seedItem(t, ctx, dsn)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	lane, moved, err := coord.SettleTerminalLane(ctx, tx, wi, "33333333-3333-3333-3333-333333333333", "ksquad-operator", "failed")
	if err != nil {
		t.Fatalf("SettleTerminalLane(failed): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if lane != "todo" || !moved {
		t.Fatalf("settle = (%q, %v), want (todo, true)", lane, moved)
	}

	var state string
	if err := db.QueryRowContext(ctx,
		`SELECT state FROM coord.work_item WHERE id = $1::uuid`, wi).Scan(&state); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state != "todo" {
		t.Fatalf("board lane = %q, want todo after a failed settle", state)
	}
}

// TestSettleRespectsHumanLaneMove: a lane the human already moved is never
// clobbered — the settle reports, it does not overwrite.
func TestSettleRespectsHumanLaneMove(t *testing.T) {
	dsn := dsnOrFatal(t)
	ctx := context.Background()
	db := openDB(t, dsn)

	resetProdSchema(t, db)
	_, wi := seedItem(t, ctx, dsn)
	// A human moved the card to in_review while the run was in flight.
	if _, err := db.ExecContext(ctx,
		`UPDATE coord.work_item SET state = 'in_review' WHERE id = $1::uuid`, wi); err != nil {
		t.Fatalf("human lane move: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// A failed settle would move in_progress → todo, but the human already
	// moved the card to in_review — the guard (WHERE state = 'in_progress')
	// must leave it untouched.
	lane, moved, err := coord.SettleTerminalLane(ctx, tx, wi, "", "ksquad-operator", "failed")
	if err != nil {
		t.Fatalf("SettleTerminalLane(failed over in_review): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if moved {
		t.Fatalf("settle moved the lane to %q — a human in_review move must be respected", lane)
	}
	var state string
	if err := db.QueryRowContext(ctx,
		`SELECT state FROM coord.work_item WHERE id = $1::uuid`, wi).Scan(&state); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state != "in_review" {
		t.Fatalf("board lane = %q, want in_review (the human's move)", state)
	}
}

// TestSettleNoOpForRetry: a retry re-entry (claiming_sandbox) owes the board
// nothing — no lane move, no audit, no comment.
func TestSettleNoOpForRetry(t *testing.T) {
	dsn := dsnOrFatal(t)
	ctx := context.Background()
	db := openDB(t, dsn)

	resetProdSchema(t, db)
	_, wi := seedItem(t, ctx, dsn)

	var comments, transitions int
	if err := db.QueryRowContext(ctx,
		`SELECT (SELECT count(*) FROM coord.comment WHERE work_item_id = $1::uuid),
		        (SELECT count(*) FROM coord.audit_log WHERE work_item_id = $1::uuid AND event_type = 'state_transition')`,
		wi).Scan(&comments, &transitions); err != nil {
		t.Fatalf("pre-counts: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	lane, moved, err := coord.SettleTerminalLane(ctx, tx, wi, "", "ksquad-operator", "claiming_sandbox")
	if err != nil {
		t.Fatalf("SettleTerminalLane(retry): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if lane != "" || moved {
		t.Fatalf("retry settle = (%q, %v), want a no-op", lane, moved)
	}

	var commentsAfter, transitionsAfter int
	if err := db.QueryRowContext(ctx,
		`SELECT (SELECT count(*) FROM coord.comment WHERE work_item_id = $1::uuid),
		        (SELECT count(*) FROM coord.audit_log WHERE work_item_id = $1::uuid AND event_type = 'state_transition')`,
		wi).Scan(&commentsAfter, &transitionsAfter); err != nil {
		t.Fatalf("post-counts: %v", err)
	}
	if commentsAfter != comments || transitionsAfter != transitions {
		t.Fatalf("retry settle wrote board rows: comments %d→%d, transitions %d→%d",
			comments, commentsAfter, transitions, transitionsAfter)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func mustProjectOf(t *testing.T, db *sql.DB, wi string) string {
	t.Helper()
	var p string
	if err := db.QueryRowContext(context.Background(),
		`SELECT project_id::text FROM coord.work_item WHERE id = $1::uuid`, wi).Scan(&p); err != nil {
		t.Fatalf("read project of %s: %v", wi, err)
	}
	return p
}
