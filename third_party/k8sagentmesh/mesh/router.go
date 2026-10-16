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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/K8sAgentMesh/K8sAgentMesh/a2a"
)

// Router delivers one task to a standing process and returns a Stream tailing
// its event sequence. It is the transport half of RuntimeAdapter.Route, factored
// out so the delivery mechanism (HTTP to the shim supervisor, a pod-proxy, an
// in-process bridge) is pluggable without touching the lifecycle logic.
type Router interface {
	// Route POSTs the task to the process serving at endpoint and returns a
	// Stream of its events. endpoint is the A2A base URL (Handle.Endpoint());
	// the task surface is appended by the router.
	Route(ctx context.Context, endpoint string, task a2a.Task) (Stream, error)
}

// httpRouter is the default Router: it POSTs the Task to the process's
// `POST /task` surface — the same JSONL event wire the shim supervisor serves
// (cmd/shim/supervisor.go) and the `shim run` stdio path speaks — and tails the
// NDJSON response as the task's live event stream. It stays mesh-local (rather
// than reusing a k8squad-internal transport) so this [generic] seam depends only
// on the A2A wire types, keeping roadmap-P3 extraction clean.
type httpRouter struct {
	client *http.Client
}

// NewHTTPRouter returns the default HTTP /task router. A nil client uses a
// default client with NO timeout: the response body is the task's live event
// stream and stays open until the task settles, so cancellation rides the Route
// context, not a client deadline.
func NewHTTPRouter(client *http.Client) Router {
	if client == nil {
		client = &http.Client{}
	}
	return &httpRouter{client: client}
}

// Route POSTs the task and returns a Stream over the NDJSON response. Delivery
// idempotency (C1) is enforced server-side by the supervisor's engine (it dedups
// on a2a_task_id), so a re-route reattaches rather than double-executes.
func (r *httpRouter) Route(ctx context.Context, endpoint string, task a2a.Task) (Stream, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("mesh: router requires a non-empty endpoint")
	}
	url := strings.TrimRight(endpoint, "/") + "/task"
	payload, err := json.Marshal(task)
	if err != nil {
		return nil, fmt.Errorf("mesh: marshal task %s: %w", task.A2ATaskID, err)
	}
	// The request context is derived so Close() aborts the in-flight POST, which
	// — server-side — cancels the supervisor's engine run (its run context is the
	// request context). The caller's ctx governs the submit itself.
	reqCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("mesh: build POST %s: %w", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("mesh: POST %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("mesh: POST %s: process answered %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	s := &httpStream{
		body:   resp.Body,
		cancel: cancel,
		events: make(chan a2a.Event),
		quit:   make(chan struct{}),
	}
	go s.pump()
	return s, nil
}

// httpStream tails a process's NDJSON `/task` response stream as a Stream.
type httpStream struct {
	body   io.ReadCloser
	cancel context.CancelFunc
	events chan a2a.Event
	quit   chan struct{}

	closeOnce sync.Once

	mu      sync.Mutex
	status  a2a.Status
	settled bool
}

// pump decodes JSONL events from the body, forwards them, and settles the
// status from the last status event when the stream ends (the process closes
// the body at the task's terminal state). It closes the events channel exactly
// once.
func (s *httpStream) pump() {
	defer close(s.events)
	dec := json.NewDecoder(s.body)
	var last a2a.Status
	for {
		var ev a2a.Event
		if err := dec.Decode(&ev); err != nil {
			break // stream end (terminal), a torn connection, or a malformed tail
		}
		last.LastSeq = ev.Seq
		if ev.Type == a2a.EventStatus {
			if sp, ok := statusPayload(ev.Payload); ok {
				last.State = sp.State
				last.Reason = sp.Reason
				if sp.TraceID != "" {
					last.TraceID = sp.TraceID
				}
			}
		}
		select {
		case s.events <- ev:
		case <-s.quit:
			return // the follower dropped us; Close tears the request down
		}
	}
	s.mu.Lock()
	s.status = last
	s.settled = true
	s.mu.Unlock()
}

func (s *httpStream) Events() <-chan a2a.Event { return s.events }

func (s *httpStream) Status() a2a.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Close signals the pump to stop and aborts the HTTP request. Idempotent.
func (s *httpStream) Close() error {
	s.closeOnce.Do(func() {
		close(s.quit)
		if s.cancel != nil {
			s.cancel()
		}
		_ = s.body.Close()
	})
	return nil
}

// statusPayload recovers a StatusPayload from a decoded event payload, which —
// after a JSON round-trip through the wire — is a map[string]any, not the
// concrete type. It re-marshals and decodes into the typed payload; a shape it
// cannot read yields ok=false and the status is left unchanged.
func statusPayload(payload any) (a2a.StatusPayload, bool) {
	if sp, ok := payload.(a2a.StatusPayload); ok {
		return sp, true
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return a2a.StatusPayload{}, false
	}
	var sp a2a.StatusPayload
	if err := json.Unmarshal(raw, &sp); err != nil {
		return a2a.StatusPayload{}, false
	}
	return sp, true
}

var _ Stream = (*httpStream)(nil)
