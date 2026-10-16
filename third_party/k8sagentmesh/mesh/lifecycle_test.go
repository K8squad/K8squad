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
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/K8sAgentMesh/K8sAgentMesh/a2a"
	"github.com/K8sAgentMesh/K8sAgentMesh/mesh"
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

// ---------------------------------------------------------------------------
// P1.4 supervisor gateway-health tests (ISI-6010): the drain/recreate loop
// riding the same fake-seam harness as the P1.2 lifecycle tests above.

// fakeSupervisorStore scripts the registry read and records drains.
type fakeSupervisorStore struct {
	mu     sync.Mutex
	eps    []mesh.EndpointHealth
	err    error
	drains []string
}

func (s *fakeSupervisorStore) Endpoints(context.Context) ([]mesh.EndpointHealth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	return append([]mesh.EndpointHealth(nil), s.eps...), nil
}

func (s *fakeSupervisorStore) DrainPod(_ context.Context, pod string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drains = append(s.drains, pod)
	// The real drain deletes the pod's rows; mirror that so the next sweep
	// sees the drained state.
	kept := s.eps[:0]
	for _, ep := range s.eps {
		if ep.Pod != pod {
			kept = append(kept, ep)
		}
	}
	s.eps = kept
	return nil
}

func (s *fakeSupervisorStore) drained(pod string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.drains {
		if d == pod {
			return true
		}
	}
	return false
}

// fakePodRecreator records recreates and can be scripted to fail.
type fakePodRecreator struct {
	mu    sync.Mutex
	err   error
	calls []string
}

func (r *fakePodRecreator) RecreatePod(_ context.Context, pod string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.calls = append(r.calls, pod)
	return nil
}

func (r *fakePodRecreator) recreated(pod string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if c == pod {
			n++
		}
	}
	return n
}

// opRecorder records drain/recreate ORDER across both seams — the evidence
// for the drain-first (no-phantom-routable) invariant.
type opRecorder struct {
	mu  sync.Mutex
	ops []string
}

func (o *opRecorder) record(op string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ops = append(o.ops, op)
}

func supervisorFixture(now time.Time) (*fakeSupervisorStore, *fakePodRecreator) {
	store := &fakeSupervisorStore{eps: []mesh.EndpointHealth{
		// pool-aaa: heartbeat 5m ago — TTL-expired (gateway down).
		{EndpointID: "ep-a1", Pod: "pool-aaa", Team: "team-acme", Role: "coder", Endpoint: "http://10.0.0.1:8080", LastSeenAt: now.Add(-5 * time.Minute)},
		// pool-bbb: heartbeat 10s ago — live.
		{EndpointID: "ep-b1", Pod: "pool-bbb", Team: "team-acme", Role: "coder", Endpoint: "http://10.0.0.2:8080", LastSeenAt: now.Add(-10 * time.Second)},
	}}
	return store, &fakePodRecreator{}
}

// The TTL leg: a pod with ZERO live endpoints is drained and recreated; a
// live pod is untouched.
func TestSupervisorHealth_TTLExpiryDrainsAndRecreates(t *testing.T) {
	now := time.Now()
	store, rec := supervisorFixture(now)
	sup, err := mesh.NewSupervisorHealth(mesh.SupervisorHealthConfig{
		Store:    store,
		Recreate: rec,
		TTL:      90 * time.Second,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	sup.SweepOnce(context.Background())

	if !store.drained("pool-aaa") {
		t.Fatalf("pool-aaa was not drained: drains=%v", store.drains)
	}
	if n := rec.recreated("pool-aaa"); n != 1 {
		t.Fatalf("pool-aaa recreates = %d, want 1", n)
	}
	if store.drained("pool-bbb") || rec.recreated("pool-bbb") != 0 {
		t.Fatalf("live pod pool-bbb was touched: drains=%v recreates=%d", store.drains, rec.recreated("pool-bbb"))
	}

	// A second sweep is a no-op: the drained pod is gone from the registry
	// and its recreate succeeded (nothing parked in pending).
	sup.SweepOnce(context.Background())
	if n := rec.recreated("pool-aaa"); n != 1 {
		t.Fatalf("pool-aaa recreated again on a quiet sweep: %d, want 1", n)
	}
}

// A failed recreate is RETRYABLE (the P1.2 invariant one level up): the pod
// stays drained (never phantom-routable) and every sweep retries until the
// recreate succeeds.
func TestSupervisorHealth_RecreateFailureRetryable(t *testing.T) {
	now := time.Now()
	store, rec := supervisorFixture(now)
	rec.err = errors.New("scheduler boom")
	sup, _ := mesh.NewSupervisorHealth(mesh.SupervisorHealthConfig{
		Store:    store,
		Recreate: rec,
		TTL:      90 * time.Second,
		Now:      func() time.Time { return now },
	})

	sup.SweepOnce(context.Background())
	if !store.drained("pool-aaa") {
		t.Fatal("pool-aaa was not drained")
	}
	if n := rec.recreated("pool-aaa"); n != 0 {
		t.Fatalf("failed recreate recorded a success: %d", n)
	}

	// Still failing: the pending pod is retried every sweep even though its
	// rows are gone from the registry (the TTL leg can no longer see it).
	sup.SweepOnce(context.Background())

	// Recover: the next sweep's retry lands the recreate.
	rec.mu.Lock()
	rec.err = nil
	rec.mu.Unlock()
	sup.SweepOnce(context.Background())
	if n := rec.recreated("pool-aaa"); n != 1 {
		t.Fatalf("pool-aaa recreates after recovery = %d, want 1", n)
	}

	// Recovered and quiet: no further retries.
	sup.SweepOnce(context.Background())
	if n := rec.recreated("pool-aaa"); n != 1 {
		t.Fatalf("pool-aaa recreated after success: %d, want 1", n)
	}
}

// The health leg: a pod whose rows are TTL-live but whose /ready probe fails
// is drained+recreated only after DownThreshold CONSECUTIVE failures — one
// flap never bounces a healthy pool.
func TestSupervisorHealth_ProbeDownThreshold(t *testing.T) {
	now := time.Now()
	store, rec := supervisorFixture(now)
	// Drop the expired pod so only the live (probe-able) one remains.
	store.eps = store.eps[1:]
	down := true
	sup, _ := mesh.NewSupervisorHealth(mesh.SupervisorHealthConfig{
		Store:    store,
		Recreate: rec,
		TTL:      90 * time.Second,
		Health: func(context.Context, string) bool {
			return !down
		},
		Now: func() time.Time { return now },
	})

	// First failed probe: below the default threshold (2) — no action.
	sup.SweepOnce(context.Background())
	if store.drained("pool-bbb") {
		t.Fatal("one failed probe drained the pod — threshold must be 2 consecutive")
	}

	// A healthy probe resets the streak.
	down = false
	sup.SweepOnce(context.Background())
	down = true
	sup.SweepOnce(context.Background())
	if store.drained("pool-bbb") {
		t.Fatal("a healthy probe did not reset the consecutive-failure streak")
	}

	// Two consecutive failures: drain + recreate.
	sup.SweepOnce(context.Background())
	if !store.drained("pool-bbb") {
		t.Fatal("two consecutive failed probes did not drain the pod")
	}
	if n := rec.recreated("pool-bbb"); n != 1 {
		t.Fatalf("pool-bbb recreates = %d, want 1", n)
	}
}

// Drain FIRST, recreate SECOND — while the recreate is in flight the registry
// holds nothing for the pod, so the route leg can never hand a task to a
// gateway that is going away (no phantom routable; P1.2's no-phantom-ACTIVE
// one level up).
func TestSupervisorHealth_DrainBeforeRecreate(t *testing.T) {
	now := time.Now()
	store, rec := supervisorFixture(now)
	order := &opRecorder{}

	// Wrap both seams with the order recorder.
	orderedStore := &orderStore{inner: store, rec: order}
	orderedRec := &orderRecreator{inner: rec, rec: order}

	sup, _ := mesh.NewSupervisorHealth(mesh.SupervisorHealthConfig{
		Store:    orderedStore,
		Recreate: orderedRec,
		TTL:      90 * time.Second,
		Now:      func() time.Time { return now },
	})
	sup.SweepOnce(context.Background())

	order.mu.Lock()
	defer order.mu.Unlock()
	var drainIdx, recreateIdx = -1, -1
	for i, op := range order.ops {
		if op == "drain:pool-aaa" {
			drainIdx = i
		}
		if op == "recreate:pool-aaa" {
			recreateIdx = i
		}
	}
	if drainIdx == -1 || recreateIdx == -1 {
		t.Fatalf("missing drain or recreate: ops=%v", order.ops)
	}
	if drainIdx > recreateIdx {
		t.Fatalf("recreate ran BEFORE drain: ops=%v — violates the no-phantom invariant", order.ops)
	}
}

type orderStore struct {
	inner *fakeSupervisorStore
	rec   *opRecorder
}

func (s *orderStore) Endpoints(ctx context.Context) ([]mesh.EndpointHealth, error) {
	return s.inner.Endpoints(ctx)
}

func (s *orderStore) DrainPod(ctx context.Context, pod string) error {
	s.rec.record("drain:" + pod)
	return s.inner.DrainPod(ctx, pod)
}

type orderRecreator struct {
	inner *fakePodRecreator
	rec   *opRecorder
}

func (r *orderRecreator) RecreatePod(ctx context.Context, pod string) error {
	r.rec.record("recreate:" + pod)
	return r.inner.RecreatePod(ctx, pod)
}

// Validation: the loop refuses a missing Store or Recreator.
func TestSupervisorHealth_ConfigValidation(t *testing.T) {
	if _, err := mesh.NewSupervisorHealth(mesh.SupervisorHealthConfig{}); err == nil {
		t.Fatal("nil Store accepted")
	}
	if _, err := mesh.NewSupervisorHealth(mesh.SupervisorHealthConfig{Store: &fakeSupervisorStore{}}); err == nil {
		t.Fatal("nil Recreate accepted")
	}
}

// ProbeReady is the supervisor-side "gateway up" signal: 200 from /ready is
// up; 503 (not serving), a wrong path, and an unreachable gateway are down.
func TestProbeReady(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ready" {
			t.Errorf("probe hit %s, want /ready", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	if !mesh.ProbeReady(context.Background(), nil, up.URL+"/") {
		t.Fatal("200 /ready gateway probed down")
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()
	if mesh.ProbeReady(context.Background(), nil, down.URL) {
		t.Fatal("503 /ready gateway probed up")
	}

	// Unreachable (a listener that never answers in time / a dead pod IP).
	if mesh.ProbeReady(context.Background(), nil, "http://127.0.0.1:1") {
		t.Fatal("unreachable gateway probed up")
	}
}
