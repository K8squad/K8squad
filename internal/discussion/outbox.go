package discussion

// ============================================================================
// Run-authored message → outbox live-echo capture (ISI-7272, parent ISI-7262 WS-1)
// ============================================================================
//
// WHY. The discussion Room live-appends a committed message from the
// `message.created` frame the apiserver's roomStreamPublisher emits
// (apiserver/projectbridge.go) — but that publisher is wired ONLY onto the
// apiserver's REST handler. An agent's final discussion message is posted
// through the discussion_post MCP tool (internal/memory/toolmcp.go) or its
// HTTP peer (toolhttp.go), both served by the SEPARATE cmd/memory process,
// which calls Store.PostMessage directly and holds no ProjectHub — so the
// reply never reached the room live and the user had to reload (ISI-7262).
//
// This seam closes the process gap by REUSING the pattern that already
// crosses it for `thinking` (ISI-5193/ISI-5208): after a run-authored
// message commits, the store captures a `discussion_message` run-entity
// event on coord.outbox, and the apiserver run-event projector bridges it
// onto the per-project `discussion` SSE event (runevents.go
// projectBridgeEventTypes). No new transport, no ProjectHub in cmd/memory.
//
// CONTRACT. Only run-authored posts capture (auth.RunID != nil): a human
// post is echoed synchronously by the in-process handler — which SKIPS the
// direct echo for run-authored posts (handler.go) so exactly one echo lands
// per committed message — and a NULL run_id row would never reach the
// projector anyway (RunEventsAfter keys on run_id). The payload is the SAME
// {type:"message.created",message:{…}} RoomEvent body the handler's echo
// marshals, so the console's parseRoomEvent folds both producers
// identically — idempotent by message id and ordered by createdAt
// (liveFeed.upsertMessage), so the projector's ~1s tail latency can never
// mis-order or duplicate a frame against the synchronous human echo.
//
// BEST-EFFORT, like every room-echo seam: the message row is the durable
// record, the outbox event the transient liveness signal. The outbox keys
// tenancy by the Project CR UID — which the discussion schema does NOT
// store (it keys projects by the "namespace/name" slug, ISI-3982) — so it is
// resolved through the SAME coord.claim→coord.work_item join the
// party-round enrichment and reply-hop resolver ride. A run with no claim
// row (an ambient agent post) skips the capture and stays reload-only,
// exactly as before this seam. The capture NEVER fails the write.

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/K8squad/K8squad/pkg/events"
)

// MessageEventType is the coord.outbox event_type of the run-authored
// discussion message echo: single-token by subject-taxonomy discipline
// (§10.2 — dots are NATS subject metacharacters). The apiserver projector
// maps it onto the room's `discussion` named SSE event.
const MessageEventType = "discussion_message"

// roomEventEnvelope is the wire body the apiserver project bridge re-publishes
// verbatim on the per-project `discussion` named event — field-compatible with
// the frame roomStreamPublisher marshals for the handler's synchronous human
// echo, so the console's parseRoomEvent cannot tell the producers apart.
type roomEventEnvelope struct {
	Type    string   `json:"type"`
	Message *Message `json:"message"`
}

// captureMessageCreated appends a best-effort `discussion_message` outbox
// event after a run-authored message commits, so the apiserver projector
// bridges it onto the room's live stream cross-process. No-op for human
// posts (RunID nil — the handler echoes those synchronously) and for a run
// with no coord.claim row. Errors are logged and swallowed: the message is
// already durable, and a lost echo costs liveness, never correctness.
func (s *Store) captureMessageCreated(ctx context.Context, auth AuthorContext, msg *Message) {
	if msg == nil || auth.RunID == nil {
		return
	}
	// The outbox keys tenancy by the Project CR UID; the discussion schema
	// keys by slug. Bridge the two through the run's claim → work item — the
	// same join enrichPartyRounds and the reply-hop resolver use.
	var projectUID, workItemID string
	err := s.db.QueryRowContext(ctx, `
		SELECT w.project_id::text, w.id::text
		  FROM coord.claim c
		  JOIN coord.work_item w ON w.id = c.work_item_id
		 WHERE c.run_id = $1::uuid
		 LIMIT 1`, *auth.RunID).Scan(&projectUID, &workItemID)
	if err != nil {
		slog.DebugContext(ctx, "discussion: message live-echo skipped (run has no claim/work item)",
			"runID", *auth.RunID, "err", err)
		return
	}
	payload, err := json.Marshal(roomEventEnvelope{Type: "message.created", Message: msg})
	if err != nil {
		slog.DebugContext(ctx, "discussion: message live-echo skipped (marshal failed)", "err", err)
		return
	}
	// The event rides its own transaction over the pool — the blessed
	// best-effort-projection posture (see CaptureRunForWorkItem): the message
	// row is the durable record, this row the transient liveness signal.
	if err := events.Capture(ctx, s.db, events.Event{
		Entity:     "run",
		ProjectID:  projectUID,
		Squad:      auth.TeamID.String(),
		EventType:  MessageEventType,
		WorkItemID: workItemID,
		RunID:      *auth.RunID,
		Payload:    payload,
	}); err != nil {
		slog.WarnContext(ctx, "discussion: message live-echo capture failed (message stays durable)",
			"runID", *auth.RunID, "messageID", msg.ID, "err", err)
	}
}
