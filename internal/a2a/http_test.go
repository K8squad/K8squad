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

package a2a

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	wire "github.com/K8squad/K8squad/pkg/a2a"
)

// TestHTTPTransportStreamsSupervisorEvents (ISI-4188 gap 6): the transport
// POSTs the task to the supervisor's /task and tails the NDJSON wire.Event
// stream, settling Status from the last status event — the same contract the
// stdio transport fulfills against `shim run`.
func TestHTTPTransportStreamsSupervisorEvents(t *testing.T) {
	var gotTask wire.Task
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/task" || r.Method != http.MethodPost {
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&gotTask); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		enc := json.NewEncoder(w)
		events := []wire.Event{
			{Seq: 1, A2ATaskID: gotTask.A2ATaskID, Type: wire.EventStatus, Payload: map[string]any{"state": "submitted"}},
			{Seq: 2, A2ATaskID: gotTask.A2ATaskID, Type: wire.EventStatus, Payload: map[string]any{"state": "working"}},
			{Seq: 3, A2ATaskID: gotTask.A2ATaskID, Type: wire.EventTool, Payload: map[string]any{"name": "read", "phase": "result", "ok": true}},
			{Seq: 4, A2ATaskID: gotTask.A2ATaskID, Type: wire.EventStatus, Payload: map[string]any{"state": "completed"}},
		}
		for _, ev := range events {
			if err := enc.Encode(ev); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	tr := &HTTPTransport{URL: func(context.Context, wire.Task) (string, error) { return srv.URL + "/task", nil }}
	sess, err := tr.Submit(context.Background(), wire.Task{
		A2ATaskID:  "run-1",
		WorkItemID: "wi-1",
		Envelope:   wire.Envelope{Input: "read /etc/hostname"},
		ModelRoute: wire.ModelRoute{Endpoint: "http://ollama:11434/v1", Model: "qwen3.8:latest"},
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// The task envelope crosses the wire intact (model_route included — the
	// pod-side engine renders its provider block from it).
	if gotTask.A2ATaskID != "run-1" || gotTask.ModelRoute.Model != "qwen3.8:latest" {
		t.Fatalf("task mangled in transit: %+v", gotTask)
	}

	var kinds []wire.EventType
	for ev := range sess.Events() {
		kinds = append(kinds, ev.Type)
	}
	if len(kinds) != 4 || kinds[2] != wire.EventTool {
		t.Fatalf("event stream mismatch: %v", kinds)
	}
	st := sess.Status()
	if st.State != "completed" || st.LastSeq != 4 {
		t.Fatalf("terminal status = %+v, want completed@4", st)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestHTTPTransportNon200: a supervisor refusal (409 busy, 500 no runtime)
// is a loud Submit error so the drive loop re-drives instead of tailing an
// error page as an event stream.
func TestHTTPTransportNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "task in flight (one pod = one run)", http.StatusConflict)
	}))
	defer srv.Close()

	tr := &HTTPTransport{URL: func(context.Context, wire.Task) (string, error) { return srv.URL + "/task", nil }}
	_, err := tr.Submit(context.Background(), wire.Task{A2ATaskID: "run-1"})
	if err == nil {
		t.Fatal("expected error on 409")
	}
}

// TestHTTPTransportRequiresResolverAndID mirrors the StdioTransport guards.
func TestHTTPTransportRequiresResolverAndID(t *testing.T) {
	if _, err := (&HTTPTransport{}).Submit(context.Background(), wire.Task{A2ATaskID: "x"}); err == nil {
		t.Fatal("expected error without URL resolver")
	}
	tr := &HTTPTransport{URL: func(context.Context, wire.Task) (string, error) { return "http://x/task", nil }}
	if _, err := tr.Submit(context.Background(), wire.Task{}); err == nil {
		t.Fatal("expected error on empty a2a_task_id")
	}
}
