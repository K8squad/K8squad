package apiserver

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
)

// ISI-5208: the room-stream publisher fans a committed message onto the per-project
// bus as a `discussion`-named SSE event carrying the RoomEvent wire shape the
// console's liveFeed reducer parses.
func TestRoomStreamPublisher_PublishMessageCreated(t *testing.T) {
	hub := NewProjectHub()
	sub := hub.Subscribe("ns/proj")
	pub := NewRoomStreamPublisher(hub)

	msg := &discussion.Message{
		ID:              uuid.New(),
		ThreadID:        uuid.New(),
		AuthorPrincipal: "john",
		Body:            "on it",
	}
	pub.PublishMessageCreated("ns/proj", msg)

	ev, ok := nrecvWithin(sub, time.Second)
	if !ok {
		t.Fatal("expected a project-bus event, got none")
	}
	if ev.Type != projectStreamEventName {
		t.Fatalf("event name: want %q, got %q", projectStreamEventName, ev.Type)
	}
	var wire struct {
		Type    string             `json:"type"`
		Message discussion.Message `json:"message"`
	}
	if err := json.Unmarshal([]byte(ev.Data), &wire); err != nil {
		t.Fatalf("data not a RoomEvent: %v (%s)", err, ev.Data)
	}
	if wire.Type != "message.created" {
		t.Fatalf("wire type: want message.created, got %q", wire.Type)
	}
	if wire.Message.ID != msg.ID || wire.Message.Body != "on it" {
		t.Fatalf("wire message mismatch: %+v", wire.Message)
	}
}

// A retraction fans a `message.deleted` RoomEvent carrying the id.
func TestRoomStreamPublisher_PublishMessageDeleted(t *testing.T) {
	hub := NewProjectHub()
	sub := hub.Subscribe("ns/proj")
	pub := NewRoomStreamPublisher(hub)

	pub.PublishMessageDeleted("ns/proj", "msg-7")

	ev, ok := nrecvWithin(sub, time.Second)
	if !ok {
		t.Fatal("expected a project-bus event, got none")
	}
	var wire struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal([]byte(ev.Data), &wire); err != nil {
		t.Fatalf("data not a RoomEvent: %v (%s)", err, ev.Data)
	}
	if wire.Type != "message.deleted" || wire.ID != "msg-7" {
		t.Fatalf("want message.deleted id msg-7, got %+v", wire)
	}
}

// A nil hub yields a nil publisher (the room stays reload-only) rather than panicking.
func TestNewRoomStreamPublisher_NilHub(t *testing.T) {
	if pub := NewRoomStreamPublisher(nil); pub != nil {
		t.Fatalf("want nil publisher for nil hub, got %#v", pub)
	}
}

// A nil reader yields a nil slug resolver (the projector bridge stays disarmed).
func TestNewProjectSlugResolver_NilReader(t *testing.T) {
	if r := NewProjectSlugResolver(nil); r != nil {
		t.Fatal("want nil resolver for nil reader")
	}
}
