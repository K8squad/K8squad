package apiserver

import (
	"context"
	"encoding/json"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/K8squad/K8squad/internal/discussion"
)

// ============================================================================
// Per-project SSE bridge glue (ISI-5208, plan ISI-5207)
// ============================================================================
//
// Two producers fan onto the per-project bus (ProjectHub, ISI-5194) so the
// discussion Room live-tails instead of polling:
//   1. the run-event projector bridges an active run's `thinking` deltas
//      (runevents.go WithProjectBridge), keyed by the run's Project slug; and
//   2. the discussion handler echoes committed messages (roomstream.go seam),
//      so an agent reply live-appends and resolves the poster's working watch.
// This file supplies the apiserver adapters both wirings need: the UID→slug
// resolver the projector calls, and the RoomStreamPublisher the handler calls.

// projectStreamEventName is the SSE event name (`event:` line) that carries a
// discussion RoomEvent on the per-project stream. The console's subscribeRoom
// listens on exactly this name and parses the data body with its liveFeed
// reducer, so the wire contract is: event=discussion, data={type,message|id}.
const projectStreamEventName = "discussion"

// NewProjectSlugResolver builds the ProjectSlugResolver the run-event projector
// uses to key the per-project bus: it maps a run's outbox project_id (the Project
// CR UID) to the console's "namespace/name" slug via the SAME informer-cache
// resolution the dashboard/overview read models use, so the projector resolves a
// Project identically to every other surface. A nil reader ⇒ nil resolver ⇒ the
// projector leaves the project bridge disarmed (per-run hub only).
func NewProjectSlugResolver(reader client.Reader) ProjectSlugResolver {
	if reader == nil {
		return nil
	}
	return func(ctx context.Context, projectUID string) (string, bool) {
		ns, name, _, err := resolveProjectFleetWideWithUID(ctx, reader, projectUID)
		if err != nil || ns == "" || name == "" {
			return "", false
		}
		return ns + "/" + name, true
	}
}

// roomStreamPublisher adapts the discussion room-echo seam onto the per-project
// SSE bus: it marshals each committed room event to the RoomEvent wire shape the
// console's liveFeed reducer parses and publishes it on the `discussion` named
// event keyed by the project slug. Publish is best-effort (ProjectHub drops a
// slow subscriber rather than blocking), so a wedged browser never stalls a write.
type roomStreamPublisher struct {
	hub *ProjectHub
}

// NewRoomStreamPublisher wires the discussion handler's live-echo seam
// (SetRoomStreamPublisher) to the per-project bus. A nil hub ⇒ nil publisher ⇒
// the room stays reload-only.
func NewRoomStreamPublisher(hub *ProjectHub) discussion.RoomStreamPublisher {
	if hub == nil {
		return nil
	}
	return &roomStreamPublisher{hub: hub}
}

// PublishMessageCreated echoes a committed message as a `message.created` room
// event. A marshal failure is swallowed — the row is already durable and a later
// reload materializes it.
func (p *roomStreamPublisher) PublishMessageCreated(projectID string, msg *discussion.Message) {
	if msg == nil {
		return
	}
	data, err := json.Marshal(struct {
		Type    string              `json:"type"`
		Message *discussion.Message `json:"message"`
	}{Type: "message.created", Message: msg})
	if err != nil {
		return
	}
	p.hub.Publish(projectID, Event{Type: projectStreamEventName, Data: string(data)})
}

// PublishMessageDeleted echoes a soft-retraction as a `message.deleted` event.
func (p *roomStreamPublisher) PublishMessageDeleted(projectID string, messageID string) {
	data, err := json.Marshal(struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}{Type: "message.deleted", ID: messageID})
	if err != nil {
		return
	}
	p.hub.Publish(projectID, Event{Type: projectStreamEventName, Data: string(data)})
}
