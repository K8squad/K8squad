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

package mesh_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/mesh"
)

// fakeAdapter extensions for P1.2: Recreate is recorded (and produces a NEW
// handle, like the real adapters), and lifecycle ops can be scripted to fail.
// The P1.1 fake lives in gateway_test.go; these helpers extend it.

// RecreateRecord is one observed Recreate call.
type RecreateRecord struct {
	OldID string
	Env   mesh.Envelope
}

// recordingAdapter wraps fakeAdapter, recording Recreate calls and returning
// fresh handles (the real ChildProcessAdapter.Recreate closes the old process
// and spawns a new one — the new handle has a different id).
type recordingAdapter struct {
	*fakeAdapter
	mu          sync.Mutex
	recreates   []RecreateRecord
	recreateErr error
	// delay parks Recreate (simulates recreate latency).
	delay time.Duration
}

func newRecordingAdapter() *recordingAdapter {
	return &recordingAdapter{fakeAdapter: newFakeAdapter()}
}

func (r *recordingAdapter) Recreate(ctx context.Context, h mesh.Handle, env mesh.Envelope) (mesh.Handle, error) {
	if r.delay > 0 {
		select {
		case <-time.After(r.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	r.mu.Lock()
	if r.recreateErr != nil {
		err := r.recreateErr
		r.mu.Unlock()
		return nil, err
	}
	r.recreates = append(r.recreates, RecreateRecord{OldID: h.ID(), Env: env})
	r.mu.Unlock()
	// A fresh handle: new id + endpoint + stream, like a respawned child.
	h, _, err := r.Spawn(ctx, env)
	return h, err
}

func (r *recordingAdapter) recreateCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.recreates)
}

// lastRecreate returns the most recent Recreate record.
func (r *recordingAdapter) lastRecreate() RecreateRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recreates[len(r.recreates)-1]
}

// obsRecorder is a mesh.LifecycleObserver that records events for assertions.
type obsRecorder struct {
	mu     sync.Mutex
	routes []mesh.RouteEvent
	spawns []mesh.SpawnEvent
	wakes  []mesh.WakeEvent
}

func (o *obsRecorder) ObserveRoute(_ context.Context, e mesh.RouteEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.routes = append(o.routes, e)
}

func (o *obsRecorder) ObserveSpawn(_ context.Context, e mesh.SpawnEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.spawns = append(o.spawns, e)
}

func (o *obsRecorder) ObserveWake(_ context.Context, e mesh.WakeEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.wakes = append(o.wakes, e)
}

// waitFor polls cond until true or the deadline passes (the sweep loop runs
// on its own ticker — lifecycle tests observe its OUTPUT, not its cadence).
func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}

// The sweep stops an IDLE process once its idle time reaches
// hibernateToStop — the RAM-reclaim tier (§6.1). REV-4: no hibernation tier
// in between; idle time runs straight to the stop bound.
func TestLifecycle_IdleProcessStopsAfterBound(t *testing.T) {
	fa := newFakeAdapter()
	gw, err := mesh.NewGateway(mesh.GatewayConfig{
		Adapter:  fa,
		Envelope: gatewayEnv(),
		Min:      1,
		Lifecycle: mesh.LifecycleConfig{
			CheckInterval:   5 * time.Millisecond,
			HibernateToStop: 30 * time.Millisecond,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gw.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// The slot goes STOPPED after the idle bound, without any request.
	waitFor(t, 2*time.Second, func() bool {
		return gw.Processes()[0].State == mesh.StateStopped
	}, "idle process was never stopped by the sweep")

	// The adapter actually terminated the process.
	fa.mu.Lock()
	defer fa.mu.Unlock()
	if len(fa.stopped) != 1 {
		t.Fatalf("adapter stops = %v, want 1", fa.stopped)
	}

	// A stopped slot is still Ready: recreate-on-demand is serving
	// capability (§6.1) — flipping NotReady would cycle the pod and defeat
	// the reclaim.
	if !gw.Ready() {
		t.Fatal("gateway must stay ready with a stopped (recreatable) slot")
	}
}

// A BUSY (ACTIVE) process is never stopped — the sweep only reclaims idle
// RAM (§6: task done → IDLE, the clock starts there).
func TestLifecycle_ActiveProcessNeverStopped(t *testing.T) {
	fa := newFakeAdapter()
	gw, _ := mesh.NewGateway(mesh.GatewayConfig{
		Adapter:  fa,
		Envelope: gatewayEnv(),
		Min:      1,
		Lifecycle: mesh.LifecycleConfig{
			CheckInterval:   5 * time.Millisecond,
			HibernateToStop: 20 * time.Millisecond,
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gw.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// Route and NEVER settle: the process stays ACTIVE forever.
	if _, err := gw.Route(context.Background(), "", a2a.Task{A2ATaskID: "t-1"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)

	p := gw.Processes()[0]
	if p.State != mesh.StateActive || !p.Busy {
		t.Fatalf("process state = %s busy=%v, want active/busy", p.State, p.Busy)
	}
	fa.mu.Lock()
	defer fa.mu.Unlock()
	if len(fa.stopped) != 0 {
		t.Fatalf("adapter stops = %v, want 0 (active process must not be stopped)", fa.stopped)
	}
}

// alwaysUp (the coordinator exemption, §6/§6.2): the pool NEVER stops its
// processes, however idle — pinned ACTIVE/IDLE, floor 1.
func TestLifecycle_AlwaysUpExempt(t *testing.T) {
	fa := newFakeAdapter()
	gw, _ := mesh.NewGateway(mesh.GatewayConfig{
		Adapter:  fa,
		Envelope: gatewayEnv(),
		Min:      1,
		Lifecycle: mesh.LifecycleConfig{
			CheckInterval:   5 * time.Millisecond,
			HibernateToStop: 20 * time.Millisecond,
			AlwaysUp:        true,
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gw.Start(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)

	p := gw.Processes()[0]
	if p.State != mesh.StateIdle {
		t.Fatalf("process state = %s, want idle (alwaysUp pins IDLE)", p.State)
	}
	fa.mu.Lock()
	defer fa.mu.Unlock()
	if len(fa.stopped) != 0 {
		t.Fatalf("adapter stops = %v, want 0 (alwaysUp exempts stop)", fa.stopped)
	}
}

// The ≤3s recreate path (D4.1): once every slot is STOPPED, the next route
// recreates from the envelope — Recreate is called with the STALE handle,
// the slot gets a NEW process, and the routing succeeds.
func TestLifecycle_RecreateOnNextRequest(t *testing.T) {
	ra := newRecordingAdapter()
	gw, _ := mesh.NewGateway(mesh.GatewayConfig{
		Adapter:  ra,
		Envelope: gatewayEnv(),
		Min:      1,
		Lifecycle: mesh.LifecycleConfig{
			CheckInterval:   5 * time.Millisecond,
			HibernateToStop: 20 * time.Millisecond,
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gw.Start(ctx); err != nil {
		t.Fatal(err)
	}
	oldID := gw.Processes()[0].EndpointID

	waitFor(t, 2*time.Second, func() bool {
		return gw.Processes()[0].State == mesh.StateStopped
	}, "process never stopped")

	// The next request recreates from the envelope and routes.
	s, err := gw.Route(context.Background(), "", a2a.Task{A2ATaskID: "t-2"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if n := ra.recreateCount(); n != 1 {
		t.Fatalf("recreates = %d, want 1", n)
	}
	rec := ra.lastRecreate()
	if rec.OldID != oldID {
		t.Fatalf("recreate used handle %q, want the stale %q", rec.OldID, oldID)
	}
	if rec.Env.Identity.Name != "coder" {
		t.Fatalf("recreate envelope identity = %+v, want the role envelope", rec.Env.Identity)
	}

	// The slot holds the NEW process and is ACTIVE with the live task.
	p := gw.Processes()[0]
	if p.State != mesh.StateActive || p.EndpointID == oldID {
		t.Fatalf("slot state = %s id = %s (old %s), want active/new-handle", p.State, p.EndpointID, oldID)
	}

	// Settling flips the recreated process back to IDLE (idle clock reset).
	// Consuming Events() starts the gated stream's pump — the release that
	// flips the state rides the pump's natural end.
	ra.SettleHandle(p.EndpointID)
	<-s.Events()
	waitFor(t, 2*time.Second, func() bool {
		return gw.Processes()[0].State == mesh.StateIdle
	}, "recreated process never flipped idle after settle")
}

// A failed recreate leaves the slot STOPPED and retryable — no phantom ACTIVE,
// no busy leak.
func TestLifecycle_RecreateFailureRetryable(t *testing.T) {
	ra := newRecordingAdapter()
	gw, _ := mesh.NewGateway(mesh.GatewayConfig{
		Adapter:  ra,
		Envelope: gatewayEnv(),
		Min:      1,
		Lifecycle: mesh.LifecycleConfig{
			CheckInterval:   5 * time.Millisecond,
			HibernateToStop: 20 * time.Millisecond,
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gw.Start(ctx); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 2*time.Second, func() bool {
		return gw.Processes()[0].State == mesh.StateStopped
	}, "process never stopped")

	// Fail the first recreate; the route must surface the error and leave a
	// clean stopped slot.
	ra.mu.Lock()
	ra.recreateErr = errors.New("recreate boom")
	ra.mu.Unlock()
	if _, err := gw.Route(context.Background(), "", a2a.Task{A2ATaskID: "t-x"}); err == nil {
		t.Fatal("expected recreate error to surface")
	}
	p := gw.Processes()[0]
	if p.State != mesh.StateStopped || p.Busy {
		t.Fatalf("slot after failed recreate: state=%s busy=%v, want stopped/not-busy", p.State, p.Busy)
	}

	// A retry with recreate working again succeeds.
	ra.mu.Lock()
	ra.recreateErr = nil
	ra.mu.Unlock()
	s, err := gw.Route(context.Background(), "", a2a.Task{A2ATaskID: "t-y"})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
}

// All-busy pool with no stopped slot still refuses with ErrNoIdleProcess —
// recreate never fabricates capacity beyond Min (the scale signal is P1.3).
func TestLifecycle_SaturationStillRefuses(t *testing.T) {
	fa := newFakeAdapter()
	gw, _ := mesh.NewGateway(mesh.GatewayConfig{Adapter: fa, Envelope: gatewayEnv(), Min: 1})
	if err := gw.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := gw.Route(context.Background(), "", a2a.Task{A2ATaskID: "t-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := gw.Route(context.Background(), "", a2a.Task{A2ATaskID: "t-2"}); !errors.Is(err, mesh.ErrNoIdleProcess) {
		t.Fatalf("second route err = %v, want ErrNoIdleProcess", err)
	}
}

// The observer receives the lifecycle telemetry: cold spawns at boot,
// envelope recreates on demand, and route events with the selection and the
// arrival→ready duration (the SLO window).
func TestLifecycle_ObserverSeesSpawnsRoutesRecreates(t *testing.T) {
	ra := newRecordingAdapter()
	obs := &obsRecorder{}
	gw, _ := mesh.NewGateway(mesh.GatewayConfig{
		Adapter:  ra,
		Envelope: gatewayEnv(),
		Min:      1,
		Team:     "team-acme",
		Role:     "coder",
		Pod:      "pool-aaa",
		Lifecycle: mesh.LifecycleConfig{
			CheckInterval:   5 * time.Millisecond,
			HibernateToStop: 20 * time.Millisecond,
		},
		Observer: obs,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gw.Start(ctx); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 2*time.Second, func() bool {
		return gw.Processes()[0].State == mesh.StateStopped
	}, "process never stopped")

	s, err := gw.Route(context.Background(), "", a2a.Task{A2ATaskID: "t-2"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	obs.mu.Lock()
	defer obs.mu.Unlock()
	// Two spawn events: boot (cold) + recreate (envelope).
	if len(obs.spawns) != 2 {
		t.Fatalf("spawn events = %d, want 2 (cold + envelope)", len(obs.spawns))
	}
	if obs.spawns[0].Source != mesh.SpawnSourceCold {
		t.Fatalf("boot spawn source = %s, want cold", obs.spawns[0].Source)
	}
	if obs.spawns[1].Source != mesh.SpawnSourceEnvelope {
		t.Fatalf("recreate spawn source = %s, want envelope", obs.spawns[1].Source)
	}
	// One route event: the recreate route, selection=overflow, with a
	// measured SLO window.
	if len(obs.routes) != 1 {
		t.Fatalf("route events = %d, want 1", len(obs.routes))
	}
	r := obs.routes[0]
	if r.Selection != mesh.SelectionOverflow {
		t.Fatalf("selection = %s, want overflow", r.Selection)
	}
	if r.Duration <= 0 {
		t.Fatalf("route duration = %v, want a measured window", r.Duration)
	}
	if r.Team != "team-acme" || r.Role != "coder" || r.Pod != "pool-aaa" || r.EndpointID == "" {
		t.Fatalf("route identity incomplete: %+v", r)
	}
}

// An explicit target naming a stopped slot recreates THAT slot.
func TestLifecycle_TargetStoppedSlotRecreates(t *testing.T) {
	ra := newRecordingAdapter()
	gw, _ := mesh.NewGateway(mesh.GatewayConfig{
		Adapter:  ra,
		Envelope: gatewayEnv(),
		Min:      1,
		Lifecycle: mesh.LifecycleConfig{
			CheckInterval:   5 * time.Millisecond,
			HibernateToStop: 20 * time.Millisecond,
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gw.Start(ctx); err != nil {
		t.Fatal(err)
	}
	oldID := gw.Processes()[0].EndpointID
	waitFor(t, 2*time.Second, func() bool {
		return gw.Processes()[0].State == mesh.StateStopped
	}, "process never stopped")

	s, err := gw.Route(context.Background(), oldID, a2a.Task{A2ATaskID: "t-3"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if n := ra.recreateCount(); n != 1 {
		t.Fatalf("recreates = %d, want 1 (targeted recreate)", n)
	}
}

// Stopped slots leave the registration set (the ingress prunes them), but a
// pool that HELD processes may still POST the (empty) set to force the prune;
// a gateway that never spawned still refuses.
func TestLifecycle_RegistrationExcludesStopped(t *testing.T) {
	var mu sync.Mutex
	var got []mesh.Registration
	srv := newRegisterServer(&got, &mu)
	defer srv.Close()

	ra := newRecordingAdapter()
	gw, _ := mesh.NewGateway(mesh.GatewayConfig{
		Adapter:          ra,
		Envelope:         gatewayEnv(),
		Min:              1,
		Pod:              "pool-aaa",
		Team:             "team-acme",
		Role:             "coder",
		RegisterURL:      srv.URL,
		RegisterInterval: time.Hour, // only the sweep-driven beats matter here
		Lifecycle: mesh.LifecycleConfig{
			CheckInterval:   5 * time.Millisecond,
			HibernateToStop: 20 * time.Millisecond,
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gw.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// After the sweep stops the process, a beat POSTs the (now empty)
	// standing set so the ingress prunes the stale endpoint.
	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= 2 && len(got[len(got)-1].Processes) == 0
	}, "no prune-registration arrived after the sweep stopped the process")
}

// newRegisterServer is the ingest side of the P1.1 registration test helper.
func newRegisterServer(got *[]mesh.Registration, mu *sync.Mutex) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reg mesh.Registration
		if err := json.NewDecoder(r.Body).Decode(&reg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		*got = append(*got, reg)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
}

// hibernationSpy counts Hibernate/Wake calls — the S4 tier's evidence surface
// (ISI-5831): the collapsed-ladder default must never touch them, and the
// armed ladder's SIGSTOP/SIGCONT cadence is asserted against them. The S4
// battery lives in hibernate_test.go.
type hibernationSpy struct {
	*recordingAdapter
	mu        sync.Mutex
	hibernate int
	wake      int
}

func (s *hibernationSpy) Hibernate(ctx context.Context, h mesh.Handle) error {
	s.mu.Lock()
	s.hibernate++
	s.mu.Unlock()
	return s.recordingAdapter.Hibernate(ctx, h)
}

func (s *hibernationSpy) Wake(ctx context.Context, h mesh.Handle) error {
	s.mu.Lock()
	s.wake++
	s.mu.Unlock()
	return s.recordingAdapter.Wake(ctx, h)
}

// Lifecycle defaults (D4.2): the operator-default table.
func TestLifecycle_Defaults(t *testing.T) {
	c := mesh.LifecycleConfig{}.WithDefaults()
	if c.CheckInterval != 30*time.Second {
		t.Fatalf("checkInterval default = %v, want 30s", c.CheckInterval)
	}
	if c.IdleToHibernate != 5*time.Minute {
		t.Fatalf("idleToHibernate default = %v, want 5m", c.IdleToHibernate)
	}
	if c.HibernateToStop != 4*time.Hour {
		t.Fatalf("hibernateToStop default = %v, want 4h", c.HibernateToStop)
	}
	if c.AlwaysUp {
		t.Fatal("alwaysUp default = false")
	}
}
