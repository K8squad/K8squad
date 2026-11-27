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

// upwardhandoff.go — the WS-B upward hand-off spine (ISI-7956, plan rev 3).
// On the run-completion seam the platform PUSHES a typed hand-off that wakes
// the finishing agent's manager: a small brief, the at-most-once marker on
// coord.upward_handoff (migration 0047), a §6.5 audit row, and — the wake
// itself — a child coord.work_item in state 'todo' with requested_agent set
// to the manager, so the ordinary intake sweep dispatches a Run to the
// manager. Push, not poll: the manager's next act is a dispatched run, never
// a poll of its children.
//
// The manager is resolved CONTROLLER-side (pkg/controller/rundrive/upward.go)
// from the WS-A org-chart edge (Role label ksquad.io/reports-to, mirrored
// from spec.reportsTo by the mrole-orgchart webhook) and passed in; pkg/coord
// stays DB-only. An empty manager — the finishing agent is the team
// root/CEO, or unresolvable — is a silent no-op: WS-E owns the CEO-to-human
// termination later, and this spine never fabricates a recipient.
//
// The brief reuses HandoffDoc (prodhandoff.go) — the doc TYPE — but NOT the
// guarded writer: WriteHandoff is custody-gated to the LIVE claim holder, and
// the terminal seam runs post-release (ProdEffects.Terminal releases the
// claim in the same transaction, before the settle), so the spine writes its
// own at-most-once marker instead of the custody predicate.
//
// At-most-once (the WS-B guardrail, mirroring a2asettle.go's marker
// discipline): INSERT ... ON CONFLICT (work_item_id, run_id, kind) DO NOTHING
// + RETURNING — a re-entry (re-drive, duplicate settle, second terminal
// path) returns no row, writes no marker, no audit and no wake item, and is
// a silent nil. failed/cancelled terminals never reach the push at all: the
// settle already routes those (failed → todo re-dispatch, cancelled →
// terminal lane); the upward push is for completions.
package coord

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// The closed set of upward hand-off kinds (migration 0047's CHECK admits
// exactly these). 'decision' is reserved for the WS-C mid-run decision
// request path that will reuse this spine; the run-completion seam emits
// done/ticket/pr today.
const (
	upwardKindDone     = "done"
	upwardKindTicket   = "ticket"
	upwardKindPR       = "pr"
	upwardKindDecision = "decision"
)

// PushUpwardHandoff writes the WS-B upward hand-off for one terminal settle,
// inside tx (the caller's terminal transaction — the push is atomic with the
// settle it accompanies or it does not happen). manager is the resolved
// manager AGENT name (controller-side, one ReportsTo level up); terminalStep
// is the settle's terminal step.
//
// Silent no-ops (return nil, nothing written):
//   - manager == ""               — root/CEO or unresolvable: WS-E owns the
//     CEO-to-human path, nothing is fabricated;
//   - terminalStep != succeeded   — failed/cancelled route via the settle's
//     own lanes; upward is for completions;
//   - runID == ""                 — a runless settle has nothing to key the
//     at-most-once marker on;
//   - no assignee on the checkout — no finishing agent to hand off from;
//   - the settling item is gone   — nowhere to hang the wake item;
//   - ON CONFLICT returns no row  — the (item, run, kind) marker already
//     exists: at-most-once, nothing
//     double-fires.
//
// On the first writer the SAME transaction commits (a) the marker row, (b)
// the §6.5 'upward_handoff_pushed' audit row, and (c) the manager's wake work
// item (state 'todo', requested_agent = manager, project/team copied from
// the settling item, parent linked to it) — intake's next sweep dispatches a
// Run to the manager, which IS the wake.
func PushUpwardHandoff(ctx context.Context, tx *sql.Tx, workItemID, runID, principal, terminalStep, manager string) error {
	// The no-op gates come first: an unarmed spine must be free to call.
	if manager == "" {
		return nil
	}
	if terminalStep != "succeeded" {
		return nil
	}
	if runID == "" {
		return nil
	}
	if tx == nil {
		return fmt.Errorf("coord.PushUpwardHandoff: nil tx")
	}
	if workItemID == "" || principal == "" {
		return fmt.Errorf("coord.PushUpwardHandoff: workItemID and principal are required (got workItemID=%q principal=%q)", workItemID, principal)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// The finishing agent: the checkout's assignee (stamped at acquire,
	// retained through the terminal release — the same read the settle's
	// summary attribution uses). No assignee: nothing to hand off FROM.
	var agent sql.NullString
	switch err := tx.QueryRowContext(ctx,
		"SELECT assignee_agent FROM coord.claim WHERE work_item_id = $1::uuid",
		workItemID).Scan(&agent); {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("coord.PushUpwardHandoff: read assignee: %w", err)
	}
	if !agent.Valid || agent.String == "" {
		return nil
	}

	// The settling item's own facts — copied onto the wake item (project,
	// team) and echoed into its title (parent title).
	var projectID, teamID, title sql.NullString
	switch err := tx.QueryRowContext(ctx,
		"SELECT project_id::text, team_id::text, title FROM coord.work_item WHERE id = $1::uuid",
		workItemID).Scan(&projectID, &teamID, &title); {
	case errors.Is(err, sql.ErrNoRows):
		return nil // the settling item is gone: nowhere to hang the wake
	case err != nil:
		return fmt.Errorf("coord.PushUpwardHandoff: read work item: %w", err)
	}

	// The run's reported changes (the same bounded change_ref read
	// synthesizeSummary performs): they carry BOTH the hand-off kind and the
	// "what changed" lines of the brief.
	var changes []string
	var changeKinds []string
	if rows, err := tx.QueryContext(ctx, `
		SELECT kind, ref, COALESCE(summary, '')
		  FROM coord.change_ref
		 WHERE work_item_id = $1::uuid AND run_id = $2::uuid
		 ORDER BY created_at ASC
		 LIMIT 20`,
		workItemID, runID); err == nil {
		for rows.Next() {
			var k, ref, summary string
			if err := rows.Scan(&k, &ref, &summary); err != nil {
				break
			}
			changeKinds = append(changeKinds, k)
			line := "- " + k + " " + ref
			if summary != "" {
				line += " — " + summary
			}
			changes = append(changes, line)
		}
		_ = rows.Close()
	}

	// The run's registered artifacts (bounded): the brief's
	// artifacts_for_downstream block.
	var artifactRefs []ArtifactRef
	var artifactLines []string
	if rows, err := tx.QueryContext(ctx, `
		SELECT kind, uri, COALESCE(sha256, '')
		  FROM coord.artifact
		 WHERE work_item_id = $1::uuid AND run_id = $2::uuid
		 ORDER BY created_at ASC
		 LIMIT 20`,
		workItemID, runID); err == nil {
		for rows.Next() {
			var k, uri, sum string
			if err := rows.Scan(&k, &uri, &sum); err != nil {
				break
			}
			artifactRefs = append(artifactRefs, ArtifactRef{Kind: k, URI: uri, SHA256: sum})
			artifactLines = append(artifactLines, "- "+k+": "+uri)
		}
		_ = rows.Close()
	}

	kind := upwardKindFromChanges(changeKinds)
	brief := renderUpwardBrief(kind, agent.String, runID, workItemID, changes, artifactLines)
	doc := HandoffDoc{
		Did:                    []string{fmt.Sprintf("Run %s succeeded on work item %s (agent %s).", runID, workItemID, agent.String)},
		Next:                   []string{"Review the completed work and decide the next action for the parent item."},
		Findings:               brief,
		ArtifactsForDownstream: artifactRefs,
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("coord.PushUpwardHandoff: marshal brief: %w", err)
	}

	// (1) The at-most-once marker AND the payload. ON CONFLICT DO NOTHING +
	// RETURNING: only the first writer gets a row back; a re-entry matches
	// nothing and the whole push is a silent no-op.
	var marked string
	switch err := tx.QueryRowContext(ctx, `
		INSERT INTO coord.upward_handoff
		       (work_item_id, run_id, from_agent, manager, kind, brief)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6::jsonb)
		ON CONFLICT (work_item_id, run_id, kind) DO NOTHING
		RETURNING work_item_id`,
		workItemID, runID, agent.String, manager, kind, body).Scan(&marked); {
	case errors.Is(err, sql.ErrNoRows):
		return nil // already pushed for this (item, run, kind): at-most-once
	case err != nil:
		return fmt.Errorf("coord.PushUpwardHandoff: mark: %w", err)
	}

	// (2) §6.5 provenance on the canonical coord.audit_log — emitted exactly
	// once, gated on the single marker row the INSERT above returned.
	// to_state carries the hand-off kind (the a2a_settled to_state=outcome
	// precedent); the payload names both endpoints of the edge.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.audit_log
		       (work_item_id, run_id, event_type, principal, to_state, payload)
		VALUES ($1::uuid, $2::uuid, 'upward_handoff_pushed', $3, $4,
		        jsonb_build_object('from_agent', $5, 'manager', $6, 'kind', $4))`,
		workItemID, runID, principal, kind, agent.String, manager); err != nil {
		return fmt.Errorf("coord.PushUpwardHandoff: audit: %w", err)
	}

	// (3) THE WAKE: a child work item on the dispatchable 'todo' lane with
	// requested_agent = the manager, project/team copied from the settling
	// item, parent linked to it. The INSERT is a NEW row — the settling item
	// itself is never touched, so the ISI-4298 no-mutation-on-success pin
	// holds. Intake's next sweep mints the manager's Run: that IS the push.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.work_item
		       (project_id, team_id, parent_id, title, body, state, created_by, source, requested_agent)
		VALUES ($1::uuid, NULLIF($2,'')::uuid, $3::uuid, $4, $5, 'todo', $6, 'board', $7)`,
		projectID.String, teamID.String, workItemID,
		fmt.Sprintf("Hand-off (%s) from %s: %s", kind, agent.String, title.String),
		brief,
		principal, manager); err != nil {
		return fmt.Errorf("coord.PushUpwardHandoff: wake item: %w", err)
	}
	return nil
}

// upwardKindFromChanges maps the run's reported change_ref kinds to the
// hand-off kind: a pull request outranks a commit, a commit outranks a bare
// completion. Pure, so the mapping is unit-testable.
func upwardKindFromChanges(kinds []string) string {
	for _, k := range kinds {
		if k == "pull_request" {
			return upwardKindPR
		}
	}
	for _, k := range kinds {
		if k == "commit" {
			return upwardKindTicket
		}
	}
	return upwardKindDone
}

// renderUpwardBrief composes the wake item's body — the small brief: kind,
// agent, run/work-item refs, what changed, artifacts, next action. Kept pure
// (no db) so the template is unit-testable.
func renderUpwardBrief(kind, agent, runID, workItemID string, changes, artifactLines []string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Upward hand-off (%s) from agent %s.\n", kind, agent))
	b.WriteString(fmt.Sprintf("Run %s succeeded on work item %s; the parent ticket needs a decision.\n", runID, workItemID))
	if len(changes) > 0 {
		b.WriteString("What changed:\n")
		b.WriteString(strings.Join(changes, "\n"))
		b.WriteString("\n")
	}
	if len(artifactLines) > 0 {
		b.WriteString("Artifacts:\n")
		b.WriteString(strings.Join(artifactLines, "\n"))
		b.WriteString("\n")
	}
	b.WriteString("Next: review the outcome, then move or close the parent item.\n")
	return b.String()
}

// WithUpwardManager opts this effects instance into the WS-B upward hand-off
// spine: Terminal pushes the completion hand-off to manager. Empty manager
// leaves the spine off (the push is a silent no-op). Returns e for chaining.
func (e *ProdEffects) WithUpwardManager(manager string) *ProdEffects {
	e.upwardManager = manager
	return e
}
