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
//     re-queue — the re-dispatch gap is tracked separately.) EXCEPTION
//     (ISI-5137/ADR-0024c §4): a source='discussion' thread-run is single-shot,
//     so its failed run maps to `cancelled` (terminal), not `todo` — a failed
//     reply attempt must be a dead end, never an unbounded PAID re-dispatch.
//   - cancelled → cancelled (TERMINAL, ISI-4489/Q3): a kill is human-initiated
//     and the ticket is done for good — it lands on the Cancelled terminal lane,
//     no longer bouncing back to todo. `done↔cancelled` remains the one forbidden
//     human edge (humanstate.go phaseTransitionAllowed); the engine reaches
//     cancelled from the in_progress working lane, never across the terminal
//     boundary.
//
// The lane-move guard stays `WHERE state='in_progress'` (never clobber a human
// move). Relaxing it to admit the ISI-4431 phase lanes belongs with the
// coordinator drive loop (E5 core), which is the code that first parks tickets on
// those lanes — landing the relaxation here, before any phase-lane ticket exists,
// would ship guard behavior nothing exercises. See arch §3.4.
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
	"strings"
)

// SettleLaneOf maps a terminal reconcile step onto the board lane the settle
// WRITES. failed maps to `todo` (dispatchable again) and cancelled maps to the
// `cancelled` terminal lane (ISI-4489/Q3); succeeded maps to "" — the engine
// records completion but does NOT move the lane (the human owns the move to done;
// see the package doc). Non-terminal or unrecognized steps also return "" — the
// caller skips the lane move.
//
// A "" result is NOT "this step is a no-op": succeeded is terminal and still
// owes the thread a summary. settleIsTerminal is the terminal test; SettleLaneOf
// is only the lane a terminal step moves (if any).
//
// ISI-4489/Q3: cancelled now maps to the `cancelled` TERMINAL lane (a kill is
// permanent), not back to `todo`. failed keeps mapping to `todo` (the safe v1
// default — the ticket is dispatchable again; coordinator-team rework is layered
// on top of, not inside, settle — arch §3.4).
func SettleLaneOf(step string) string {
	switch step {
	case "failed":
		return "todo"
	case "cancelled":
		return "cancelled"
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
	// (failed → todo, cancelled → cancelled), from in_progress, never clobbering
	// a human move. succeeded maps to "" and is deliberately NOT moved: the §6.2
	// drive contract keeps a succeeded ticket in_progress, and — critically — the
	// engine must not touch coord.work_item on success, because the ISI-4298
	// deterministic-resume pin reads work_item.updated_at and any UPDATE (even a
	// same-lane one) would mint a phantom revision. Completion is still recorded
	// by the run_terminal audit and the summary below; the move to done is the
	// human's.
	lane = SettleLaneOf(terminalStep)

	// ISI-5137 / ADR-0024c §4 — a source='discussion' thread-run is single-shot.
	// SettleLaneOf maps a failed run back to 'todo' (the ISI-4556 re-dispatchable
	// lane); for a board-hidden discussion item that lane re-arms an unbounded
	// PAID re-dispatch loop for a reply attempt that already failed. On the FIRST
	// terminal run of a discussion item the item goes to the 'cancelled' terminal
	// lane instead — a failed reply is a dead end, not a paid loop. Only the
	// failed→'todo' mapping needs the override: 'cancelled' is already terminal
	// and succeeded owns no engine lane (it stays in_progress, ISI-4298 pin —
	// and carries a terminal claim, so intake's single-shot scan never re-selects
	// it either). Gated on the coord.work_item.source column (migration 0027).
	if lane == "todo" {
		var src sql.NullString
		switch err := tx.QueryRowContext(ctx,
			`SELECT source FROM coord.work_item WHERE id = $1::uuid`, workItemID).Scan(&src); err {
		case nil, sql.ErrNoRows:
			// proceed; a missing row leaves the lane and the WHERE-guarded UPDATE
			// below no-ops it anyway.
		default:
			return "", false, fmt.Errorf("coord.SettleTerminalLane: read source: %w", err)
		}
		if src.String == "discussion" {
			lane = "cancelled"
		}
	}

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

	// (3) The per-run SUMMARY — the ISI-7094 two-phase invariant. A completed run
	// owes the thread EXACTLY ONE kind='summary' comment. The agent authors a rich
	// structured recap as its final act (taskio post-comment kind=summary); if it
	// omitted one, the PLATFORM synthesizes a minimal summary from the run record
	// HERE, in the terminal txn, so the invariant holds structurally and never on
	// agent goodwill (the ISI-7088 key decision). When the agent already
	// summarised, the engine's terminal fact still belongs on the thread — but as a
	// PROGRESS row (it joins the per-run Activity feed), never a second summary. A
	// coord.comment INSERT never touches coord.work_item, so both branches stay
	// compatible with the succeeded no-mutation contract above.
	runParam := sql.NullString{}
	if runID != "" {
		runParam = sql.NullString{String: runID, Valid: true}
	}
	hasSummary := false
	if runID != "" {
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM coord.comment
				 WHERE work_item_id = $1::uuid AND run_id = $2::uuid AND kind = 'summary')`,
			workItemID, runID).Scan(&hasSummary); err != nil {
			return "", false, fmt.Errorf("coord.SettleTerminalLane: summary presence: %w", err)
		}
	}

	if hasSummary {
		// Agent already summarised — the engine terminal line lands as a progress
		// row (the one-line outcome belongs in the Activity feed, not as a second
		// top-level summary).
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO coord.comment (work_item_id, author_principal, body, kind, run_id)
			VALUES ($1::uuid, $2, $3, 'progress', $4::uuid)`,
			workItemID, principal, settleSummary(terminalStep, lane, moved, agent.String), runParam); err != nil {
			return "", false, fmt.Errorf("coord.SettleTerminalLane: terminal progress: %w", err)
		}
		return lane, moved, nil
	}

	// Agent omitted a summary — the platform synthesizes the required one from the
	// durable run record (change refs, artifacts, outcome), carrying NO progress
	// tokens so the recap reads human-first.
	body, err := synthesizeSummary(ctx, tx, workItemID, runID, terminalStep, lane, moved, agent.String)
	if err != nil {
		return "", false, fmt.Errorf("coord.SettleTerminalLane: synthesize summary: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.comment (work_item_id, author_principal, body, kind, run_id)
		VALUES ($1::uuid, $2, $3, 'summary', $4::uuid)`,
		workItemID, principal, body, runParam); err != nil {
		return "", false, fmt.Errorf("coord.SettleTerminalLane: summary: %w", err)
	}
	return lane, moved, nil
}

// synthesizeSummary builds the platform's minimal structured recap for a run that
// reached a terminal step WITHOUT the agent authoring its own summary (ISI-7094 /
// the ISI-7088 "platform always synthesizes" invariant). It reads the durable run
// record — the change refs (commits / PRs) and artifacts the run reported — and
// composes the five-part template the agent template mirrors: What I did / What
// changed / Decisions / Next steps / Evidence. It deliberately carries NO
// [run/[tool/[status progress tokens (a summary is the human-readable recap; the
// step-by-step detail lives in the Activity feed). The record reads are
// best-effort: a query hiccup degrades a section to its honest-empty wording
// rather than failing the settle (the summary comment must always land).
func synthesizeSummary(ctx context.Context, tx *sql.Tx, workItemID, runID, terminalStep, lane string, moved bool, agent string) (string, error) {
	runParam := sql.NullString{}
	if runID != "" {
		runParam = sql.NullString{String: runID, Valid: true}
	}

	outcome := map[string]string{
		"succeeded": "Run succeeded",
		"failed":    "Run failed (retry budget exhausted)",
		"cancelled": "Run was cancelled",
	}[terminalStep]
	if outcome == "" {
		outcome = "Run reached terminal step " + terminalStep
	}
	who := ""
	if agent != "" {
		who = " by agent " + agent
	}

	// What changed: the run's reported commits / PRs, bounded.
	var changes []string
	if rows, err := tx.QueryContext(ctx, `
		SELECT kind, ref, COALESCE(summary, '')
		  FROM coord.change_ref
		 WHERE work_item_id = $1::uuid
		   AND ($2::uuid IS NULL OR run_id = $2::uuid)
		 ORDER BY created_at ASC
		 LIMIT 20`, workItemID, runParam); err == nil {
		for rows.Next() {
			var kind, ref, summary string
			if err := rows.Scan(&kind, &ref, &summary); err != nil {
				break
			}
			line := "- " + kind + " " + ref
			if summary != "" {
				line += " — " + summary
			}
			changes = append(changes, line)
		}
		_ = rows.Close()
	}

	// Evidence: the run's reported artifacts, bounded.
	var artifacts []string
	if rows, err := tx.QueryContext(ctx, `
		SELECT kind, uri
		  FROM coord.artifact
		 WHERE work_item_id = $1::uuid
		   AND ($2::uuid IS NULL OR run_id = $2::uuid)
		 ORDER BY created_at ASC
		 LIMIT 20`, workItemID, runParam); err == nil {
		for rows.Next() {
			var kind, uri string
			if err := rows.Scan(&kind, &uri); err != nil {
				break
			}
			artifacts = append(artifacts, "- "+kind+": "+uri)
		}
		_ = rows.Close()
	}

	return renderSynthesizedSummary(outcome, who, terminalStep, runID, changes, artifacts), nil
}

// renderSynthesizedSummary composes the five-part platform summary from already-read
// pieces. Kept pure (no db) so the template — and the ISI-7094 invariant that a
// summary carries NO [run/[tool/[status progress tokens — is unit-testable.
func renderSynthesizedSummary(outcome, who, terminalStep, runID string, changes, artifacts []string) string {
	next := map[string]string{
		"succeeded": "Awaiting human review; move the ticket to done when the work is accepted.",
		"failed":    "The ticket is back on the dispatchable lane for another attempt or human attention.",
		"cancelled": "The run was stopped; no further automated work is queued.",
	}[terminalStep]
	if next == "" {
		next = "See the ticket lane for the current state."
	}

	var b strings.Builder
	b.WriteString("Run summary (auto-generated — the agent did not post one)\n\n")
	b.WriteString("What I did: " + outcome + who + ".\n\n")
	b.WriteString("What changed:\n")
	if len(changes) > 0 {
		b.WriteString(strings.Join(changes, "\n") + "\n")
	} else {
		b.WriteString("No commits or pull requests were reported for this run.\n")
	}
	b.WriteString("\nDecisions and why: Not captured by the agent. See the per-run Activity feed for the step-by-step detail.\n")
	b.WriteString("\nNext steps: " + next + "\n")
	b.WriteString("\nEvidence: run_id=")
	if runID != "" {
		b.WriteString(runID)
	} else {
		b.WriteString("(unknown)")
	}
	b.WriteString("; terminal step=" + terminalStep + ".\n")
	if len(artifacts) > 0 {
		b.WriteString("Artifacts:\n" + strings.Join(artifacts, "\n") + "\n")
	}
	return b.String()
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
		return fmt.Sprintf("%s%s: lane left as-is (human move respected, or already terminal/parked).", outcome, who)
	}
}
