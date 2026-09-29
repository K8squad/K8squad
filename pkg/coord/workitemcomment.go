// workitemcomment.go — Story ISI-4406: the HUMAN work-item comment write op, the
// Team-fenced sibling of the agent-only run-token path (pkg/taskio → the free
// coord.AppendComment primitive in taskdetail.go).
//
// coord.AppendComment is a pure INSERT primitive: it takes no teamID and does NO
// tenancy check, because its only caller (the agent post-comment path) has already
// been authorised by a run token bound to that very work item. The human console
// path has no such binding — it is keyed by item id behind the §13 BFF choke point,
// exactly like the field-edit and lane-move ops — so it MUST fence tenancy itself or
// it would leak the existence of another Team's item. This method is that fence: it
// locks + reads the item, refuses a cross-tenant id as ErrWorkItemNotFound (404,
// existence-hiding, never a 403), then appends the comment and a §6.5
// 'work_item_commented' audit row in the SAME transaction. The author is
// server-supplied (the session principal), never client text — identical to every
// other write on this store.
//
// ISI-4495 (comment-triggered re-dispatch): a human comment is now also a WORK
// NUDGE. In the same transaction, when the item is parked on a lane a fresh Run
// can be minted from AND no live run holds the checkout, the comment advances the
// lane to 'todo' — exactly the dispatch signal Intake sweeps on (ADR-0022 §1.2:
// "start a Run" IS the lane advance). Gated fail-closed:
//
//   - backlog    → NEVER re-triggered (board decision ISI-4495: a backlog comment
//     is conversation, not work; assignment stays the explicit dispatch verb);
//   - todo       → already the dispatch lane (a Run is minting/about to mint — a
//     second nudge is a no-op, never a duplicate signal);
//   - done / cancelled → terminal; reopening is a deliberate human lane move
//     (kanban DnD / the detail status control), never a comment side-effect;
//   - any lane with a LIVE checkout holder (coord.claim.holder_principal NOT
//     NULL — a run is actively working) → never yanked; the comment lands in the
//     thread and the live run keeps custody (§6.2);
//   - everything else (the six working phases + the settled engine lanes — a
//     succeeded run parks the item in in_progress with the checkout RELEASED,
//     settle.go) → re-enter 'todo' so Intake mints the next Run.
//
// The move is a guarded CAS on the lane we locked (same discipline as
// humanstate.go (4)): a racing lane change leaves 0 rows and the comment is
// still persisted — the nudge is skipped, never a clobber. Provenance is ONE
// extra §6.5 'state_transition' audit row (the exact shape every other lane move
// writes, initiator "human", payload via=comment) so the thread's statusHistory
// narrates the nudge exactly like a kanban move.
package coord

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// HumanCommentOutcome is the result of one human comment: the persisted comment
// (embedded, same JSON shape as before — additive fields only) plus, when the
// comment re-triggered work, the lane it moved the item to. ReTriggered=false
// leaves the state fields empty (omitted on the wire).
type HumanCommentOutcome struct {
	TaskComment
	ReTriggered bool   `json:"reTriggered"`
	FromState   string `json:"fromState,omitempty"`
	ToState     string `json:"toState,omitempty"`
}

// commentReTriggerLanes are the lanes a human comment re-dispatches from: the
// six working phases and the two engine lanes (a settled/succeeded run parks the
// item on in_progress with the checkout released; in_review is the agent's own
// "awaiting review" self-report — both are "not being worked right now" the
// moment the claim holder is NULL). backlog/todo/terminals are excluded by
// design (see the file header).
var commentReTriggerLanes = map[string]bool{
	"design":         true,
	"planning":       true,
	"implementation": true,
	"code_review":    true,
	"testing":        true,
	"documentation":  true,
	"in_progress":    true,
	"in_review":      true,
}

// AppendHumanComment appends one human-authored comment to a work item under a Team
// fence and records its §6.5 audit provenance atomically — and, when the item is
// parked and unheld (ISI-4495), advances the lane to 'todo' in the same
// transaction so the operator Intake sweep mints the next Run.
//
// Semantics (mirroring UpdateWorkItem so the whole human write surface answers one
// shape):
//   - (outcome, nil): the comment is persisted; Author is the server-stamped
//     principal. When the comment re-triggered work, ReTriggered is true and
//     FromState/ToState carry the lane move (ToState is always "todo").
//   - (zero, ErrInvalidWorkItem): workItemID/principal missing, or an empty body (400).
//   - (zero, ErrWorkItemNotFound): no such item, or one outside teamID — existence-
//     hiding (404). Pass teamID == "" only for a trusted, already-tenancy-checked
//     caller (fleet-admin, ISI-3937), exactly like UpdateWorkItem.
//   - (zero, err): infrastructure failure; nothing was written.
//
// ISI-5214 (parent ISI-5212 S2): a comment may carry structured ticket LINKS the
// ticket-detail `#`-picker collected (refs). This mirrors the discussion room's
// postMessageReq.References + StampTicketRefsFrom seam (internal/discussion), done
// coord-natively: each candidate is deduped + UUID-validated, then resolved against
// THIS ticket's own project inside the same transaction (an unknown, out-of-project,
// or cross-Team ref is dropped, never persisted), its Title canonicalized from
// coord.work_item, and the survivors merged into coord.comment.payload under the
// `references` key. A link is never a dispatch and never a write fence — an empty or
// unresolvable ref set degrades to a plain, link-free (but durable) comment.
func (s *WorkItemWriteStore) AppendHumanComment(ctx context.Context, workItemID, teamID, principal, body string, refs []CommentRef) (HumanCommentOutcome, error) {
	if workItemID == "" || principal == "" {
		return HumanCommentOutcome{}, fmt.Errorf("%w: workItemID and principal are required", ErrInvalidWorkItem)
	}
	if body == "" {
		return HumanCommentOutcome{}, fmt.Errorf("%w: comment body may not be empty", ErrInvalidWorkItem)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return HumanCommentOutcome{}, fmt.Errorf("coord.AppendHumanComment: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Tenancy fence (§12.1): lock + read the target (and the live claim holder,
	// the ISI-4495 live-run guard); an item outside the caller's Team is 404
	// (existence-hiding), never a cross-tenant 403 — the same wall the field-edit
	// and lane-move ops present. FOR UPDATE OF wi serialises against a racing
	// delete/reparent/lane-move so the FK the INSERT depends on cannot vanish
	// mid-txn and the re-dispatch CAS acts on the lane we read.
	var currentState string
	var teamOut sql.NullString
	var liveHolder sql.NullString
	var projectID string
	err = tx.QueryRowContext(ctx, `
		SELECT wi.state, wi.team_id::text, wi.project_id::text, c.holder_principal
		  FROM coord.work_item wi
		  LEFT JOIN coord.claim c ON c.work_item_id = wi.id
		 WHERE wi.id = $1::uuid
		   FOR UPDATE OF wi`,
		workItemID).Scan(&currentState, &teamOut, &projectID, &liveHolder)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return HumanCommentOutcome{}, ErrWorkItemNotFound
	case err != nil:
		return HumanCommentOutcome{}, fmt.Errorf("coord.AppendHumanComment: read item: %w", err)
	}
	if teamID != "" && (!teamOut.Valid || teamOut.String != teamID) {
		return HumanCommentOutcome{}, ErrWorkItemNotFound
	}

	// Resolve the `#`-picker ticket links against THIS ticket's project (ISI-5214),
	// dropping any that don't exist / belong to another project or Team, and
	// canonicalize each surviving title. An empty result leaves payload NULL, so a
	// plain comment stays link-free (mirror of discussion.StampTicketRefsFrom's
	// best-effort, degrade-to-link-free contract).
	resolvedRefs, err := resolveCommentRefs(ctx, tx, projectID, teamID, refs)
	if err != nil {
		return HumanCommentOutcome{}, fmt.Errorf("coord.AppendHumanComment: resolve refs: %w", err)
	}
	// payloadArg stays nil (→ NULL) for a link-free comment; otherwise the JSON is
	// passed as a string with a $N::jsonb cast — the SAME lib/pq discipline the
	// re-dispatch audit INSERT below uses, avoiding the []byte→bytea ambiguity.
	var payloadArg any
	if len(resolvedRefs) > 0 {
		encoded, err := json.Marshal(map[string]any{"references": resolvedRefs})
		if err != nil {
			return HumanCommentOutcome{}, fmt.Errorf("coord.AppendHumanComment: marshal refs: %w", err)
		}
		payloadArg = string(encoded)
	}

	var created time.Time
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO coord.comment (work_item_id, author_principal, body, payload)
		VALUES ($1::uuid, $2, $3, $4::jsonb)
		RETURNING created_at`, workItemID, principal, body, payloadArg).Scan(&created); err != nil {
		return HumanCommentOutcome{}, fmt.Errorf("coord.AppendHumanComment: insert: %w", err)
	}

	// §6.5 provenance in the same txn, fence NULL (ADR-037: a comment is neither a
	// custody nor a lease op) — the audit trail records WHO commented, the body
	// itself lives in the append-only coord.comment row.
	if err := s.writeAudit(ctx, tx, workItemID, "work_item_commented", principal, "", map[string]any{}); err != nil {
		return HumanCommentOutcome{}, err
	}

	outcome := HumanCommentOutcome{TaskComment: TaskComment{Author: principal, Body: body, CreatedAt: created, References: resolvedRefs}}

	// ISI-4495 comment-triggered re-dispatch: parked lane + no live checkout
	// holder → advance to the dispatch lane so Intake mints the next Run. The
	// WHERE re-asserts BOTH the lane we locked and the holder-still-absent guard,
	// so a run that claimed between our read and this write (claim sets
	// in_progress + holder) fails the CAS and the comment survives un-nudged —
	// fail-closed, never a clobber of a live run's lane.
	if commentReTriggerLanes[currentState] && !liveHolder.Valid {
		res, err := tx.ExecContext(ctx, `
			UPDATE coord.work_item
			   SET state = 'todo', updated_at = now()
			 WHERE id = $1::uuid
			   AND state = $2
			   AND NOT EXISTS (
			       SELECT 1 FROM coord.claim c
			        WHERE c.work_item_id = coord.work_item.id
			          AND c.holder_principal IS NOT NULL)`,
			workItemID, currentState)
		if err != nil {
			return HumanCommentOutcome{}, fmt.Errorf("coord.AppendHumanComment: re-dispatch: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return HumanCommentOutcome{}, fmt.Errorf("coord.AppendHumanComment: re-dispatch rows: %w", err)
		} else if n == 1 {
			// The SAME §6.5 'state_transition' audit row every other lane move
			// writes (humanstate.go (5)) — initiator "human", payload names the
			// comment as the via, so the thread's statusHistory narrates the
			// nudge exactly like a kanban move and the provenance stays one shape.
			payload, err := json.Marshal(map[string]any{
				"initiator":  "human",
				"from_state": currentState,
				"to_state":   "todo",
				"via":        "comment",
			})
			if err != nil {
				return HumanCommentOutcome{}, fmt.Errorf("coord.AppendHumanComment: re-dispatch payload: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO coord.audit_log
				       (work_item_id, event_type, principal, from_state, to_state, payload)
				VALUES ($1::uuid, 'state_transition', $2, $3, 'todo', $4::jsonb)`,
				workItemID, principal, currentState, string(payload)); err != nil {
				return HumanCommentOutcome{}, fmt.Errorf("coord.AppendHumanComment: re-dispatch audit: %w", err)
			}
			outcome.ReTriggered = true
			outcome.FromState = currentState
			outcome.ToState = "todo"
		}
		// n == 0: the lane or holder moved under us — the comment still lands,
		// the nudge is skipped (fail-closed, see above).
	}

	if err := tx.Commit(); err != nil {
		return HumanCommentOutcome{}, fmt.Errorf("coord.AppendHumanComment: commit: %w", err)
	}
	return outcome, nil
}

// resolveCommentRefs is the coord-native mirror of the discussion normalize →
// resolve → survive pipeline (discussion.normalizeTicketRefs + the apiserver
// ticketRefResolver, ISI-5165). It runs inside the comment transaction: it dedups
// candidates by work-item id (first-seen wins, order preserved), drops any with a
// blank or non-UUID id, then keeps only those that EXIST in the commented ticket's
// OWN project within the caller's Team scope — an unknown, out-of-project, or
// cross-Team id is dropped (existence-hiding), never persisted. The surviving title
// is canonicalized from coord.work_item so a client-supplied chip label cannot spoof
// it. teamID == "" is the trusted fleet-admin path (mirrors AppendHumanComment's own
// team-fence convention). An empty input or an all-dropped set returns nil, so the
// comment persists link-free.
func resolveCommentRefs(ctx context.Context, tx *sql.Tx, projectID, teamID string, refs []CommentRef) ([]CommentRef, error) {
	if len(refs) == 0 || projectID == "" {
		return nil, nil
	}
	seen := make(map[string]bool, len(refs))
	out := make([]CommentRef, 0, len(refs))
	for _, r := range refs {
		id := r.WorkItemID
		if id == "" {
			continue
		}
		if _, err := uuid.Parse(id); err != nil {
			continue // work items are UUID-keyed (ISI-5134); a non-UUID token is not a valid ref
		}
		if seen[id] {
			continue
		}
		seen[id] = true

		// The candidate must live in the SAME project as the commented ticket and be
		// visible to the caller's Team; an empty teamID is the trusted fleet-admin path.
		var title string
		err := tx.QueryRowContext(ctx, `
			SELECT title
			  FROM coord.work_item
			 WHERE id = $1::uuid
			   AND project_id = $2::uuid
			   AND ($3 = '' OR team_id::text = $3)`,
			id, projectID, teamID).Scan(&title)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			continue // unknown / out-of-project / cross-Team — drop (existence-hiding)
		case err != nil:
			return nil, err
		}
		out = append(out, CommentRef{WorkItemID: id, Title: title})
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
