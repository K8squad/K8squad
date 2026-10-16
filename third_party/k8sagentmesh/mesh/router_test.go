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
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/K8sAgentMesh/K8sAgentMesh/a2a"
)

// TestHTTPRouterStreamsTaskEvents proves the default router speaks the shim
// supervisor's `POST /task` NDJSON wire: it POSTs the task to <endpoint>/task
// and tails the event sequence, settling the terminal status.
func TestHTTPRouterStreamsTaskEvents(t *testing.T) {
	var gotTask a2a.Task
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/task" {
			t.Errorf("router POSTed to %q, want /task", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotTask)

		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		enc := json.NewEncoder(w)
		events := []a2a.Event{
			{Seq: 1, A2ATaskID: "run-7", Type: a2a.EventMessage, Payload: a2a.MessagePayload{Role: "agent", Text: "hi"}},
			{Seq: 2, A2ATaskID: "run-7", Type: a2a.EventStatus, Payload: a2a.StatusPayload{State: a2a.TaskState("completed"), TraceID: "abc"}},
		}
		for _, ev := range events {
			_ = enc.Encode(ev)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	router := NewHTTPRouter(srv.Client())
	stream, err := router.Route(context.Background(), srv.URL, a2a.Task{A2ATaskID: "run-7"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	defer stream.Close()

	var got []a2a.Event
	for ev := range stream.Events() {
		got = append(got, ev)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if gotTask.A2ATaskID != "run-7" {
		t.Fatalf("server received task %q, want run-7", gotTask.A2ATaskID)
	}
	st := stream.Status()
	if string(st.State) != "completed" {
		t.Fatalf("settled state = %q, want completed", st.State)
	}
	if st.LastSeq != 2 {
		t.Fatalf("settled LastSeq = %d, want 2", st.LastSeq)
	}
	if st.TraceID != "abc" {
		t.Fatalf("settled TraceID = %q, want abc", st.TraceID)
	}
}

func TestHTTPRouterNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "task in flight (one pod = one run)", http.StatusConflict)
	}))
	defer srv.Close()

	router := NewHTTPRouter(srv.Client())
	_, err := router.Route(context.Background(), srv.URL, a2a.Task{A2ATaskID: "run-1"})
	if err == nil {
		t.Fatal("Route should fail on a non-200 supervisor answer")
	}
}

func TestHTTPRouterEmptyEndpoint(t *testing.T) {
	router := NewHTTPRouter(nil)
	if _, err := router.Route(context.Background(), "", a2a.Task{A2ATaskID: "x"}); err == nil {
		t.Fatal("Route should reject an empty endpoint")
	}
}

// TestHTTPRouterCloseAbortsStream proves Close tears the connection down and
// closes the events channel even while the server is mid-stream.
func TestHTTPRouterCloseAbortsStream(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		enc := json.NewEncoder(w)
		_ = enc.Encode(a2a.Event{Seq: 1, Type: a2a.EventMessage, Payload: a2a.MessagePayload{Text: "one"}})
		if flusher != nil {
			flusher.Flush()
		}
		<-release // hold the stream open until the test releases it
	}))
	defer srv.Close()
	defer close(release)

	router := NewHTTPRouter(srv.Client())
	stream, err := router.Route(context.Background(), srv.URL, a2a.Task{A2ATaskID: "run-1"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	// Drain the first event, then close mid-stream.
	select {
	case <-stream.Events():
	case <-time.After(2 * time.Second):
		t.Fatal("first event never arrived")
	}
	_ = stream.Close()

	// The events channel must close promptly after Close.
	select {
	case _, ok := <-stream.Events():
		if ok {
			// drain any buffered event, then expect close
			<-stream.Events()
		}
	case <-time.After(2 * time.Second):
		t.Fatal("events channel did not close after Close")
	}
}
