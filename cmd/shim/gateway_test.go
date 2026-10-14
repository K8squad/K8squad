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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/mesh"
)

// fakeGwAdapter is a scripted RuntimeAdapter for the HTTP-edge tests: each
// routed task streams one synthetic event and settles — immediately when
// autoSettle, or only when Release is called (hold mode) so a test can keep
// a process observably busy.
type fakeGwAdapter struct {
	mu         sync.Mutex
	handles    []*fakeGwHandle
	autoSettle bool
	streams    []*fakeGwStream
}

type fakeGwHandle struct {
	id string
	ep string
}

func (h *fakeGwHandle) ID() string       { return h.id }
func (h *fakeGwHandle) Endpoint() string { return h.ep }

func (f *fakeGwAdapter) Spawn(_ context.Context, env mesh.Envelope) (mesh.Handle, a2a.AgentCard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := &fakeGwHandle{
		id: fmt.Sprintf("fake-%d", len(f.handles)),
		ep: fmt.Sprintf("http://127.0.0.1:%d", 9100+len(f.handles)),
	}
	f.handles = append(f.handles, h)
	return h, a2a.AgentCard{Agent: env.Identity}, nil
}

func (f *fakeGwAdapter) Register(_ context.Context, h mesh.Handle) (a2a.AgentCard, error) {
	return a2a.AgentCard{}, nil
}

func (f *fakeGwAdapter) Route(_ context.Context, h mesh.Handle, task a2a.Task) (mesh.Stream, error) {
	f.mu.Lock()
	s := &fakeGwStream{release: make(chan struct{})}
	f.streams = append(f.streams, s)
	auto := f.autoSettle
	f.mu.Unlock()
	if auto {
		s.Close()
	}
	return s, nil
}

// Release settles every stream this adapter routed that is still held.
func (f *fakeGwAdapter) Release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.streams {
		s.Close()
	}
}

func (f *fakeGwAdapter) Hibernate(context.Context, mesh.Handle) error { return nil }
func (f *fakeGwAdapter) Wake(context.Context, mesh.Handle) error      { return nil }
func (f *fakeGwAdapter) Stop(context.Context, mesh.Handle) error      { return nil }
func (f *fakeGwAdapter) Recreate(ctx context.Context, h mesh.Handle, env mesh.Envelope) (mesh.Handle, error) {
	return h, nil
}

// fakeGwStream streams one synthetic event and settles when released —
// via Close (the Stream contract: Close unblocks Events) or auto-settle.
type fakeGwStream struct {
	release   chan struct{}
	closeOnce sync.Once
}

func (s *fakeGwStream) Events() <-chan a2a.Event {
	ch := make(chan a2a.Event, 1)
	ch <- a2a.Event{Seq: 1, Type: a2a.EventStatus}
	go func() {
		<-s.release
		close(ch)
	}()
	return ch
}

func (s *fakeGwStream) Close() error {
	s.closeOnce.Do(func() { close(s.release) })
	return nil
}

func (s *fakeGwStream) Status() a2a.Status { return a2a.Status{State: a2a.TaskCompleted} }

var _ mesh.RuntimeAdapter = (*fakeGwAdapter)(nil)

// newTestGateway builds a started gateway over the fake adapter plus the
// HTTP mux the runGateway serves, on an httptest server.
func newTestGateway(t *testing.T, min int) (*httptest.Server, *mesh.Gateway, *fakeGwAdapter) {
	t.Helper()
	fa := &fakeGwAdapter{autoSettle: true}
	gw, err := mesh.NewGateway(mesh.GatewayConfig{
		Adapter:  fa,
		Envelope: mesh.Envelope{RuntimeType: "opencode"},
		Min:      min,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gw.Stop(context.Background()) })

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /ready", handleGatewayReady(gw))
	mux.HandleFunc("GET /endpoints", handleGatewayEndpoints(gw))
	mux.HandleFunc("POST /task", handleGatewayTask(gw))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, gw, fa
}

// /ready reports the pool: ready with the process count once >= 1 child.
func TestGatewayHTTP_Ready(t *testing.T) {
	srv, _, _ := newTestGateway(t, 2)
	resp, err := http.Get(srv.URL + "/ready")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/ready = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Serving  bool `json:"serving"`
		Ready    bool `json:"ready"`
		Idle     int  `json:"idle"`
		Processes int  `json:"processes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Ready || body.Processes != 2 || body.Idle != 2 {
		t.Fatalf("ready body = %+v, want ready pool of 2 idle", body)
	}
}

// /endpoints serves the live endpoint set.
func TestGatewayHTTP_Endpoints(t *testing.T) {
	srv, _, _ := newTestGateway(t, 1)
	resp, err := http.Get(srv.URL + "/endpoints")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Processes []struct {
			EndpointID string `json:"EndpointID"`
			Endpoint   string `json:"Endpoint"`
			Busy       bool   `json:"Busy"`
		} `json:"processes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Processes) != 1 || body.Processes[0].EndpointID != "fake-0" {
		t.Fatalf("endpoints body = %+v", body)
	}
}

// POST /task streams the routed task's NDJSON events; a second POST naming
// the SAME busy target gets 409 — the gateway-local one-process-one-task
// guard (D3.3 #3) surfaced on the wire.
func TestGatewayHTTP_TaskRouteAndBusy409(t *testing.T) {
	// Hold mode: the routed stream stays live until released, keeping the
	// process observably busy across requests.
	fa := &fakeGwAdapter{autoSettle: false}
	gw, err := mesh.NewGateway(mesh.GatewayConfig{
		Adapter:  fa,
		Envelope: mesh.Envelope{RuntimeType: "opencode"},
		Min:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gw.Stop(context.Background()) })
	mux := http.NewServeMux()
	mux.HandleFunc("GET /endpoints", handleGatewayEndpoints(gw))
	mux.HandleFunc("POST /task", handleGatewayTask(gw))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// A task whose stream we hold open: the handler stays in its event
	// loop, the process stays busy.
	first, err := http.Post(srv.URL+"/task", "application/json", strings.NewReader(`{"a2a_task_id":"t-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Body.Close()

	// The busy flag flips as soon as the route is in flight.
	deadline := time.Now().Add(2 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		resp, err := http.Get(srv.URL + "/endpoints")
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Processes []struct {
				Busy bool `json:"Busy"`
			} `json:"processes"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if len(body.Processes) == 1 && body.Processes[0].Busy {
			ready = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !ready {
		t.Fatal("process never flipped busy while the routed stream was live")
	}

	second, err := http.Post(srv.URL+"/task?target=fake-0", "application/json",
		strings.NewReader(`{"a2a_task_id":"t-2"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Body.Close()
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("busy target POST = %d, want 409", second.StatusCode)
	}

	// Settle the held stream: the process releases and routes again.
	fa.Release()
	third, err := http.Post(srv.URL+"/task", "application/json",
		strings.NewReader(`{"a2a_task_id":"t-3"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer third.Body.Close()
	if third.StatusCode != http.StatusOK {
		t.Fatalf("POST after settle = %d, want 200", third.StatusCode)
	}
}

// A task whose stream already settled routes again cleanly — the busy flag
// releases at stream settle (no stuck-idle-pool-of-one regression).
func TestGatewayHTTP_TaskSettlesAndReRoutes(t *testing.T) {
	srv, _, _ := newTestGateway(t, 1)
	for i := 0; i < 3; i++ {
		resp, err := http.Post(srv.URL+"/task", "application/json",
			strings.NewReader(fmt.Sprintf(`{"a2a_task_id":"t-%d"}`, i)))
		if err != nil {
			t.Fatal(err)
		}
		// Drain the whole NDJSON body so the stream settles before the next lap.
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				break
			}
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("lap %d POST = %d, want 200", i, resp.StatusCode)
		}
		if !strings.Contains(sb.String(), `"seq"`) {
			t.Fatalf("lap %d body missing streamed event: %q", i, sb.String())
		}
	}
}

// buildGateway is fail-closed on the pool identity: no team/role, no gateway.
func TestBuildGatewayRequiresPoolIdentity(t *testing.T) {
	t.Setenv("KSQUAD_MESH_TEAM", "")
	t.Setenv("KSQUAD_MESH_ROLE", "")
	if _, err := buildGateway(); err == nil {
		t.Fatal("expected pool identity validation error")
	}
}
