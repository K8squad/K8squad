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

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	clienta2a "github.com/K8squad/K8squad/internal/a2a"
	"github.com/K8squad/K8squad/pkg/a2a"
)

// TestStreamRunEventsReasoningOnlyFollowStaysAlive is the ISI-7694 regression
// check: a reasoning model streams only delta.reasoning_content
// (delta.content=null) during its thinking phase, so the engine emits ZERO
// run_events for the whole phase. The supervisor must (1) flush the 200 headers
// before the first event so the operator's bounded response-header wait opens
// the follow immediately, and (2) keep the NDJSON body non-idle during the
// silent reasoning phase. Together those keep the operator's a2a follow alive
// instead of false-killing it as a follow_error (parent ISI-7686 → releaseDead
// zombie_no_sandbox).
//
// The check drives the REAL operator follow path (clienta2a.HTTPTransport +
// Client.Dispatch, i.e. Begin+Follow) against a server that runs the production
// streamRunEvents over a channel that is run-event-SILENT for far longer than
// the operator's response-header timeout before the terminal event arrives.
// With the old lazy-flush `for ev := range events` loop the 200 would sit
// buffered until the first event, the operator's POST would time out waiting for
// headers, and Dispatch would fail — so this test fails against the pre-fix code.
func TestStreamRunEventsReasoningOnlyFollowStaysAlive(t *testing.T) {
	const (
		keepalive      = 15 * time.Millisecond
		reasoningPhase = 200 * time.Millisecond // run-event-silent "thinking" gap
		headerTimeout  = 40 * time.Millisecond  // << reasoningPhase
	)

	events := make(chan a2a.Event)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_ = streamRunEvents(r.Context(), w, events, keepalive)
	}))
	defer srv.Close()

	// Producer: model enters a long reasoning_content phase (content=null → no
	// run_events) and only then streams its first assistant output and settles.
	go func() {
		time.Sleep(reasoningPhase)
		events <- a2a.Event{Seq: 1, A2ATaskID: "run-7694", Type: a2a.EventStatus, Payload: map[string]any{"state": "working"}}
		events <- a2a.Event{Seq: 2, A2ATaskID: "run-7694", Type: a2a.EventStatus, Payload: map[string]any{"state": "completed"}}
		close(events)
	}()

	tr := &clienta2a.HTTPTransport{
		URL:    func(context.Context, a2a.Task) (string, error) { return srv.URL + "/task", nil },
		Client: &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: headerTimeout}},
	}
	client := clienta2a.New(tr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := client.Dispatch(ctx, a2a.Task{A2ATaskID: "run-7694"}, nil)
	if err != nil {
		t.Fatalf("follow died on a reasoning-only stream (the follow_error zombie): %v", err)
	}
	if res.Status.State != a2a.TaskCompleted {
		t.Fatalf("follow did not settle Completed: got state %q", res.Status.State)
	}
}

// flushRecorder is a streaming-aware http.ResponseWriter that records, under a
// lock, the byte stream and the point at which the first Flush happened relative
// to the first body write — the properties a plain httptest.ResponseRecorder
// cannot observe because it does not model flush ordering.
type flushRecorder struct {
	mu            sync.Mutex
	header        http.Header
	buf           []byte
	flushes       int
	headerFlushed bool // Flush happened before any body byte was written
	wroteBody     bool
}

func newFlushRecorder() *flushRecorder { return &flushRecorder{header: make(http.Header)} }

func (f *flushRecorder) Header() http.Header { return f.header }

func (f *flushRecorder) WriteHeader(int) {}

func (f *flushRecorder) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(p) > 0 {
		f.wroteBody = true
	}
	f.buf = append(f.buf, p...)
	return len(p), nil
}

func (f *flushRecorder) Flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushes++
	if !f.wroteBody && f.flushes == 1 {
		f.headerFlushed = true
	}
}

func (f *flushRecorder) snapshot() (body string, flushes int, headerFirst bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.buf), f.flushes, f.headerFlushed
}

// TestStreamRunEventsFlushesHeadersAndKeepsAlive asserts the two runtime-agnostic
// properties of streamRunEvents directly: the headers are flushed BEFORE the
// first event, and a run-event-silent gap longer than the keepalive interval
// emits whitespace keepalive bytes — the newline the operator's json.Decoder
// skips, so the follow stays open with no spurious event.
func TestStreamRunEventsFlushesHeadersAndKeepsAlive(t *testing.T) {
	const keepalive = 10 * time.Millisecond

	rec := newFlushRecorder()
	events := make(chan a2a.Event)

	done := make(chan struct{})
	go func() {
		_ = streamRunEvents(context.Background(), rec, events, keepalive)
		close(done)
	}()

	// Reasoning phase: stay run-event-silent long enough for several keepalives.
	time.Sleep(80 * time.Millisecond)

	body, flushes, headerFirst := rec.snapshot()
	if !headerFirst {
		t.Fatalf("headers were not flushed before the first event (operator would time out the POST): flushes=%d", flushes)
	}
	// At least one keepalive newline must have flowed (the exact count is timing
	// dependent; require >= 1 so bytes demonstrably moved).
	keepaliveBytes := 0
	for _, b := range body {
		if b == '\n' {
			keepaliveBytes++
		}
	}
	if keepaliveBytes == 0 {
		t.Fatalf("expected >=1 keepalive newline during the silent phase, got body=%q", body)
	}

	// Now settle: the terminal event must still encode and close the stream.
	events <- a2a.Event{Seq: 1, A2ATaskID: "run-7694", Type: a2a.EventStatus, Payload: map[string]any{"state": "completed"}}
	close(events)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("streamRunEvents did not return after the channel closed")
	}
}
