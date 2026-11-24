// failurenotice.go — the humane failure surface for terminal-failed runs
// (ISI-7772, parent family ISI-7074, spawned from ISI-7770 deliverable 5).
//
// Before this, every failed run settled to the same hollow artifacts: the
// synthesized summary opened "Run summary (auto-generated — the agent did
// not post one)" — a lie on a failed run, the agent never had the chance —
// and asserted "Run failed (retry budget exhausted)" as a hardcoded
// assumption even when the durable evidence said otherwise. This file reads
// the evidence the settle transaction already sits beside and renders a
// notice that names who failed, the real cause class, and the honest next
// action.
package coord

import (
	"context"
	"database/sql"
	"strings"

	"github.com/google/uuid"
)

// runFailureEvidence is the durable failure evidence the settle reads for a
// terminal-failed run: the same two causes ProdSettleReader
// FailureCauseForWorkItem attributes a Run Ready condition with — the latest
// settled a2a dispatch lap, then the gone-sandbox reap reason — plus the
// phase journal ran/never-ran distinction. All fields are zero-safe: a
// missing row is evidence of absence, not an error.
type runFailureEvidence struct {
	a2aOutcome  string // latest settled lap settle_outcome: failed|follow_error; "" = none usable
	sandboxGone string // the gone-sandbox reap reason (audit_log sandbox_bind_cleared); "" = none
	ran         bool   // the phase journal shows the run reached running
	lastFrom    string // from_phase of the terminal failed edge; "" when unknown
}

// readRunFailureEvidence reads the evidence best-effort inside the settle
// transaction: a query hiccup degrades a field to its zero value — the
// failure notice must always land, exactly like the change-ref and artifact
// reads it sits beside. Non-uuid ids skip the reads (the ::uuid casts would
// otherwise 22P02 on a malformed ref).
func readRunFailureEvidence(ctx context.Context, tx *sql.Tx, workItemID, runID string) runFailureEvidence {
	var ev runFailureEvidence
	if workItemID == "" {
		return ev
	}
	if _, err := uuid.Parse(workItemID); err != nil {
		return ev
	}
	// (1) The latest SETTLED dispatch lap — the same selection
	// FailureCauseForWorkItem makes: a failed lap is the agent-side (model
	// route) terminal non-success; follow_error is a broken follow stream; a
	// succeeded lap is never a failure cause.
	_ = tx.QueryRowContext(ctx, `
		SELECT COALESCE(settle_outcome, '')
		  FROM coord.a2a_dispatch
		 WHERE work_item_id = $1::uuid AND settled_at IS NOT NULL
		 ORDER BY settled_at DESC, a2a_task_id DESC
		 LIMIT 1`, workItemID).Scan(&ev.a2aOutcome)
	if ev.a2aOutcome != SettleOutcomeFailed && ev.a2aOutcome != SettleOutcomeFollowError {
		ev.a2aOutcome = ""
		// (2) No settled lap named a cause — the infra-death source: the
		// gone-sandbox reap reason (the same event_type the Run-condition
		// attribution reads).
		_ = tx.QueryRowContext(ctx, `
			SELECT COALESCE(payload->>'reason', '')
			  FROM coord.audit_log
			 WHERE work_item_id = $1::uuid AND event_type = 'sandbox_bind_cleared'
			 ORDER BY id DESC
			 LIMIT 1`, workItemID).Scan(&ev.sandboxGone)
	}
	if runID != "" {
		if _, err := uuid.Parse(runID); err == nil {
			// (3) The phase journal: did the run ever reach running, and from
			// which phase did it fail?
			_ = tx.QueryRowContext(ctx, `
				SELECT EXISTS (SELECT 1 FROM coord.run_phase_transition
				                WHERE run_uid = $1::uuid AND to_phase = 'running'),
				       COALESCE((SELECT from_phase FROM coord.run_phase_transition
				                  WHERE run_uid = $1::uuid AND to_phase = 'failed'
				                 ORDER BY ts DESC LIMIT 1), '')`,
				runID).Scan(&ev.ran, &ev.lastFrom)
		}
	}
	return ev
}

// failureCause classifies the durable evidence into the humane cause class
// ISI-7772 names: model route retry limit / follow-stream error / sandbox
// death / died before start. Pure — unit-tested. The classes deliberately
// mirror the cause-selection FailureCauseForWorkItem already publishes, so
// the thread notice and the Run Ready condition never disagree.
func failureCause(ev runFailureEvidence) (class, short, sentence string) {
	switch {
	case ev.a2aOutcome == SettleOutcomeFailed:
		return "model_route", "model route retry limit",
			"the model route gave up — the agent-side run settled failed (its retry budget was exhausted or the provider errored terminally)"
	case ev.a2aOutcome == SettleOutcomeFollowError:
		return "follow_error", "follow stream error",
			"the live stream to the agent broke before a result arrived, so the run outcome is unknown"
	case ev.sandboxGone != "":
		return "sandbox", "sandbox death (" + ev.sandboxGone + ")",
			"the sandbox died — " + ev.sandboxGone
	case ev.ran:
		return "sandbox", "sandbox death mid-run",
			"the run died mid-flight — it had started working and its sandbox or infra died underneath it (no dispatch settlement was recorded)"
	default:
		return "before_start", "died before start",
			"the run died before it started working — it never got past startup or the queue (sandbox cold-boot death or an endpoint-gate give-up)"
	}
}

// failureNotice is the humane failure surface ISI-7772 owes a terminal-failed
// run thread: who failed, the real cause class from durable evidence, and
// the honest next action. It replaces the hollow "the agent did not post one"
// template on failed runs — the agent had no chance to post one.
type failureNotice struct {
	agent      string
	source     string
	class      string
	short      string
	sentence   string
	next       string
	ran        bool
	lastFrom   string
	a2aOutcome string
}

// newFailureNotice composes the notice from read evidence. The next action
// is source-aware: a discussion reply is single-shot (a failed reply is a
// dead end — needs attention, re-ask in the room), a ticket goes back to
// todo (needs attention; the engine does not auto re-dispatch while the run
// record owns the item).
func newFailureNotice(ev runFailureEvidence, agent, source string) *failureNotice {
	class, short, sentence := failureCause(ev)
	next := "The ticket is back on the todo lane and needs attention; no automatic re-dispatch is queued while the run record owns the item."
	if source == "discussion" {
		next = "This discussion reply is single-shot: no automatic retry is queued. Needs attention — re-ask in the room to try again."
	}
	return &failureNotice{
		agent: agent, source: source, class: class, short: short, sentence: sentence,
		next: next, ran: ev.ran, lastFrom: ev.lastFrom, a2aOutcome: ev.a2aOutcome,
	}
}

// render writes the failure notice sections. It mirrors the agent summary
// template section vocabulary (What happened / What changed / Decisions and
// why / Next steps / Evidence) so a failed run notice reads like the recap
// it replaces — but every line is evidence, not filler. Pure (no db) so the
// ISI-7094 no-progress-tokens invariant stays unit-testable for the failure
// path too.
func (f *failureNotice) render(b *strings.Builder, runID string, changes, artifacts []string) {
	b.WriteString("Run failure notice (auto-generated — the run failed before the agent could post a summary)\n\n")
	who := "The run"
	if f.agent != "" {
		who = f.agent + "'s run"
	}
	b.WriteString("What happened: " + who + " failed — " + f.sentence + ".\n\n")
	b.WriteString("What changed:\n")
	if len(changes) > 0 {
		b.WriteString(strings.Join(changes, "\n") + "\n")
	} else {
		b.WriteString("No commits or pull requests were reported for this run.\n")
	}
	b.WriteString("\nDecisions and why: Not captured — the run died before completing. See the per-run Activity feed for the step-by-step detail.\n")
	b.WriteString("\nNext steps: " + f.next + "\n")
	b.WriteString("\nEvidence: run_id=")
	if runID != "" {
		b.WriteString(runID)
	} else {
		b.WriteString("(unknown)")
	}
	b.WriteString("; terminal step=failed; cause=" + f.class)
	if f.lastFrom != "" {
		b.WriteString("; phases: " + f.lastFrom + " → failed")
	}
	if f.a2aOutcome != "" {
		b.WriteString("; a2a settle=" + f.a2aOutcome)
	}
	b.WriteString(".\n")
	if len(artifacts) > 0 {
		b.WriteString("Artifacts:\n" + strings.Join(artifacts, "\n") + "\n")
	}
}
