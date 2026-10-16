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
	"errors"
	"fmt"
	"sync"
	"syscall"
	"testing"

	"github.com/K8sAgentMesh/K8sAgentMesh/a2a"
)

// fakeInstance records the signals and closes the adapter drives through the
// Instance seam — the proof that Hibernate/Wake/Stop map onto the right OS
// primitive without launching a real process.
type fakeInstance struct {
	id       string
	endpoint string

	mu      sync.Mutex
	signals []syscall.Signal
	closes  int
	sigErr  error
	closErr error
}

func (f *fakeInstance) ID() string       { return f.id }
func (f *fakeInstance) Endpoint() string { return f.endpoint }

func (f *fakeInstance) Signal(sig syscall.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, sig)
	return f.sigErr
}

func (f *fakeInstance) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return f.closErr
}

func (f *fakeInstance) lastSignal() (syscall.Signal, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.signals) == 0 {
		return 0, false
	}
	return f.signals[len(f.signals)-1], true
}

// fakeHost hands out fakeInstances and records the launch specs it saw.
type fakeHost struct {
	mu        sync.Mutex
	specs     []LaunchSpec
	launchErr error
	nextInst  func(i int) *fakeInstance
	sink      TelemetrySink
}

func (h *fakeHost) Launch(ctx context.Context, spec LaunchSpec) (Instance, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.launchErr != nil {
		return nil, h.launchErr
	}
	i := len(h.specs)
	h.specs = append(h.specs, spec)
	if h.nextInst != nil {
		return h.nextInst(i), nil
	}
	return &fakeInstance{id: fmt.Sprintf("inst-%d", i), endpoint: fmt.Sprintf("http://inst-%d:8080", i)}, nil
}

func (h *fakeHost) Telemetry() TelemetrySink {
	if h.sink != nil {
		return h.sink
	}
	return NopSink{}
}

// fakeRouter records the Route calls and returns a canned stream.
type fakeRouter struct {
	mu        sync.Mutex
	endpoints []string
	tasks     []a2a.Task
	stream    Stream
	err       error
}

func (r *fakeRouter) Route(ctx context.Context, endpoint string, task a2a.Task) (Stream, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endpoints = append(r.endpoints, endpoint)
	r.tasks = append(r.tasks, task)
	if r.err != nil {
		return nil, r.err
	}
	return r.stream, nil
}

// nopStream is a closed, empty Stream for Route tests.
type nopStream struct{ closes int }

func (s *nopStream) Events() <-chan a2a.Event {
	ch := make(chan a2a.Event)
	close(ch)
	return ch
}
func (s *nopStream) Status() a2a.Status { return a2a.Status{} }
func (s *nopStream) Close() error       { s.closes++; return nil }

// recordingSink captures lifecycle events for assertions.
type recordingSink struct {
	mu     sync.Mutex
	events []sinkEvent
}

type sinkEvent struct {
	op       string
	handleID string
	endpoint string
	err      error
}

func (s *recordingSink) OnLifecycle(op, handleID, endpoint string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, sinkEvent{op, handleID, endpoint, err})
}

func (s *recordingSink) ops() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.events))
	for i, e := range s.events {
		out[i] = e.op
	}
	return out
}

// newTestAdapter builds a base child-process adapter over the given host/router
// with a trivial card/launch so the 7 ops can be exercised without opencode.
func newTestAdapter(host PlatformHost, router Router) *ChildProcessAdapter {
	card := func(env Envelope) a2a.AgentCard {
		return a2a.AgentCard{Runtime: a2a.RuntimeInfo{Type: env.RuntimeType}, Skills: env.Skills}
	}
	launch := func(env Envelope) (LaunchSpec, error) {
		return LaunchSpec{RuntimeType: env.RuntimeType, Env: []string{"RT=" + env.RuntimeType}}, nil
	}
	return newChildProcessAdapter(host, "test", card, launch, WithRouter(router))
}

func TestSpawnLaunchesAndReturnsCard(t *testing.T) {
	host := &fakeHost{}
	a := newTestAdapter(host, &fakeRouter{})

	h, card, err := a.Spawn(context.Background(), Envelope{Skills: []string{"review"}})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if h.ID() != "inst-0" || h.Endpoint() != "http://inst-0:8080" {
		t.Fatalf("handle = %s/%s, want inst-0", h.ID(), h.Endpoint())
	}
	// RuntimeType defaults to the adapter's flavor when the envelope omits it.
	if card.Runtime.Type != "test" {
		t.Fatalf("card runtime type = %q, want test", card.Runtime.Type)
	}
	if len(host.specs) != 1 || host.specs[0].RuntimeType != "test" {
		t.Fatalf("launch spec = %+v, want one test spec", host.specs)
	}
}

func TestSpawnLaunchError(t *testing.T) {
	host := &fakeHost{launchErr: errors.New("no pod")}
	a := newTestAdapter(host, &fakeRouter{})
	if _, _, err := a.Spawn(context.Background(), Envelope{}); err == nil {
		t.Fatal("Spawn should fail when the host cannot launch")
	}
}

func TestSpawnBuildSpecError(t *testing.T) {
	host := &fakeHost{}
	a := newChildProcessAdapter(host, "test",
		func(Envelope) a2a.AgentCard { return a2a.AgentCard{} },
		func(Envelope) (LaunchSpec, error) { return LaunchSpec{}, errors.New("bad route") },
	)
	if _, _, err := a.Spawn(context.Background(), Envelope{}); err == nil {
		t.Fatal("Spawn should fail when the launch spec cannot be built")
	}
	if len(host.specs) != 0 {
		t.Fatal("host must not be launched when the spec build fails")
	}
}

func TestRegisterReturnsSpawnCard(t *testing.T) {
	a := newTestAdapter(&fakeHost{}, &fakeRouter{})
	h, spawnCard, err := a.Spawn(context.Background(), Envelope{RuntimeType: "test", Skills: []string{"a", "b"}})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	regCard, err := a.Register(context.Background(), h)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if len(regCard.Skills) != 2 || regCard.Runtime.Type != spawnCard.Runtime.Type {
		t.Fatalf("Register card = %+v, want the Spawn card", regCard)
	}
}

func TestRouteDeliversToEndpoint(t *testing.T) {
	stream := &nopStream{}
	router := &fakeRouter{stream: stream}
	a := newTestAdapter(&fakeHost{}, router)

	h, _, err := a.Spawn(context.Background(), Envelope{})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	got, err := a.Route(context.Background(), h, a2a.Task{A2ATaskID: "run-1"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if got != stream {
		t.Fatal("Route should return the router's stream")
	}
	if len(router.endpoints) != 1 || router.endpoints[0] != h.Endpoint() {
		t.Fatalf("router endpoints = %v, want [%s]", router.endpoints, h.Endpoint())
	}
	if router.tasks[0].A2ATaskID != "run-1" {
		t.Fatalf("router task id = %q, want run-1", router.tasks[0].A2ATaskID)
	}
}

func TestRouteRequiresTaskID(t *testing.T) {
	a := newTestAdapter(&fakeHost{}, &fakeRouter{})
	h, _, _ := a.Spawn(context.Background(), Envelope{})
	if _, err := a.Route(context.Background(), h, a2a.Task{}); err == nil {
		t.Fatal("Route must reject an empty a2a_task_id")
	}
}

func TestRouteError(t *testing.T) {
	router := &fakeRouter{err: errors.New("dial fail")}
	a := newTestAdapter(&fakeHost{}, router)
	h, _, _ := a.Spawn(context.Background(), Envelope{})
	if _, err := a.Route(context.Background(), h, a2a.Task{A2ATaskID: "x"}); err == nil {
		t.Fatal("Route should propagate the router error")
	}
}

func TestHibernateWakeSignals(t *testing.T) {
	inst := &fakeInstance{id: "i0", endpoint: "http://i0:8080"}
	host := &fakeHost{nextInst: func(int) *fakeInstance { return inst }}
	a := newTestAdapter(host, &fakeRouter{})

	h, _, _ := a.Spawn(context.Background(), Envelope{})

	if err := a.Hibernate(context.Background(), h); err != nil {
		t.Fatalf("Hibernate: %v", err)
	}
	if sig, _ := inst.lastSignal(); sig != syscall.SIGSTOP {
		t.Fatalf("Hibernate sent %v, want SIGSTOP", sig)
	}
	if err := a.Wake(context.Background(), h); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if sig, _ := inst.lastSignal(); sig != syscall.SIGCONT {
		t.Fatalf("Wake sent %v, want SIGCONT", sig)
	}
}

func TestHibernateSignalError(t *testing.T) {
	inst := &fakeInstance{id: "i0", sigErr: errors.New("ESRCH")}
	host := &fakeHost{nextInst: func(int) *fakeInstance { return inst }}
	a := newTestAdapter(host, &fakeRouter{})
	h, _, _ := a.Spawn(context.Background(), Envelope{})
	if err := a.Hibernate(context.Background(), h); err == nil {
		t.Fatal("Hibernate should propagate a signal error")
	}
}

func TestStopClosesInstance(t *testing.T) {
	inst := &fakeInstance{id: "i0"}
	host := &fakeHost{nextInst: func(int) *fakeInstance { return inst }}
	a := newTestAdapter(host, &fakeRouter{})
	h, _, _ := a.Spawn(context.Background(), Envelope{})
	if err := a.Stop(context.Background(), h); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if inst.closes != 1 {
		t.Fatalf("Stop closed %d times, want 1", inst.closes)
	}
}

func TestRecreateStopsOldAndSpawnsNew(t *testing.T) {
	var insts []*fakeInstance
	host := &fakeHost{nextInst: func(i int) *fakeInstance {
		in := &fakeInstance{id: fmt.Sprintf("i%d", i), endpoint: fmt.Sprintf("http://i%d:8080", i)}
		insts = append(insts, in)
		return in
	}}
	a := newTestAdapter(host, &fakeRouter{})

	h0, _, _ := a.Spawn(context.Background(), Envelope{RuntimeType: "test"})
	h1, err := a.Recreate(context.Background(), h0, Envelope{RuntimeType: "test"})
	if err != nil {
		t.Fatalf("Recreate: %v", err)
	}
	if h1.ID() == h0.ID() {
		t.Fatal("Recreate must return a fresh handle")
	}
	if insts[0].closes != 1 {
		t.Fatalf("Recreate did not stop the old instance (closes=%d)", insts[0].closes)
	}
	if len(host.specs) != 2 {
		t.Fatalf("Recreate launched %d times, want 2", len(host.specs))
	}
}

func TestRecreateSurvivesStopFailure(t *testing.T) {
	calls := 0
	host := &fakeHost{nextInst: func(i int) *fakeInstance {
		calls++
		// The first (old) instance fails to close — the wedged-process case
		// Recreate exists to recover from. The rebuild must still happen.
		if i == 0 {
			return &fakeInstance{id: "old", closErr: errors.New("already dead")}
		}
		return &fakeInstance{id: "new"}
	}}
	a := newTestAdapter(host, &fakeRouter{})
	h0, _, _ := a.Spawn(context.Background(), Envelope{})
	h1, err := a.Recreate(context.Background(), h0, Envelope{})
	if err != nil {
		t.Fatalf("Recreate should survive a stop failure: %v", err)
	}
	if h1.ID() != "new" {
		t.Fatalf("Recreate handle = %q, want new", h1.ID())
	}
}

func TestForeignHandleRejected(t *testing.T) {
	a := newTestAdapter(&fakeHost{}, &fakeRouter{})
	foreign := &foreignHandle{}
	ctx := context.Background()
	if _, err := a.Register(ctx, foreign); err == nil {
		t.Error("Register must reject a foreign handle")
	}
	if _, err := a.Route(ctx, foreign, a2a.Task{A2ATaskID: "x"}); err == nil {
		t.Error("Route must reject a foreign handle")
	}
	if err := a.Hibernate(ctx, foreign); err == nil {
		t.Error("Hibernate must reject a foreign handle")
	}
	if err := a.Wake(ctx, foreign); err == nil {
		t.Error("Wake must reject a foreign handle")
	}
	if err := a.Stop(ctx, foreign); err == nil {
		t.Error("Stop must reject a foreign handle")
	}
	if _, err := a.Recreate(ctx, foreign, Envelope{}); err == nil {
		t.Error("Recreate must reject a foreign handle")
	}
}

type foreignHandle struct{}

func (foreignHandle) ID() string       { return "foreign" }
func (foreignHandle) Endpoint() string { return "http://foreign" }

func TestTelemetrySinkRecordsLifecycle(t *testing.T) {
	sink := &recordingSink{}
	inst := &fakeInstance{id: "i0", endpoint: "http://i0:8080"}
	host := &fakeHost{sink: sink, nextInst: func(int) *fakeInstance { return inst }}
	a := newTestAdapter(host, &fakeRouter{stream: &nopStream{}})

	ctx := context.Background()
	h, _, _ := a.Spawn(ctx, Envelope{})
	_, _ = a.Register(ctx, h)
	_, _ = a.Route(ctx, h, a2a.Task{A2ATaskID: "x"})
	_ = a.Hibernate(ctx, h)
	_ = a.Wake(ctx, h)
	_ = a.Stop(ctx, h)

	want := []string{"spawn", "register", "route", "hibernate", "wake", "stop"}
	got := sink.ops()
	if len(got) != len(want) {
		t.Fatalf("sink ops = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sink op[%d] = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}
