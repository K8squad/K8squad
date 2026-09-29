package apiserver

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// projectStreamHarness mounts hub.streamProject on a mux (so {projectId} resolves) behind an
// httptest server and returns a live line channel for one open GET stream. The returned cancel
// tears the request down (unblocking the handler goroutine).
func projectStreamHarness(t *testing.T, hub *ProjectHub, projectID string) (<-chan string, context.CancelFunc) {
	t.Helper()
	r := mux.NewRouter()
	r.HandleFunc("/api/projects/{projectId}/stream", hub.streamProject).Methods(http.MethodGet)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	url := srv.URL + "/api/projects/" + projectID + "/stream"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open stream: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	lines := make(chan string, 256)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	return lines, cancel
}

// A fresh connection gets the subscribe preamble, then receives a subsequently-published event.
func TestStreamProject_LiveTail(t *testing.T) {
	hub := NewProjectHub()

	lines, cancel := projectStreamHarness(t, hub, "proj-1")
	defer cancel()

	// Preamble confirms the subscription is live before we publish.
	waitLine(t, lines, isExactly(": subscribed project=proj-1"), "subscribe preamble")

	hub.Publish("proj-1", Event{ID: "5", Type: "discussion.message", Data: "hello"})
	if got := waitLine(t, lines, hasPrefix("id: "), "first id line"); got != "id: 5" {
		t.Fatalf("want live id 5, got %q", got)
	}
	waitLine(t, lines, isExactly("event: discussion.message"), "event line")
	waitLine(t, lines, isExactly("data: hello"), "data line")
}

// An event published for a DIFFERENT project must not reach this subscriber.
func TestStreamProject_ScopedByProject(t *testing.T) {
	hub := NewProjectHub()

	lines, cancel := projectStreamHarness(t, hub, "proj-1")
	defer cancel()
	waitLine(t, lines, isExactly(": subscribed project=proj-1"), "subscribe preamble")

	// Wrong project — dropped.
	hub.Publish("proj-2", Event{ID: "9", Type: "discussion.message", Data: "not for me"})
	// Right project — delivered.
	hub.Publish("proj-1", Event{ID: "10", Type: "discussion.message", Data: "mine"})

	if got := waitLine(t, lines, hasPrefix("id: "), "first id line"); got != "id: 10" {
		t.Fatalf("cross-project leak: want first id 10, got %q", got)
	}
}

// Publish reports the delivered-subscriber count and never blocks with no subscribers.
func TestProjectHub_PublishDeliveryCount(t *testing.T) {
	hub := NewProjectHub()

	if n := hub.Publish("proj-1", Event{Data: "x"}); n != 0 {
		t.Fatalf("publish to empty project: want 0 delivered, got %d", n)
	}

	sub := hub.Subscribe("proj-1")
	defer hub.Unsubscribe("proj-1", sub)
	if got := hub.subscriberCount("proj-1"); got != 1 {
		t.Fatalf("subscriberCount = %d, want 1", got)
	}
	if n := hub.Publish("proj-1", Event{Data: "y"}); n != 1 {
		t.Fatalf("publish with one subscriber: want 1 delivered, got %d", n)
	}
}

// Unsubscribe is idempotent and closes the subscriber channel exactly once.
func TestProjectHub_UnsubscribeIdempotent(t *testing.T) {
	hub := NewProjectHub()
	sub := hub.Subscribe("proj-1")

	hub.Unsubscribe("proj-1", sub)
	if got := hub.subscriberCount("proj-1"); got != 0 {
		t.Fatalf("after unsubscribe subscriberCount = %d, want 0", got)
	}
	// Second unsubscribe must not panic (double-close) — the channel was already closed.
	hub.Unsubscribe("proj-1", sub)
}

// A slow consumer whose buffer is full is skipped rather than blocking the publisher.
func TestProjectHub_SlowConsumerDropped(t *testing.T) {
	hub := NewProjectHub()
	sub := hub.Subscribe("proj-1")
	defer hub.Unsubscribe("proj-1", sub)

	// Fill the buffer without draining, then overflow it.
	for i := 0; i < subBuffer; i++ {
		if n := hub.Publish("proj-1", Event{Data: "fill"}); n != 1 {
			t.Fatalf("fill publish %d: want 1 delivered, got %d", i, n)
		}
	}
	// The buffer is now full; the next publish must be dropped for this subscriber (0 delivered)
	// and must return promptly rather than blocking.
	done := make(chan int, 1)
	go func() { done <- hub.Publish("proj-1", Event{Data: "overflow"}) }()
	select {
	case n := <-done:
		if n != 0 {
			t.Fatalf("overflow publish: want 0 delivered (dropped), got %d", n)
		}
	case <-time.After(time.Second):
		t.Fatal("publish blocked on a full subscriber buffer")
	}
}

// An invalid projectId (CR/LF-bearing charset) is rejected before any stream write.
func TestStreamProject_RejectsInvalidProjectID(t *testing.T) {
	hub := NewProjectHub()
	req := httptest.NewRequest(http.MethodGet, "/api/projects/x/stream", nil)
	req = mux.SetURLVars(req, map[string]string{"projectId": "bad\nid"})
	rec := httptest.NewRecorder()

	hub.streamProject(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid projectId: want 400, got %d", rec.Code)
	}
}
