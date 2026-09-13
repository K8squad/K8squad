// settle.go — the ISI-4237 terminal settle: when a Run reaches a terminal
// step, the BOARD must reflect it. Before this, the engine's terminal paths
// released the checkout and wrote their own audit rows but never touched
// coord.work_item.state, so a completed ticket sat in in_progress forever
// with no assignee, no lane, and no summary — "the ticket has no assignee …
// our crds needs updates on the progress".
//
// SettleTerminalLane is the ONE shared helper both terminal paths call inside
// their existing transaction (the machine path: ProdEffects.Terminal; the
// death-after-retry-budget path: rundrive.ProdClaims.enter → FailEnter):
//
//	UPDATE coord.work_item SET state = <lane>          — the lane move
//	  WHERE id = … AND state = 'in_progress'             (never clobber a human move)
//	INSERT INTO coord.audit_log 'state_transition'    — §6.5, lands in the
//	                                                      board statusHistory read
//	INSERT INTO coord.comment <summary line>          — the change summary the
//	                                                      ticket thread shows
//
// Lane mapping (the §13 enum has no engine-failure lane):
//   - succeeded → NO engine lane move: the ticket stays in_progress. The §6.2
//     drive contract keeps a succeeded card in_progress (rundrive D1/D5/D6) and
//     the ISI-4298 deterministic-resume pin (work_item.updated_at) forbids the
//     gratuitous touch a lane write would mint. Completion is still reflected —
//     run_terminal lands in the statusHistory, the change-summary comment on the
//     thread, and the assignee is retained — but the move to done is the
//     human's (a settled run is finished work awaiting review, not auto-closed).
//   - failed    → todo (back to the dispatchable lane: honest "not being
//     worked, needs attention". NOTE: intake currently suppresses items whose
//     workItemRef still owns a Run CR, so this is a board state, not an auto
//     re-queue — the re-dispatch gap is tracked separately.)
//   - cancelled → todo (same shape; a kill is human-initiated, the card is
//     theirs to move, but the settle keeps it out of zombie in_progress).
//
// The summary comment's author is the ENGINE principal; the agent attribution
// is read from the checkout row's assignee_agent (stamped at acquire, retained
// through release) so the line answers "who worked this" even after the
// holder is cleared.
package coord

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// SettleLaneOf maps a terminal reconcile step onto the board lane the settle
// WRITES. Only failed/cancelled map to a lane the engine moves (todo). succeeded
// maps to "" — the engine records completion but does NOT move the lane (the
// human owns the move to done; see the package doc). Non-terminal or
// unrecognized steps also return "" — the caller skips the lane move.
//
// A "" result is NOT "this step is a no-op": succeeded is terminal and still
// owes the thread a summary. settleIsTerminal is the terminal test; SettleLaneOf
// is only the lane a terminal step moves (if any).
func SettleLaneOf(step string) string {
	switch step {
	case "failed", "cancelled":
		return "todo"
	default:
		return ""
	}
}

// settleIsTerminal reports whether a reconcile step is a terminal outcome the
// settle owes the board a record for (a summary, and — for failed/cancelled — a
// lane move). A non-terminal re-entry (a retry lap's claiming_sandbox,
// dispatching, running, …) owes the board nothing.
func settleIsTerminal(step string) bool {
	switch step {
	case "succeeded", "failed", "cancelled":
		return true
	default:
		return false
	}
}

// SettleTerminalLane settles one work item's board lane for a terminal run
// step, inside tx (the caller's terminal transaction — settle is atomic with
// the terminal facts it accompanies or it does not happen). runID stamps the
// audit provenance; principal is the engine principal authoring the move and
// the summary comment.
//
// Returns the lane written ("" when the step maps to no lane or the item was
// not in in_progress) and whether the lane actually moved. A human lane move
// raced ahead of the settle is RESPECTED, never clobbered: the UPDATE only
// matches an item still in in_progress. The summary comment is written even
// when the lane did not move (the terminal fact is still thread-worthy; the
// body then reports the outcome without a lane claim).
func SettleTerminalLane(ctx context.Context, tx *sql.Tx, workItemID, runID, principal, terminalStep string) (lane string, moved bool, err error) {
	if tx == nil {
		return "", false, fmt.Errorf("coord.SettleTerminalLane: nil tx")
	}
	if workItemID == "" || principal == "" || terminalStep == "" {
		return "", false, fmt.Errorf("coord.SettleTerminalLane: workItemID, principal and terminalStep are required (got workItemID=%q principal=%q terminalStep=%q)", workItemID, principal, terminalStep)
	}

	// Only a TERMINAL step settles the board. A non-terminal re-entry (a retry
	// lap's claiming_sandbox, dispatching, running, …) owes the board NOTHING —
	// no lane move, no audit, no summary — so the helper is safe to call from
	// every re-entry path unconditionally. Return before any write.
	if !settleIsTerminal(terminalStep) {
		return "", false, nil
	}

	// Attribution for the summary line: the agent the checkout names (stamped
	// at acquire, ISI-4237; retained through the terminal release). Nullable
	// both for pre-4237 rows and for release shapes that clear it.
	var agent sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT assignee_agent FROM coord.claim WHERE work_item_id = $1::uuid`,
		workItemID).Scan(&agent); err != nil && err != sql.ErrNoRows {
		return "", false, fmt.Errorf("coord.SettleTerminalLane: read assignee: %w", err)
	}

	// (1) The lane move — ONLY for a step that maps to an engine-owned lane
	// (failed/cancelled → todo), from in_progress, never clobbering a human
	// move. succeeded maps to "" and is deliberately NOT moved: the §6.2 drive
	// contract keeps a succeeded ticket in_progress, and — critically — the
	// engine must not touch coord.work_item on success, because the ISI-4298
	// deterministic-resume pin reads work_item.updated_at and any UPDATE (even a
	// same-lane one) would mint a phantom revision. Completion is still recorded
	// by the run_terminal audit and the summary below; the move to done is the
	// human's.
	lane = SettleLaneOf(terminalStep)
	if lane != "" {
		res, err := tx.ExecContext(ctx, `
			UPDATE coord.work_item
			   SET state = $1
			 WHERE id = $2::uuid AND state = 'in_progress'`,
			lane, workItemID)
		if err != nil {
			return "", false, fmt.Errorf("coord.SettleTerminalLane: lane: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 1 {
			moved = true
			payload, merr := json.Marshal(map[string]string{"reason": "run_settled", "step": terminalStep})
			if merr != nil {
				return "", false, fmt.Errorf("coord.SettleTerminalLane: payload: %w", merr)
			}
			// (2) §6.5 state_transition — the SAME event type the board's
			// statusHistory read filters on, so the engine's lane move lands
			// in the ticket timeline exactly like a human move.
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO coord.audit_log
				       (work_item_id, run_id, event_type, principal,
				        from_state, to_state, payload)
				VALUES ($1::uuid, NULLIF($2,'')::uuid, 'state_transition', $3,
				        'in_progress', $4, $5::jsonb)`,
				workItemID, runID, principal, lane, string(payload)); err != nil {
				return "", false, fmt.Errorf("coord.SettleTerminalLane: audit: %w", err)
			}
		}
	}

	// (3) The change summary line — the one engine comment the settle owes the
	// ticket thread for EVERY terminal outcome, success included (ISI-4237 fix
	// scope). A coord.comment INSERT never touches coord.work_item, so it is
	// compatible with the succeeded no-mutation contract above. One line,
	// honest, agent-attributed when known; the outcome verb matches the step.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.comment (work_item_id, author_principal, body)
		VALUES ($1::uuid, $2, $3)`,
		workItemID, principal, settleSummary(terminalStep, lane, moved, agent.String)); err != nil {
		return "", false, fmt.Errorf("coord.SettleTerminalLane: summary: %w", err)
	}
	return lane, moved, nil
}

// settleSummary renders the one-line change summary. It names the agent when
// known and the lane the settle wrote (or the item's unchanged lane when a
// human move was respected).
func settleSummary(terminalStep, lane string, moved bool, agent string) string {
	who := ""
	if agent != "" {
		who = " — agent " + agent
	}
	outcome := map[string]string{
		"succeeded": "Run succeeded",
		"failed":    "Run failed (retry budget exhausted)",
		"cancelled": "Run cancelled",
	}[terminalStep]
	if outcome == "" {
		outcome = "Run reached terminal step " + terminalStep
	}
	switch {
	case moved:
		return fmt.Sprintf("%s%s: lane → %s (engine settle).", outcome, who, lane)
	case lane == "":
		// succeeded owns no engine lane: completion is recorded on the ticket,
		// the move to done stays with the human.
		return fmt.Sprintf("%s%s: recorded on the ticket; lane unchanged (awaiting review).", outcome, who)
	default:
		return fmt.Sprintf("%s%s: lane left as-is (moved by a human or not in progress).", outcome, who)
	}
}
