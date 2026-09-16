// workitemcomment_test.go — white-box unit checks for the ISI-4495
// comment-triggered re-dispatch lane policy: which lanes a human comment may
// re-enter 'todo' from. Pure logic, no Postgres — the guarded CAS + audit row
// are proven by the wired store behaviour on the deployed cluster (same
// discipline as humanstate_test.go: the SQL-level CHECKs live in the migration
// tests, the store wiring in the apiserver/taskio suites).
package coord

import "testing"

func TestCommentReTriggerLanes(t *testing.T) {
	// The six working phases re-trigger: a parked ticket (no live checkout
	// holder) with a human comment is the board asking for the next Run.
	for _, s := range []string{"design", "planning", "implementation", "code_review", "testing", "documentation"} {
		if !commentReTriggerLanes[s] {
			t.Errorf("working phase %q must be comment-re-triggerable", s)
		}
	}
	// The two engine lanes re-trigger when unheld: a succeeded run parks the
	// item in in_progress with the checkout RELEASED (settle.go), and in_review
	// is the agent's awaiting-review self-report — both are "not being worked
	// right now" the moment the claim holder is NULL. (The live-holder guard is
	// in the store's SQL, not this map — a LIVE in_progress run is never yanked.)
	for _, s := range []string{"in_progress", "in_review"} {
		if !commentReTriggerLanes[s] {
			t.Errorf("engine lane %q must be comment-re-triggerable when unheld", s)
		}
	}
	// backlog NEVER re-triggers (board decision ISI-4495: a backlog comment is
	// conversation; assignment stays the explicit dispatch verb).
	if commentReTriggerLanes["backlog"] {
		t.Error("backlog must NOT be comment-re-triggerable (ISI-4495 board decision)")
	}
	// todo is already the dispatch lane — a nudge there is a no-op by design.
	if commentReTriggerLanes["todo"] {
		t.Error("todo must NOT be comment-re-triggerable (already the dispatch lane)")
	}
	// Terminals never reopen on a comment — reopening is a deliberate human
	// lane move (kanban DnD / the detail status control).
	for _, s := range []string{"done", "cancelled"} {
		if commentReTriggerLanes[s] {
			t.Errorf("terminal %q must NOT be comment-re-triggerable", s)
		}
	}
	// Unknown states fail closed (never re-triggered, never guessed).
	for _, s := range []string{"", "blocked", "Blocked", "in-progress", "TODO"} {
		if commentReTriggerLanes[s] {
			t.Errorf("unknown state %q must fail closed (not re-triggerable)", s)
		}
	}
}
