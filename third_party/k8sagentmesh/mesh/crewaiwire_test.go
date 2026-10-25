/*
Copyright 2026 The K8sAgentMesh Authors.

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

package mesh

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/K8sAgentMesh/K8sAgentMesh/a2a"
)

// fakeCrewServer is an httptest stand-in for a CrewAI A2A server speaking the
// pinned 0.3.x wire (JSON-RPC at /a2a, message/stream SSE, tasks/get,
// tasks/cancel), in the registry_fake_test tradition: in-memory state,
// scripted knobs, everything the router tests assert on recorded.
type fakeCrewServer struct {
	srv *httptest.Server

	mu sync.Mutex
	// submits records every message the server accepted (message/stream AND
	// message/send) — the C1 "exactly one submission" assertions read it.
	submits     []crewWireMessage
	submissions int
	cancelCalls int
	getCalls    int
	authHeaders []string

	// state is the crew-side task state tasks/get reports.
	state string
	// completionsAfterGets holds "working" until the Nth tasks/get, then
	// flips to state — the reattach/poll scripts.
	completionsAfterGets int
	// streamReject makes message/stream answer the -32004 envelope (the
	// capabilities.streaming=false fallback script).
	streamReject bool
	// holdStream keeps the message/stream SSE body open after the scripted
	// frames (the in-flight scripts for C1 reattach and cancel-on-close).
	holdStream bool
	// v10frames serves 1.0 wrapper frames instead of 0.3.x kind frames.
	v10frames bool
	// sseClosed reports the last message/stream body was closed by the client.
	sseBodiesClosed int
}

func newFakeCrewServer(t *testing.T) *fakeCrewServer {
	t.Helper()
	f := &fakeCrewServer{state: "working"}
	mux := http.NewServeMux()
	mux.HandleFunc("/a2a", f.handleRPC)
	mux.HandleFunc("/.well-known/agent-card.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"fake-crew","protocolVersion":"0.3.0"}`))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCrewServer) endpoint() string { return f.srv.URL }

func (f *fakeCrewServer) handleRPC(w http.ResponseWriter, r *http.Request) {
	var req crewWireRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad envelope", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.authHeaders = append(f.authHeaders, r.Header.Get("Authorization"))
	f.mu.Unlock()

	switch req.Method {
	case crewaiMethodStream:
		f.handleStream(w, r, &req)
	case crewaiMethodSend:
		f.handleSend(w, &req)
	case crewaiMethodGet:
		f.handleGet(w)
	case crewaiMethodCancel:
		f.handleCancel(w)
	default:
		writeJSON(w, http.StatusOK, crewWireEnvelope{ID: req.ID, Error: &crewWireRPCError{Code: -32601, Message: "method not found"}})
	}
}

func (f *fakeCrewServer) handleStream(w http.ResponseWriter, r *http.Request, req *crewWireRequest) {
	var params crewWireSendParams
	_ = json.Unmarshal(mustJSON(req.Params), &params)
	f.mu.Lock()
	streamReject := f.streamReject
	hold := f.holdStream
	v10 := f.v10frames
	if !streamReject {
		// Record only deliveries: a rejected message/stream never reached
		// the crew, so C1 accounting must not count it.
		f.submits = append(f.submits, params.Message)
		f.submissions++
	}
	f.mu.Unlock()
	if streamReject {
		writeJSON(w, http.StatusOK, crewWireEnvelope{ID: req.ID, Error: &crewWireRPCError{Code: -32004, Message: "streaming not supported"}})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fl := w.(http.Flusher)
	frame := func(result any) {
		env := crewWireEnvelope{ID: req.ID, Result: mustJSON(result)}
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(env))
		fl.Flush()
	}
	if v10 {
		frame(map[string]any{"task": map[string]any{"id": "g-1", "contextId": "ctx-1", "status": map[string]any{"state": "TASK_STATE_WORKING"}}})
		frame(map[string]any{"statusUpdate": map[string]any{"taskId": "g-1", "contextId": "ctx-1", "status": map[string]any{"state": "TASK_STATE_COMPLETED"}}})
		return
	}
	frame(crewWireTask{Kind: "task", ID: "g-1", ContextID: "ctx-1", Status: crewWireStatus{State: "working"}})
	frame(map[string]any{
		"kind": "status-update", "taskId": "g-1", "contextId": "ctx-1",
		"status": map[string]any{"state": "working"},
	})
	frame(map[string]any{
		"kind": "message", "role": "agent", "messageId": "m-1",
		"parts": []map[string]any{{"kind": "text", "text": "researching"}},
	})
	frame(map[string]any{
		"kind": "artifact-update", "taskId": "g-1", "contextId": "ctx-1",
		"artifact": map[string]any{"artifactId": "a-1", "name": "report",
			"parts": []map[string]any{{"kind": "text", "text": "the report body"}}},
	})
	if hold {
		// In-flight script: emit progress, then hold the body open until the
		// client closes it (Close cancels) or the test flips state.
		<-r.Context().Done()
		f.mu.Lock()
		f.sseBodiesClosed++
		f.mu.Unlock()
		return
	}
	frame(map[string]any{
		"kind": "status-update", "taskId": "g-1", "contextId": "ctx-1", "final": true,
		"status": map[string]any{"state": "completed",
			"message": map[string]any{"role": "agent", "parts": []map[string]any{{"kind": "text", "text": "done"}}}},
	})
}

func (f *fakeCrewServer) handleSend(w http.ResponseWriter, req *crewWireRequest) {
	var params crewWireSendParams
	_ = json.Unmarshal(mustJSON(req.Params), &params)
	f.mu.Lock()
	f.submits = append(f.submits, params.Message)
	f.submissions++
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, crewWireEnvelope{ID: req.ID, Result: mustJSON(
		crewWireTask{Kind: "task", ID: "g-1", ContextID: "ctx-1", Status: crewWireStatus{State: "working"}},
	)})
}

func (f *fakeCrewServer) handleGet(w http.ResponseWriter) {
	f.mu.Lock()
	f.getCalls++
	state := f.state
	if f.completionsAfterGets > 0 && f.getCalls <= f.completionsAfterGets {
		state = "working"
	}
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, crewWireEnvelope{ID: 1, Result: mustJSON(
		crewWireTask{Kind: "task", ID: "g-1", ContextID: "ctx-1", Status: crewWireStatus{State: state}},
	)})
}

func (f *fakeCrewServer) handleCancel(w http.ResponseWriter) {
	f.mu.Lock()
	f.cancelCalls++
	f.state = "canceled"
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, crewWireEnvelope{ID: 1, Result: mustJSON(
		crewWireTask{Kind: "task", ID: "g-1", ContextID: "ctx-1", Status: crewWireStatus{State: "canceled"}},
	)})
}

func (f *fakeCrewServer) submissionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.submissions
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// collectEvents drains a stream's events until the channel closes or the
// deadline passes, returning the events and whether the stream settled.
func collectEvents(t *testing.T, s Stream) []a2a.Event {
	t.Helper()
	var out []a2a.Event
	timeout := time.After(8 * time.Second)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-timeout:
			t.Fatalf("stream did not close within deadline; events so far: %+v", summarize(out))
		}
	}
}

func summarize(evs []a2a.Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, fmt.Sprintf("%d/%s", e.Seq, e.Type))
	}
	return out
}

// assertSeqGapFree asserts C4: seq starts where the stream was told to and
// increments by exactly one per event.
func assertSeqGapFree(t *testing.T, evs []a2a.Event, first uint64) {
	t.Helper()
	for i, ev := range evs {
		if want := first + uint64(i); ev.Seq != want {
			t.Fatalf("event %d seq = %d, want %d (gap-free C4)", i, ev.Seq, want)
		}
	}
}

func crewTask(id string) a2a.Task {
	return a2a.Task{
		A2ATaskID: id,
		Envelope:  a2a.Envelope{SystemContext: "You are a research crew.", Input: "Summarize the wire."},
	}
}

func TestCrewaiRouterHappyPathStreamsFrames(t *testing.T) {
	f := newFakeCrewServer(t)
	r := NewCrewaiRouter(nil, "")

	s, err := r.Route(context.Background(), f.endpoint(), crewTask("t-1"))
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	evs := collectEvents(t, s)
	assertSeqGapFree(t, evs, 1)

	// The frame sequence: task snapshot (working), status working, message,
	// artifact ref + artifact text message, terminal completed (with its
	// embedded message lifted ahead of the status event).
	var states []a2a.TaskState
	var msgs, artifacts int
	for _, ev := range evs {
		switch ev.Type {
		case a2a.EventStatus:
			sp, ok := ev.Payload.(a2a.StatusPayload)
			if !ok {
				t.Fatalf("status event payload = %T", ev.Payload)
			}
			states = append(states, sp.State)
		case a2a.EventMessage:
			msgs++
		case a2a.EventArtifactRef:
			artifacts++
			ref, ok := ev.Payload.(a2a.ArtifactRef)
			if !ok {
				t.Fatalf("artifact payload = %T", ev.Payload)
			}
			if ref.URI != "a2a://g-1/a-1" {
				t.Fatalf("artifact URI = %q, want a2a://g-1/a-1", ref.URI)
			}
			if ref.SHA256 != "" {
				t.Fatalf("artifact sha256 must stay empty (never fabricated), got %q", ref.SHA256)
			}
		}
	}
	if len(states) < 3 || states[0] != a2a.TaskWorking || states[len(states)-1] != a2a.TaskCompleted {
		t.Fatalf("state sequence = %v, want working…completed", states)
	}
	if msgs < 3 { // progress message + artifact body + terminal message
		t.Fatalf("message events = %d, want ≥3", msgs)
	}
	if artifacts != 1 {
		t.Fatalf("artifact events = %d, want 1", artifacts)
	}
	if st := s.Status(); st.State != a2a.TaskCompleted {
		t.Fatalf("terminal status = %v, want completed", st.State)
	}
	if n := f.submissionCount(); n != 1 {
		t.Fatalf("submissions = %d, want exactly 1 (one send, one stream)", n)
	}
	// The message carried the module correlation surface.
	f.mu.Lock()
	msg := f.submits[0]
	f.mu.Unlock()
	if msg.MessageID != "t-1" {
		t.Fatalf("messageId = %q, want the a2a_task_id t-1", msg.MessageID)
	}
	if msg.Metadata["a2a_task_id"] != "t-1" {
		t.Fatalf("metadata a2a_task_id = %q", msg.Metadata["a2a_task_id"])
	}
	if !strings.Contains(crewPartsText(msg.Parts), "Summarize the wire.") {
		t.Fatalf("message text %q missing the task input", crewPartsText(msg.Parts))
	}
}

func TestCrewaiRouterV10WrapperFrames(t *testing.T) {
	f := newFakeCrewServer(t)
	f.v10frames = true
	// The v10 script closes the body after the terminal wrapper frame; if the
	// tail hands over to polling, handleGet reports completed too.
	f.state = "completed"
	r := NewCrewaiRouter(nil, "")

	s, err := r.Route(context.Background(), f.endpoint(), crewTask("t-v10"))
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	evs := collectEvents(t, s)
	var last a2a.TaskState
	for _, ev := range evs {
		if ev.Type == a2a.EventStatus {
			last = ev.Payload.(a2a.StatusPayload).State
		}
	}
	if last != a2a.TaskCompleted {
		t.Fatalf("last state = %v, want completed (1.0 wrapper frames classified)", last)
	}
}

// C1: a re-route while the task is in flight must reattach (tasks/get), never
// re-send — CrewAI documents no task-id dedup, so a second message would be a
// second crew run.
func TestCrewaiRouterRerouteReattachesNeverResends(t *testing.T) {
	f := newFakeCrewServer(t)
	f.holdStream = true
	f.completionsAfterGets = 2 // working, working, then the scripted state
	f.state = "completed"
	r := NewCrewaiRouter(nil, "")

	first, err := r.Route(context.Background(), f.endpoint(), crewTask("t-c1"))
	if err != nil {
		t.Fatalf("first Route: %v", err)
	}
	// The first stream is held open; the follower drops it without cancel is
	// NOT wanted here — close it after draining a frame, task stays in flight.
	go func() {
		<-first.Events()
	}()

	second, err := r.Route(context.Background(), f.endpoint(), crewTask("t-c1"))
	if err != nil {
		t.Fatalf("re-route: %v", err)
	}
	evs := collectEvents(t, second)
	if len(evs) == 0 {
		t.Fatal("reattached stream produced no events")
	}
	// Continuation seq: the reattach continues the task's cursor — first
	// stream emitted at least one event, so the second starts after it.
	if evs[0].Seq < 2 {
		t.Fatalf("reattach first seq = %d, want continuation ≥2", evs[0].Seq)
	}
	last := second.Status()
	if last.State != a2a.TaskCompleted {
		t.Fatalf("reattached terminal = %v, want completed via tasks/get", last.State)
	}
	if n := f.submissionCount(); n != 1 {
		t.Fatalf("submissions = %d, want exactly 1 — re-route must reattach, never re-send (C1)", n)
	}
	_ = first.Close()
}

// C1: a re-route on a task this router saw terminal replays the terminal
// status without any crew round-trip.
func TestCrewaiRouterTerminalRerouteReplaysStatus(t *testing.T) {
	f := newFakeCrewServer(t)
	r := NewCrewaiRouter(nil, "")
	s, err := r.Route(context.Background(), f.endpoint(), crewTask("t-done"))
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	collectEvents(t, s)

	again, err := r.Route(context.Background(), f.endpoint(), crewTask("t-done"))
	if err != nil {
		t.Fatalf("terminal re-route: %v", err)
	}
	evs := collectEvents(t, again)
	if len(evs) != 1 || evs[0].Type != a2a.EventStatus {
		t.Fatalf("replay events = %+v, want a single terminal status", summarize(evs))
	}
	if evs[0].Payload.(a2a.StatusPayload).State != a2a.TaskCompleted {
		t.Fatalf("replay state = %v, want completed", evs[0].Payload.(a2a.StatusPayload).State)
	}
	if n := f.submissionCount(); n != 1 {
		t.Fatalf("submissions = %d, want 1 — terminal re-route replays, never re-sends", n)
	}
}

// V4 mapping at this seam: Close on a still-live task issues the best-effort
// tasks/cancel, mirroring the HTTP router's cancel-on-transport-tear.
func TestCrewaiRouterCloseCancelsInFlightTask(t *testing.T) {
	f := newFakeCrewServer(t)
	f.holdStream = true
	r := NewCrewaiRouter(nil, "")

	s, err := r.Route(context.Background(), f.endpoint(), crewTask("t-cancel"))
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	<-s.Events() // wait for the first frame so googleID is adopted
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		calls := f.cancelCalls
		f.mu.Unlock()
		if calls == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Close never issued tasks/cancel")
}

// The non-streaming fallback: a crew that answers message/stream with -32004
// gets message/send + tasks/get polling instead.
func TestCrewaiRouterFallsBackToSendAndPoll(t *testing.T) {
	f := newFakeCrewServer(t)
	f.streamReject = true
	f.completionsAfterGets = 1 // one working poll, then the scripted state
	f.state = "completed"
	r := NewCrewaiRouter(nil, "")

	s, err := r.Route(context.Background(), f.endpoint(), crewTask("t-fb"))
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	evs := collectEvents(t, s)
	if len(evs) == 0 {
		t.Fatal("fallback stream produced no events")
	}
	if last := s.Status().State; last != a2a.TaskCompleted {
		t.Fatalf("fallback terminal = %v, want completed via poll", last)
	}
	f.mu.Lock()
	n, msgs := f.submissions, f.submits
	f.mu.Unlock()
	if n != 1 {
		t.Fatalf("submissions = %d, want 1 (the message/send fallback)", n)
	}
	if msgs[0].MessageID != "t-fb" {
		t.Fatalf("fallback messageId = %q", msgs[0].MessageID)
	}
}

func TestCrewaiRouterMapsFailureTerminal(t *testing.T) {
	// A server whose terminal frame is failed with a message: the reason is
	// lifted (bounded) and the task settles failed.
	r := NewCrewaiRouter(nil, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		mk := func(result any) {
			fmt.Fprintf(w, "data: %s\n\n", mustJSON(crewWireEnvelope{ID: 1, Result: mustJSON(result)}))
			fl.Flush()
		}
		mk(crewWireTask{Kind: "task", ID: "g-9", ContextID: "ctx-9", Status: crewWireStatus{State: "working"}})
		mk(map[string]any{
			"kind": "status-update", "taskId": "g-9", "contextId": "ctx-9", "final": true,
			"status": map[string]any{"state": "failed",
				"message": map[string]any{"role": "agent",
					"parts": []map[string]any{{"kind": "text", "text": "tool exploded"}}}},
		})
	}))
	defer srv.Close()

	s, err := r.Route(context.Background(), srv.URL, crewTask("t-fail"))
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	collectEvents(t, s)
	st := s.Status()
	if st.State != a2a.TaskFailed {
		t.Fatalf("terminal = %v, want failed", st.State)
	}
	if !strings.Contains(st.Reason, "tool exploded") {
		t.Fatalf("reason = %q, want the failure text lifted", st.Reason)
	}
}

func TestCrewaiRouterSendsBearerToken(t *testing.T) {
	f := newFakeCrewServer(t)
	r := NewCrewaiRouter(nil, "s3cret")

	s, err := r.Route(context.Background(), f.endpoint(), crewTask("t-auth"))
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	collectEvents(t, s)
	f.mu.Lock()
	hdrs := f.authHeaders
	f.mu.Unlock()
	if len(hdrs) == 0 || hdrs[0] != "Bearer s3cret" {
		t.Fatalf("Authorization headers = %v, want Bearer s3cret", hdrs)
	}
}

func TestCrewaiRouterRejectsBadInputs(t *testing.T) {
	r := NewCrewaiRouter(nil, "")
	if _, err := r.Route(context.Background(), "", crewTask("t")); err == nil {
		t.Fatal("empty endpoint must fail")
	}
	if _, err := r.Route(context.Background(), "http://x", a2a.Task{}); err == nil {
		t.Fatal("empty a2a_task_id must fail")
	}
}

func TestCrewMapState(t *testing.T) {
	cases := []struct {
		in   string
		want a2a.TaskState
	}{
		{"submitted", a2a.TaskSubmitted},
		{"working", a2a.TaskWorking},
		{"input-required", a2a.TaskInputRequired},
		{"auth-required", a2a.TaskAuthRequired},
		{"completed", a2a.TaskCompleted},
		{"failed", a2a.TaskFailed},
		{"canceled", a2a.TaskCanceled},
		{"cancelled", a2a.TaskCanceled},
		{"rejected", a2a.TaskFailed},
		{"task-state/working", a2a.TaskWorking},
		{"TASK_STATE_COMPLETED", a2a.TaskCompleted},
		{"TASK_STATE_INPUT_REQUIRED", a2a.TaskInputRequired},
		{"", a2a.TaskSubmitted},
		{"unknown", a2a.TaskSubmitted},
		{"mystery-state", a2a.TaskWorking},
	}
	for _, c := range cases {
		got, _ := crewMapState(c.in)
		if got != c.want {
			t.Errorf("crewMapState(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
