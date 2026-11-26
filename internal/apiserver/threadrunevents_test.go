package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/events"
)

// ISI-7869 (WS-C of parent ISI-7864): GET /api/projects/{projectId}/
// discussion/threads/{threadId}/run-events — the durable discussion-feed
// hydration read. These tests pin the contract the console hydration relies
// on: the ledger anchor correlation, the room-event type filter, ascending
// per-run tails keyed by the outbox row id, and the honest 400/401 surface.

const (
	testThreadID  = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	testAnchorMsg = "77777777-6666-5555-4444-333333333333"
)

type fakeAnchorReader struct {
	anchors []discussion.ThreadRunAnchor
	err     error
}

func (f *fakeAnchorReader) ThreadRunAnchors(_ context.Context, _ string, _ uuid.UUID) ([]discussion.ThreadRunAnchor, error) {
	return f.anchors, f.err
}

type failingRunReader struct{ events.RunEventReader }

func (failingRunReader) RunEventsForRun(context.Context, string, int64, int) ([]events.RunEvent, error) {
	return nil, errors.New("reader down")
}

func threadRunEventsRequest(t *testing.T, target string, anchors ThreadRunAnchorReader, reader events.RunEventReader, authed bool) *httptest.ResponseRecorder {
	t.Helper()
	router := mux.NewRouter()
	router.Path("/api/projects/{projectId}/discussion/threads/{threadId}/run-events").
		Methods(http.MethodGet).HandlerFunc(threadRunEventsHandler(anchors, reader))
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if authed {
		req = req.WithContext(discussion.WithAuth(req.Context(), discussion.AuthorContext{Principal: "user:alice"}))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func anchoredThreadReader() *fakeRunReader {
	reader := &fakeRunReader{}
	ts := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	reader.add(
		events.RunEvent{ID: 11, RunID: testRunUID, EventType: "thinking", Payload: []byte(`{"runId":"` + testRunUID + `","author":"agent:john","body":"step one","seq":1}`), OccurredAt: ts},
		events.RunEvent{ID: 12, RunID: testRunUID, EventType: "tool_result", Payload: []byte(`{"x":1}`), OccurredAt: ts.Add(time.Second)},
		events.RunEvent{ID: 13, RunID: testRunUID, EventType: "ticket_comment", Payload: []byte(`{"author":"agent:john","body":"final"}`), OccurredAt: ts.Add(2 * time.Second)},
		events.RunEvent{ID: 14, RunID: testRunUID, EventType: "discussion_message", Payload: []byte(`{"type":"message.created"}`), OccurredAt: ts.Add(3 * time.Second)},
		events.RunEvent{ID: 15, RunID: testRunUID, EventType: "ended", Payload: []byte(`{"agent":"john"}`), OccurredAt: ts.Add(4 * time.Second)},
		events.RunEvent{ID: 16, RunID: otherRunUID, EventType: "thinking", Payload: []byte(`{"author":"other"}`), OccurredAt: ts.Add(5 * time.Second)},
	)
	return reader
}

func decodeThreadRunEvents(t *testing.T, rec *httptest.ResponseRecorder) ThreadRunEventsResponse {
	t.Helper()
	var resp ThreadRunEventsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

func TestThreadRunEventsHydratesAnchoredRoomTail(t *testing.T) {
	anchors := &fakeAnchorReader{anchors: []discussion.ThreadRunAnchor{{RunID: testRunUID, AnchorMessageID: testAnchorMsg}}}
	rec := threadRunEventsRequest(t, "/api/projects/team-1/proj-a/discussion/threads/"+testThreadID+"/run-events", anchors, anchoredThreadReader(), true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	resp := decodeThreadRunEvents(t, rec)
	if resp.ThreadID != testThreadID {
		t.Fatalf("threadId = %s, want the path id", resp.ThreadID)
	}
	if len(resp.Runs) != 1 {
		t.Fatalf("runs = %d, want 1 (the anchored run only)", len(resp.Runs))
	}
	run := resp.Runs[0]
	if run.RunID != testRunUID || run.AnchorMessageID != testAnchorMsg {
		t.Fatalf("run = %+v, want the ledger pair verbatim", run)
	}
	// Only the room-bridged types hydrate, ascending by outbox row id.
	if len(run.Events) != 4 {
		t.Fatalf("events = %d, want 4 (thinking, ticket_comment, discussion_message, ended; tool_result filtered)", len(run.Events))
	}
	wantIDs := []string{"11", "13", "14", "15"}
	wantTypes := []string{"thinking", "ticket_comment", "discussion_message", "ended"}
	for i, ev := range run.Events {
		if ev.ID != wantIDs[i] || ev.Type != wantTypes[i] {
			t.Fatalf("event[%d] = (%s, %s), want (%s, %s)", i, ev.ID, ev.Type, wantIDs[i], wantTypes[i])
		}
		if len(ev.Data) == 0 {
			t.Fatalf("event[%d] carries no payload", i)
		}
	}
}

func TestThreadRunEventsEmptyForDispatchlessThread(t *testing.T) {
	anchors := &fakeAnchorReader{}
	rec := threadRunEventsRequest(t, "/api/projects/team-1/proj-a/discussion/threads/"+testThreadID+"/run-events", anchors, anchoredThreadReader(), true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeThreadRunEvents(t, rec)
	if resp.Runs == nil || len(resp.Runs) != 0 {
		t.Fatalf("runs = %+v, want an empty (never null) array", resp.Runs)
	}
}

func TestThreadRunEventsUnauthenticated(t *testing.T) {
	anchors := &fakeAnchorReader{}
	rec := threadRunEventsRequest(t, "/api/projects/team-1/proj-a/discussion/threads/"+testThreadID+"/run-events", anchors, anchoredThreadReader(), false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestThreadRunEventsBadThreadID(t *testing.T) {
	anchors := &fakeAnchorReader{}
	rec := threadRunEventsRequest(t, "/api/projects/team-1/proj-a/discussion/threads/not-a-uuid/run-events", anchors, anchoredThreadReader(), true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestThreadRunEventsAnchorReadFailureIsBadGateway(t *testing.T) {
	anchors := &fakeAnchorReader{err: errors.New("ledger down")}
	rec := threadRunEventsRequest(t, "/api/projects/team-1/proj-a/discussion/threads/"+testThreadID+"/run-events", anchors, anchoredThreadReader(), true)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

func TestThreadRunEventsPerRunFailureSkipsRun(t *testing.T) {
	anchors := &fakeAnchorReader{anchors: []discussion.ThreadRunAnchor{{RunID: testRunUID, AnchorMessageID: testAnchorMsg}}}
	rec := threadRunEventsRequest(t, "/api/projects/team-1/proj-a/discussion/threads/"+testThreadID+"/run-events", anchors, failingRunReader{anchoredThreadReader()}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (per-run failure degrades)", rec.Code)
	}
	resp := decodeThreadRunEvents(t, rec)
	if len(resp.Runs) != 0 {
		t.Fatalf("runs = %d, want 0 (the unreadable run is skipped)", len(resp.Runs))
	}
}
