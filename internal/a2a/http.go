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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	wire "github.com/K8squad/K8squad/pkg/a2a"
)

// HTTPTransport is the sandbox-topology A2A transport (ISI-4188 gap 6): it
// POSTs the Task to the bound sandbox pod's in-pod supervisor
// (cmd/shim supervisor, `POST /task`) and speaks the identical JSONL event
// framing the stdio wire uses — the supervisor's /task handler streams the
// same wire.Event sequence over the response body. This is the topology where
// the runtime CLI actually lives: the sandbox image carries the shim + the
// runtime (opencode/codex/…), while the operator image carries the shim alone
// and cannot exec the CLI itself (the stdio topology's structural limit).
//
// Auth/transport security rides the layer below: the supervisor port is only
// reachable from the control plane via the team namespace's default-deny
// NetworkPolicy carve-out (control-plane :8080), and the pod is single-Run.
type HTTPTransport struct {
	// URL resolves the task's target endpoint (e.g. http://<podIP>:8080/task)
	// from the wire.Task — the caller (rundrive dispatch) maps the Run's bound
	// sandboxRef onto a pod IP. It is called once per Submit so a re-drive
	// re-resolves (the sandbox may have been rebound between laps).
	URL func(ctx context.Context, t wire.Task) (string, error)
	// Client, when set, is the HTTP client used for the POST. Nil uses a
	// default client with NO timeout — the response body is the task's live
	// event stream and stays open until the task settles; cancellation rides
	// the Submit context.
	Client *http.Client
}

// Submit POSTs the task to the supervisor and returns a Session tailing the
// NDJSON event stream. Idempotency (C1) is enforced server-side: the
// supervisor's engine dedups on a2a_task_id (a re-submit reattaches, never
// starts a second execution) and 409s only on a genuinely different in-flight
// task.
func (t *HTTPTransport) Submit(ctx context.Context, task wire.Task) (Session, error) {
	if t.URL == nil {
		return nil, fmt.Errorf("a2a: HTTPTransport requires a URL resolver")
	}
	if task.A2ATaskID == "" {
		return nil, fmt.Errorf("a2a: HTTPTransport.Submit requires a non-empty a2a_task_id")
	}
	url, err := t.URL(ctx, task)
	if err != nil {
		return nil, fmt.Errorf("a2a: resolve supervisor URL for task %s: %w", task.A2ATaskID, err)
	}
	payload, err := json.Marshal(task)
	if err != nil {
		return nil, fmt.Errorf("a2a: marshal task %s: %w", task.A2ATaskID, err)
	}
	// The request context deliberately does NOT die with Submit's caller: the
	// dispatcher follows the stream on a background context (WithoutCancel at
	// the Dispatcher.Submit seam), so the POST inherits that lifetime. Session
	// teardown (Cancel/Close) cancels the derived ctx, which aborts the
	// in-flight request and — server-side — cancels the supervisor's engine
	// run, whose context is the request context.
	reqCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("a2a: build POST %s: %w", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	hc := t.Client
	if hc == nil {
		hc = &http.Client{}
	}
	resp, err := hc.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("a2a: POST %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("a2a: POST %s: supervisor answered %s: %s", url, resp.Status, string(body))
	}

	sess := &httpSession{
		taskID: task.A2ATaskID,
		body:   resp.Body,
		cancel: cancel,
		events: make(chan wire.Event),
		quit:   make(chan struct{}),
	}
	go sess.pump(resp.Body)
	return sess, nil
}

// httpSession tails a supervisor task's NDJSON response stream.
type httpSession struct {
	taskID string
	body   io.ReadCloser
	cancel context.CancelFunc
	events chan wire.Event
	quit   chan struct{}

	closeOnce sync.Once

	mu      sync.Mutex
	status  wire.Status
	settled bool
}

// pump decodes JSONL events from the response body, forwards them, and
// settles the session's status from the last status event when the stream
// ends (the supervisor closes the body at the task's terminal state). It
// always closes the events channel exactly once.
func (s *httpSession) pump(r io.Reader) {
	defer close(s.events)
	dec := json.NewDecoder(r)
	var last wire.Status
	for {
		var ev wire.Event
		if err := dec.Decode(&ev); err != nil {
			break // stream end (terminal), a torn connection, or a malformed tail
		}
		last.LastSeq = ev.Seq
		if ev.Type == wire.EventStatus {
			if sp, ok := statusPayload(ev.Payload); ok {
				last.State = sp.State
				last.Reason = sp.Reason
			}
		}
		select {
		case s.events <- ev:
		case <-s.quit:
			// The follower dropped us (ctx canceled / sink error). Stop pumping;
			// Close tears the request down.
			return
		}
	}
	s.mu.Lock()
	s.status = last
	s.settled = true
	s.mu.Unlock()
}

func (s *httpSession) Events() <-chan wire.Event { return s.events }

func (s *httpSession) Status() wire.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Cancel aborts the in-flight POST. The supervisor runs the engine on the
// request context, so tearing the connection down cancels the live task
// server-side — there is no separate cancel RPC on the /task wire (C8:
// idempotent no-op on an already-terminal task).
func (s *httpSession) Cancel(ctx context.Context, reason string) error {
	return s.Close()
}

// Close signals the pump to stop and aborts the HTTP request. Idempotent.
func (s *httpSession) Close() error {
	s.closeOnce.Do(func() {
		close(s.quit)
		if s.cancel != nil {
			s.cancel()
		}
		_ = s.body.Close()
	})
	return nil
}
