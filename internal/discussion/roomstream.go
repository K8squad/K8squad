package discussion

// Room live-stream seam (ISI-5208, plan ISI-5207).
//
// The discussion Room "still feels like a forum, not a chat": a human posts an
// @-mention, a run starts, the agent replies — but the reply only appears on a
// manual reload, because NO producer fans a committed message onto the per-project
// SSE bus (ISI-5194 ProjectHub). The room's optimistic "{agent} is working…"
// affordance (ISI-5174) therefore never resolves to "replied" live either; it can
// only time out to "could not respond". This seam closes that gap: after a message
// commits, the handler echoes it onto the project's live stream so every open Room
// live-appends the reply and its working watch resolves — no reload, no polling.
//
// It stays a THIN decision-free seam (mirroring SetMentionDispatcher /
// SetReplyHopResolver): the discussion package never learns SSE or the ProjectHub
// key convention. The apiserver supplies the adapter (cmd/apiserver) that marshals
// each event to the same RoomEvent wire shape the console's liveFeed reducer parses
// (`{type:"message.created", message:{…}}` on the named `discussion` event) and
// publishes it keyed by the project slug. A nil publisher (DB-less dev runs, or
// before the wiring lands) leaves the room reload-only exactly as before.

// RoomStreamPublisher fans committed room events onto a project's live SSE stream.
// Every method is best-effort and MUST NOT block or error the write path: the
// message is already durable, so a publish miss loses only the live echo (a later
// reload materializes it). projectID is the console's "namespace/name" slug — the
// same key the ProjectHub and its RBAC gate use.
type RoomStreamPublisher interface {
	// PublishMessageCreated echoes a newly committed message as a `message.created`
	// room event, so subscribed Rooms live-append it (a human post to co-viewers, an
	// agent reply that also resolves the poster's working watch).
	PublishMessageCreated(projectID string, msg *Message)
	// PublishMessageDeleted echoes a soft-retraction as a `message.deleted` event so
	// the tombstone propagates live.
	PublishMessageDeleted(projectID string, messageID string)
}

// SetRoomStreamPublisher wires the live-echo seam onto the handler (post-construction,
// same rationale as SetMentionDispatcher). Without it, room writes are reload-only.
func (h *Handler) SetRoomStreamPublisher(p RoomStreamPublisher) { h.roomStream = p }
