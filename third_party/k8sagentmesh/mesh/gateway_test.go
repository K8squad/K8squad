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

package mesh_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/K8sAgentMesh/K8sAgentMesh/a2a"
	"github.com/K8sAgentMesh/K8sAgentMesh/mesh"
)

// fakeAdapter is a scripted RuntimeAdapter: in-memory handles, controllable
// event streams, recorded ops. It stands in for opencode so the gateway's
// generic logic is tested against the SEAM, not a runtime.
type fakeAdapter struct {
	mu sync.Mutex
	// spawnErr fails the next Spawn.
	spawnErr error
	// routeErr fails the next Route.
	routeErr error
	// streams per handle id: the stream the fake route returned.
	streams map[string]*fakeStream
	handles []*fakeHandle
	routed  []string
	stopped []string
}

type fakeHandle struct {
	id   string
	ep   string
	card a2a.AgentCard
}

func (h *fakeHandle) ID() string       { return h.id }
func (h *fakeHandle) Endpoint() string { return h.ep }

func newFakeAdapter() *fakeAdapter {
	return &fakeAdapter{streams: map[string]*fakeStream{}}
}

func (f *fakeAdapter) Spawn(_ context.Context, env mesh.Envelope) (mesh.Handle, a2a.AgentCard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.spawnErr != nil {
		return nil, a2a.AgentCard{}, f.spawnErr
	}
	h := &fakeHandle{
		id: fmt.Sprintf("fake-%d", len(f.handles)),
		ep: fmt.Sprintf("http://127.0.0.1:%d", 9000+len(f.handles)),
		card: a2a.AgentCard{
			Agent:  env.Identity,
			Skills: env.Skills,
		},
	}
	f.handles = append(f.handles, h)
	s := &fakeStream{done: make(chan struct{})}
	f.streams[h.id] = s
	return h, h.card, nil
}

func (f *fakeAdapter) Register(_ context.Context, h mesh.Handle) (a2a.AgentCard, error) {
	fh := h.(*fakeHandle)
	return fh.card, nil
}

func (f *fakeAdapter) Route(_ context.Context, h mesh.Handle, task a2a.Task) (mesh.Stream, error) {
	f.mu.Lock()
	if f.routeErr != nil {
		err := f.routeErr
		f.mu.Unlock()
		return nil, err
	}
	fh := h.(*fakeHandle)
	f.routed = append(f.routed, fh.id)
	fs := f.streams[fh.id]
	f.mu.Unlock()
	return fs, nil
}

// SettleHandle flips the stream of one routed handle to settled — the fake's
// equivalent of the process finishing its task.
func (f *fakeAdapter) SettleHandle(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.streams[id]; ok {
		s.settle()
	}
}

func (f *fakeAdapter) Hibernate(context.Context, mesh.Handle) error { return nil }
func (f *fakeAdapter) Wake(context.Context, mesh.Handle) error      { return nil }

func (f *fakeAdapter) Stop(_ context.Context, h mesh.Handle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, h.ID())
	return nil
}

func (f *fakeAdapter) Recreate(ctx context.Context, h mesh.Handle, env mesh.Envelope) (mesh.Handle, error) {
	_ = ctx
	_ = env
	return h, nil
}

// fakeStream is a Stream that streams nothing until its done channel closes.
// settle is idempotent and shared by Close so the Stream contract (Close
// unblocks Events) and explicit settles never double-close.
type fakeStream struct {
	done chan struct{}
	once sync.Once
}

func (s *fakeStream) settle() { s.once.Do(func() { close(s.done) }) }

func (s *fakeStream) Events() <-chan a2a.Event {
	ch := make(chan a2a.Event)
	go func() {
		defer close(ch)
		<-s.done
	}()
	return ch
}

func (s *fakeStream) Status() a2a.Status { return a2a.Status{State: a2a.TaskCompleted} }

func (s *fakeStream) Close() error {
	s.settle()
	return nil
}

var _ mesh.RuntimeAdapter = (*fakeAdapter)(nil)
var _ mesh.Stream = (*fakeStream)(nil)

func gatewayEnv() mesh.Envelope {
	return mesh.Envelope{
		RuntimeType: "opencode",
		Identity:    a2a.AgentIdentity{Name: "coder", Squad: "team-acme", Project: "p"},
		Skills:      []string{"go"},
	}
}

// Start spawns exactly Min children pre-loaded with the envelope, and Ready
// reflects the pool.
func TestGateway_StartSpawnsPool(t *testing.T) {
	fa := newFakeAdapter()
	gw, err := mesh.NewGateway(mesh.GatewayConfig{
		Adapter:  fa,
		Envelope: gatewayEnv(),
		Min:      2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !gw.Ready() {
		t.Fatal("gateway should be ready with a live pool")
	}
	procs := gw.Processes()
	if len(procs) != 2 {
		t.Fatalf("processes = %d, want 2", len(procs))
	}
	if procs[0].Card.Agent.Name != "coder" || procs[0].Endpoint == procs[1].Endpoint {
		t.Fatalf("envelope card / distinct endpoints broken: %+v", procs)
	}
}

// Min clamps to 1 — a zero-Min gateway must still hold one always-up child.
func TestGateway_MinClampsToOne(t *testing.T) {
	fa := newFakeAdapter()
	gw, _ := mesh.NewGateway(mesh.GatewayConfig{Adapter: fa, Envelope: gatewayEnv(), Min: 0})
	if err := gw.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(gw.Processes()); n != 1 {
		t.Fatalf("processes = %d, want 1 (clamped)", n)
	}
}

// Route to an empty target picks an idle process; while its stream is live
// the process is busy and a second route picks the OTHER process; when every
// process is busy the route refuses with ErrNoIdleProcess (D3.3 guard #3).
func TestGateway_RouteIdleSelectionAndBusyGuard(t *testing.T) {
	fa := newFakeAdapter()
	gw, _ := mesh.NewGateway(mesh.GatewayConfig{Adapter: fa, Envelope: gatewayEnv(), Min: 2})
	if err := gw.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	task := a2a.Task{A2ATaskID: "t-1"}

	s1, err := gw.Route(context.Background(), "", task)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := gw.Route(context.Background(), "", task)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gw.Route(context.Background(), "", task); !errors.Is(err, mesh.ErrNoIdleProcess) {
		t.Fatalf("third route err = %v, want ErrNoIdleProcess", err)
	}
	// Busy state is visible on the snapshot.
	for _, p := range gw.Processes() {
		if !p.Busy {
			t.Fatalf("process %s should be busy", p.EndpointID)
		}
	}
	// Settle stream 1: its process flips back to idle and routes again.
	fa.SettleHandle(fa.routed[0])
	<-s1.Events()
	<-s1.Events() // drained
	s1.Close()
	if _, err := gw.Route(context.Background(), "", task); err != nil {
		t.Fatalf("route after settle: %v", err)
	}
	s2.Close()
}

// Route to an explicit busy target refuses with ErrProcessBusy; an unknown
// target errors loudly.
func TestGateway_RouteTargetGuard(t *testing.T) {
	fa := newFakeAdapter()
	gw, _ := mesh.NewGateway(mesh.GatewayConfig{Adapter: fa, Envelope: gatewayEnv(), Min: 2})
	if err := gw.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	task := a2a.Task{A2ATaskID: "t-1"}
	if _, err := gw.Route(context.Background(), "fake-0", task); err != nil {
		t.Fatal(err)
	}
	if _, err := gw.Route(context.Background(), "fake-0", task); !errors.Is(err, mesh.ErrProcessBusy) {
		t.Fatalf("busy target err = %v, want ErrProcessBusy", err)
	}
	if _, err := gw.Route(context.Background(), "ghost", task); err == nil {
		t.Fatal("unknown target should error")
	}
}

// A failed Adapter.Route releases the process back to idle — the busy flag
// must never leak on a dispatch error.
func TestGateway_RouteErrorReleases(t *testing.T) {
	fa := newFakeAdapter()
	gw, _ := mesh.NewGateway(mesh.GatewayConfig{Adapter: fa, Envelope: gatewayEnv(), Min: 1})
	if err := gw.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	fa.mu.Lock()
	fa.routeErr = errors.New("boom")
	fa.mu.Unlock()
	if _, err := gw.Route(context.Background(), "", a2a.Task{A2ATaskID: "t-1"}); err == nil {
		t.Fatal("expected route error")
	}
	if p := gw.Processes()[0]; p.Busy {
		t.Fatal("process stayed busy after a failed route")
	}
}

// Stop terminates every child.
func TestGateway_StopStopsChildren(t *testing.T) {
	fa := newFakeAdapter()
	gw, _ := mesh.NewGateway(mesh.GatewayConfig{Adapter: fa, Envelope: gatewayEnv(), Min: 2})
	if err := gw.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := gw.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	fa.mu.Lock()
	defer fa.mu.Unlock()
	if len(fa.stopped) != 2 {
		t.Fatalf("stopped = %v, want 2", fa.stopped)
	}
}

// The registration loop POSTs the endpoint set to the Supervisor ingress on
// start and on every tick (the same POST is the heartbeat).
func TestGateway_RegistrationLoop(t *testing.T) {
	var mu sync.Mutex
	var got []mesh.Registration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/register" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		var reg mesh.Registration
		if err := json.NewDecoder(r.Body).Decode(&reg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		got = append(got, reg)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	fa := newFakeAdapter()
	gw, err := mesh.NewGateway(mesh.GatewayConfig{
		Adapter:          fa,
		Envelope:         gatewayEnv(),
		Min:              1,
		Pod:              "pool-aaa",
		Team:             "team-acme",
		Role:             "coder",
		RegisterURL:      srv.URL,
		RegisterInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := gw.Start(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	mu.Lock()
	defer mu.Unlock()
	if len(got) < 2 {
		t.Fatalf("registrations = %d, want >= 2 (initial + heartbeat)", len(got))
	}
	reg := got[0]
	if reg.Pod != "pool-aaa" || reg.Team != "team-acme" || reg.Role != "coder" {
		t.Fatalf("registration identity mismatch: %+v", reg)
	}
	if len(reg.Processes) != 1 || reg.Processes[0].EndpointID == "" || reg.Processes[0].Endpoint == "" {
		t.Fatalf("registration endpoint set broken: %+v", reg.Processes)
	}
}

// A gateway configured to register but missing the pool identity is
// rejected at construction — an unattributable registration is unroutable.
func TestGateway_RegistrationRequiresIdentity(t *testing.T) {
	fa := newFakeAdapter()
	if _, err := mesh.NewGateway(mesh.GatewayConfig{
		Adapter: fa, Envelope: gatewayEnv(), RegisterURL: "http://sup", Team: "t", // Role missing
	}); err == nil {
		t.Fatal("expected identity validation error")
	}
}

// AdvertiseURL rewrites every registered process's endpoint to the gateway's
// own routable URL (§5.2): children bind 127.0.0.1 inside the pod, so the
// loopback address the handle reports would be unroutable from the
// operator-side route leg. Unset keeps the handle's endpoint verbatim
// (local/dev lanes sharing the host network).
func TestGateway_RegistrationAdvertisesGatewayURL(t *testing.T) {
	var mu sync.Mutex
	var got []mesh.Registration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reg mesh.Registration
		_ = json.NewDecoder(r.Body).Decode(&reg)
		mu.Lock()
		got = append(got, reg)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	for _, tc := range []struct {
		name      string
		advertise string
		want      string
	}{
		{name: "advertised", advertise: "http://10.1.2.3:8080/", want: "http://10.1.2.3:8080"},
		{name: "unset keeps handle endpoint", advertise: "", want: "http://127.0.0.1:9000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fa := newFakeAdapter()
			gw, err := mesh.NewGateway(mesh.GatewayConfig{
				Adapter:          fa,
				Envelope:         gatewayEnv(),
				Min:              1,
				Pod:              "pool-aaa",
				Team:             "team-acme",
				Role:             "coder",
				RegisterURL:      srv.URL,
				RegisterInterval: time.Hour, // the immediate Start beat is enough
				AdvertiseURL:     tc.advertise,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := gw.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			_ = gw.Stop(context.Background())
			mu.Lock()
			defer mu.Unlock()
			if len(got) == 0 {
				t.Fatal("no registration arrived")
			}
			last := got[len(got)-1]
			if len(last.Processes) != 1 {
				t.Fatalf("processes = %d, want 1", len(last.Processes))
			}
			if ep := last.Processes[0].Endpoint; ep != tc.want {
				t.Fatalf("advertised endpoint = %q, want %q", ep, tc.want)
			}
		})
	}
}
