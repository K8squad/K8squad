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

// hibernate_test.go — the AgentMesh S4 battery (ISI-5831): the two-tier
// idle→hibernate→stop ladder behind LifecycleConfig.Hibernate, the wake tier
// on Route (§5.3: prefer idle, then hibernated), rebind-at-wake (X5), and the
// D9.1 claim-never-frozen invariant proven as DoD gate D2 by T-HIB-LEASE-01:
// no double-dispatch under any hibernate/sweep interleaving.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/K8sAgentMesh/K8sAgentMesh/a2a"
	"github.com/K8sAgentMesh/K8sAgentMesh/mesh"
)

// hibEnv is the S4 test envelope: a model the rebind test can drift from.
func hibEnv() mesh.Envelope {
	env := gatewayEnv()
	env.Model = "model-a"
	return env
}

// armedLifecycle is the S4 knob set: hibernate armed, all bounds tiny so the
// tests drive the ladder with waitFor instead of sleeps.
func armedLifecycle() mesh.LifecycleConfig {
	return mesh.LifecycleConfig{
		Hibernate:       true,
		CheckInterval:   5 * time.Millisecond,
		IdleToHibernate: 20 * time.Millisecond,
		HibernateToStop: 150 * time.Millisecond,
	}
}

// TestS4_TwoTierLadder_HibernateThenStop walks the full armed ladder:
// IDLE → (idleToHibernate) HIBERNATED → (hibernateToStop measured from the
// HIBERNATE instant, not from idle) STOPPED. The stop-from-hibernated leg
// wakes first so the graceful SIGTERM lands (§6.1 D4.1).
func TestS4_TwoTierLadder_HibernateThenStop(t *testing.T) {
	spy := &hibernationSpy{recordingAdapter: newRecordingAdapter()}
	gw, err := mesh.NewGateway(mesh.GatewayConfig{
		Adapter: spy, Envelope: hibEnv(), Min: 1,
		Lifecycle: armedLifecycle(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gw.Start(ctx); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 2*time.Second, func() bool {
		return gw.Processes()[0].State == mesh.StateHibernated
	}, "process never hibernated past idleToHibernate")
	spy.mu.Lock()
	h := spy.hibernate
	spy.mu.Unlock()
	if h != 1 {
		t.Fatalf("hibernate calls = %d, want exactly 1 (SIGSTOP per tier entry)", h)
	}

	// It must STAY hibernated well past idleToHibernate but inside
	// hibernateToStop — the tier boundary is real, not idle accrual.
	time.Sleep(60 * time.Millisecond)
	if s := gw.Processes()[0].State; s != mesh.StateHibernated {
		t.Fatalf("state = %s, want hibernated (inside the hibernateToStop window)", s)
	}

	waitFor(t, 2*time.Second, func() bool {
		return gw.Processes()[0].State == mesh.StateStopped
	}, "hibernated process never stopped past hibernateToStop")
	spy.mu.Lock()
	w := spy.wake
	h2 := spy.hibernate
	spy.mu.Unlock()
	// The stop-from-hibernated leg SIGCONTs first (graceful SIGTERM); the
	// wake count is at least the pre-stop resume.
	if w < 1 {
		t.Fatalf("wake calls = %d, want >=1 (wake-before-stop on the frozen tier)", w)
	}
	if h2 != 1 {
		t.Fatalf("hibernate calls = %d, want 1 (no re-hibernate after stop)", h2)
	}
}

// TestS4_CollapsedLadderDefault_IsByteIdenticalP12 is the REV-4 X1 canary,
// reframed at S4: the two-tier ladder is OPT-IN. With Hibernate disarmed (the
// default — and what every pre-S4 pool pod runs), even with BOTH bounds long
// past, the gateway must never SIGSTOP a process into HIBERNATED and never
// Wake one: idle time accrues straight to the stop bound. If this fails,
// someone made hibernation unconditional — revert that; the armed ladder is
// gated by LifecycleConfig.Hibernate (the D9.3 kill-switch).
func TestS4_CollapsedLadderDefault_IsByteIdenticalP12(t *testing.T) {
	spy := &hibernationSpy{recordingAdapter: newRecordingAdapter()}
	gw, err := mesh.NewGateway(mesh.GatewayConfig{
		Adapter:  spy,
		Envelope: gatewayEnv(),
		Min:      1,
		Lifecycle: mesh.LifecycleConfig{
			CheckInterval:   5 * time.Millisecond,
			IdleToHibernate: 10 * time.Millisecond, // BOTH bounds tiny — the
			HibernateToStop: 15 * time.Millisecond, // tier boundary is long past
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

	waitFor(t, 2*time.Second, func() bool {
		p := gw.Processes()
		return len(p) > 0 && p[0].State == mesh.StateStopped && !p[0].Recreating
	}, "process never stopped (collapsed ladder)")
	s, err := gw.Route(context.Background(), "", a2a.Task{A2ATaskID: "t-collapsed"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	spy.mu.Lock()
	defer spy.mu.Unlock()
	if spy.hibernate != 0 || spy.wake != 0 {
		t.Fatalf("collapsed-ladder violation: Hibernate called %d×, Wake %d× — the tier is S4-opt-in (LifecycleConfig.Hibernate)",
			spy.hibernate, spy.wake)
	}
	for _, p := range gw.Processes() {
		if p.State == mesh.StateHibernated {
			t.Fatal("collapsed-ladder violation: a process entered StateHibernated without the S4 arm")
		}
	}
}

// TestS4_AlwaysUpExemptFromHibernate: the coordinator posture pins
// ACTIVE/IDLE — never HIBERNATED, never STOPPED (§6 diagram note).
func TestS4_AlwaysUpExemptFromHibernate(t *testing.T) {
	spy := &hibernationSpy{recordingAdapter: newRecordingAdapter()}
	lc := armedLifecycle()
	lc.AlwaysUp = true
	gw, err := mesh.NewGateway(mesh.GatewayConfig{
		Adapter: spy, Envelope: hibEnv(), Min: 1, Lifecycle: lc,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gw.Start(ctx); err != nil {
		t.Fatal(err)
	}

	time.Sleep(250 * time.Millisecond) // past BOTH bounds
	if s := gw.Processes()[0].State; s != mesh.StateIdle {
		t.Fatalf("alwaysUp state = %s, want idle (exempt from hibernate+stop)", s)
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if spy.hibernate != 0 || spy.wake != 0 {
		t.Fatalf("alwaysUp pool touched Hibernate/Wake (%d/%d) — coordinators never freeze",
			spy.hibernate, spy.wake)
	}
}

// TestS4_RouteWakesHibernated_LRU: with no idle process, Route's wake tier
// picks the hibernated one (selection=wake), SIGCONTs BEFORE delivering, and
// the process is ACTIVE until the stream settles back to IDLE.
func TestS4_RouteWakesHibernated(t *testing.T) {
	spy := &hibernationSpy{recordingAdapter: newRecordingAdapter()}
	obs := &obsRecorder{}
	gw, err := mesh.NewGateway(mesh.GatewayConfig{
		Adapter: spy, Envelope: hibEnv(), Min: 1,
		Lifecycle: armedLifecycle(), Observer: obs,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gw.Start(ctx); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 2*time.Second, func() bool {
		return gw.Processes()[0].State == mesh.StateHibernated
	}, "process never hibernated")

	s, err := gw.Route(context.Background(), "", a2a.Task{A2ATaskID: "t-wake"})
	if err != nil {
		t.Fatal(err)
	}
	if got := gw.Processes()[0].State; got != mesh.StateActive {
		t.Fatalf("post-wake state = %s, want active", got)
	}
	obs.mu.Lock()
	if len(obs.routes) != 1 || obs.routes[0].Selection != mesh.SelectionWake {
		obs.mu.Unlock()
		t.Fatalf("route selection = %+v, want selection=wake", obs.routes)
	}
	if len(obs.wakes) != 1 || obs.wakes[0].Err != nil || obs.wakes[0].Rebind {
		obs.mu.Unlock()
		t.Fatalf("wake events = %+v, want one clean non-rebind wake", obs.wakes)
	}
	obs.mu.Unlock()
	s.Close()
	waitFor(t, 2*time.Second, func() bool {
		return gw.Processes()[0].State == mesh.StateIdle
	}, "woken process never settled back to idle")
}

// TestS4_WakeInFlightWindowRefused: while the sweep has DECIDED to hibernate
// but the SIGSTOP has not landed (hibInFlight), Route must not pick the
// process — a SIGCONT racing ahead of the SIGSTOP would freeze a process
// mid-delivery (the wake-during-write hazard, Brainstormer-F3). The refusal
// is the legible 409 shape the re-drive retries.
func TestS4_WakeInFlightWindowRefused(t *testing.T) {
	block := make(chan struct{})
	adapter := newBlockingHibAdapter(block)
	gw, err := mesh.NewGateway(mesh.GatewayConfig{
		Adapter: adapter, Envelope: hibEnv(), Min: 1,
		Lifecycle: armedLifecycle(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gw.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// Park inside the SIGSTOP window: the sweep decided (state=hibernated,
	// hibInFlight) but the adapter call has not returned.
	waitFor(t, 2*time.Second, func() bool {
		return gw.Processes()[0].State == mesh.StateHibernated && adapter.hibStarted()
	}, "sweep never entered the hibernate window")

	if _, err := gw.Route(context.Background(), "", a2a.Task{A2ATaskID: "t-race"}); err == nil {
		t.Fatal("route into the SIGSTOP landing window must refuse — SIGCONT may not precede SIGSTOP")
	}

	// Once the SIGSTOP lands, the wake tier serves the same route.
	close(block)
	waitFor(t, 2*time.Second, func() bool {
		_, err := gw.Route(context.Background(), "", a2a.Task{A2ATaskID: "t-race"})
		return err == nil
	}, "route never succeeded after the SIGSTOP landed")
}

// blockingHibAdapter parks Hibernate on a channel so the test can hold the
// sweep inside the SIGSTOP landing window.
type blockingHibAdapter struct {
	*recordingAdapter
	blockHib chan struct{}
	once     sync.Once
	started  chan struct{}
}

func newBlockingHibAdapter(blockHib chan struct{}) *blockingHibAdapter {
	return &blockingHibAdapter{
		recordingAdapter: newRecordingAdapter(),
		blockHib:         blockHib,
		started:          make(chan struct{}),
	}
}

func (b *blockingHibAdapter) hibStarted() bool {
	select {
	case <-b.started:
		return true
	default:
		return false
	}
}

func (b *blockingHibAdapter) Hibernate(ctx context.Context, h mesh.Handle) error {
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.blockHib:
	case <-ctx.Done():
		return ctx.Err()
	}
	return b.recordingAdapter.Hibernate(ctx, h)
}

// TestS4_RebindAtWake: a wake whose envelope model DRIFTED since the process
// spawned is a respawn from the CURRENT envelope — never a stale-model
// SIGCONT, never a mid-stream rebind (X5: rebind-at-wake == respawn).
func TestS4_RebindAtWake(t *testing.T) {
	rec := newRecordingAdapter()
	spy := &hibernationSpy{recordingAdapter: rec}
	obs := &obsRecorder{}
	gw, err := mesh.NewGateway(mesh.GatewayConfig{
		Adapter: spy, Envelope: hibEnv(), Min: 1,
		Lifecycle: armedLifecycle(), Observer: obs,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gw.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool {
		return gw.Processes()[0].State == mesh.StateHibernated
	}, "process never hibernated")

	// The role's model moved while the process slept (the S4 drift source).
	moved := hibEnv()
	moved.Model = "model-b"
	gw.UpdateEnvelope(moved)

	s, err := gw.Route(context.Background(), "", a2a.Task{A2ATaskID: "t-rebind"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// The wake took the respawn path: Recreate ran with the CURRENT envelope…
	if n := rec.recreateCount(); n != 1 {
		t.Fatalf("recreate calls = %d, want 1 (rebind-at-wake == respawn)", n)
	}
	if got := rec.lastRecreate().Env.Model; got != "model-b" {
		t.Fatalf("recreate env model = %q, want model-b (the drifted envelope)", got)
	}
	// …the route still reads as the wake tier, and the wake event carries
	// Rebind=true with no SIGCONT latency point (the cost is the spawn).
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.routes) != 1 || obs.routes[0].Selection != mesh.SelectionWake {
		t.Fatalf("route selection = %+v, want selection=wake on the rebind path", obs.routes)
	}
	if len(obs.wakes) != 1 || !obs.wakes[0].Rebind {
		t.Fatalf("wake events = %+v, want one Rebind wake", obs.wakes)
	}
}

// TestS4_T_HIB_LEASE_01 is DoD gate D2 (chapter §13.6, ISI-5807 locked
// f93795d5): claim-never-frozen — no double-dispatch under ANY
// hibernate/sweep interleaving. The adapter below enforces the invariant set
// itself and fails the test on any violation:
//
//   - V1  a Route delivered to a SIGSTOP'd-not-yet-SIGCONT'd process (the
//     frozen-lease double-execution class, Test-F2/Coder-C3);
//   - V2  a second concurrent task on a process that already has one (the
//     double-dispatch the a2a_dispatch marker backstops — the gateway-local
//     guard #3 must never let it happen);
//   - V3  a Hibernate on a process with a live task (freezing a holder — the
//     D9.1 lease-while-frozen hazard).
//
// The test races routes, settles, and sweep ticks (tiny bounds ⇒ hibernate
// and stop transitions interleave with dispatch) for a sustained window and
// asserts zero violations, at least one hibernate, and at least one
// wake-served route.
func TestS4_T_HIB_LEASE_01(t *testing.T) {
	inv := newInvariantAdapter(t)
	gw, err := mesh.NewGateway(mesh.GatewayConfig{
		Adapter: inv, Envelope: hibEnv(), Min: 2,
		Lifecycle: mesh.LifecycleConfig{
			Hibernate:       true,
			CheckInterval:   2 * time.Millisecond,
			IdleToHibernate: 5 * time.Millisecond,
			HibernateToStop: 400 * time.Millisecond,
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

	const racers = 8
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				task := a2a.Task{A2ATaskID: fmt.Sprintf("t-lease-%d-%d", i, n)}
				s, err := gw.Route(context.Background(), "", task)
				if err == nil {
					// Hold the task briefly, then settle it — the stream
					// close releases the process back to idle/hibernate
					// eligibility while other racers keep dispatching.
					time.Sleep(time.Duration(n%3) * time.Millisecond)
					s.Close()
				}
				// Idle breathing room: long enough that the sweep (2ms
				// cadence) hibernates an idle process past the 5ms bound
				// between bursts — the hibernate/wake interleaving this
				// test exists to race. A pool hammered without gaps never
				// idles, and the tier would stay dark (correct, but
				// unexercised).
				time.Sleep(time.Duration(8+n%5) * time.Millisecond)
			}
		}(i)
	}
	time.Sleep(400 * time.Millisecond)
	close(stop)
	wg.Wait()

	inv.mu.Lock()
	defer inv.mu.Unlock()
	if inv.violations != nil {
		t.Fatalf("T-HIB-LEASE-01 violations: %v", inv.violations)
	}
	if inv.hibernates == 0 {
		t.Fatal("T-HIB-LEASE-01 never hibernated — the interleaving did not exercise the tier")
	}
	if inv.wakeRoutes == 0 {
		t.Fatal("T-HIB-LEASE-01 never served a wake route — the wake tier was not exercised")
	}
	if inv.routes == 0 {
		t.Fatal("T-HIB-LEASE-01 never routed — the race did not run")
	}
}

// invariantAdapter is the T-HIB-LEASE-01 oracle: it tracks per-handle
// frozen/live state and active-task counts, and records violations of the
// D2 invariant set the moment the adapter seam observes them.
type invariantAdapter struct {
	*fakeAdapter
	t  *testing.T
	mu sync.Mutex
	// frozen[id]: the handle is SIGSTOP'd (between Hibernate and Wake/Stop).
	frozen map[string]bool
	// tasks[id]: live routed tasks on the handle (V2: must never exceed 1).
	tasks map[string]int
	// violations accumulates V1/V2/V3 findings.
	violations []string

	routes     int
	hibernates int
	wakeRoutes int
	// pendingWake[id]: a Wake cleared the freeze — the next Route on the
	// handle is a wake delivery.
	pendingWake map[string]bool
}

func newInvariantAdapter(t *testing.T) *invariantAdapter {
	return &invariantAdapter{
		fakeAdapter: newFakeAdapter(), t: t,
		frozen: map[string]bool{}, tasks: map[string]int{},
		pendingWake: map[string]bool{},
	}
}

func (v *invariantAdapter) Hibernate(_ context.Context, h mesh.Handle) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	id := h.ID()
	if v.tasks[id] > 0 {
		v.violations = append(v.violations, fmt.Sprintf("V3: Hibernate on %s with %d live task(s) — froze a holder (D9.1)", id, v.tasks[id]))
	}
	v.frozen[id] = true
	v.hibernates++
	return nil
}

func (v *invariantAdapter) Wake(_ context.Context, h mesh.Handle) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	id := h.ID()
	delete(v.frozen, id)
	v.pendingWake[id] = true // the very next Route on this handle is a wake delivery
	return nil
}

func (v *invariantAdapter) Stop(_ context.Context, h mesh.Handle) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.frozen, h.ID())
	delete(v.pendingWake, h.ID())
	v.fakeAdapter.Stop(context.Background(), h)
	return nil
}

func (v *invariantAdapter) Route(ctx context.Context, h mesh.Handle, task a2a.Task) (mesh.Stream, error) {
	v.mu.Lock()
	id := h.ID()
	if v.frozen[id] {
		v.violations = append(v.violations, fmt.Sprintf("V1: Route of %s while SIGSTOP'd (task %s) — delivery to a frozen process", id, task.A2ATaskID))
	}
	v.tasks[id]++
	if v.tasks[id] > 1 {
		v.violations = append(v.violations, fmt.Sprintf("V2: second concurrent task on %s (task %s) — double dispatch", id, task.A2ATaskID))
	}
	v.routes++
	wasWake := v.pendingWake[id]
	if wasWake {
		v.wakeRoutes++
		delete(v.pendingWake, id)
	}
	v.mu.Unlock()

	stream, err := v.fakeAdapter.Route(ctx, h, task)
	if err != nil {
		v.mu.Lock()
		v.tasks[id]--
		v.mu.Unlock()
		return nil, err
	}
	// SettleAccounted stream: the task count drops synchronously at settle
	// (BEFORE the done channel closes, hence strictly before the gateway's
	// release() can un-busy the slot) — the V2 accounting can never lag a
	// subsequent dispatch the way a channel-watching goroutine could.
	return &settledAccountingStream{
		inner: stream,
		once:  sync.Once{},
		done: func() {
			v.mu.Lock()
			v.tasks[id]--
			v.mu.Unlock()
		},
	}, nil
}

// settledAccountingStream wraps a Stream and runs exactly one accounting
// callback at its first settle signal (Events closing or Close).
type settledAccountingStream struct {
	inner mesh.Stream
	once  sync.Once
	done  func()
}

func (s *settledAccountingStream) account() { s.once.Do(s.done) }

func (s *settledAccountingStream) Events() <-chan a2a.Event {
	ch := make(chan a2a.Event)
	go func() {
		defer close(ch)
		for ev := range s.inner.Events() {
			ch <- ev
		}
		s.account()
	}()
	return ch
}

func (s *settledAccountingStream) Status() a2a.Status { return s.inner.Status() }

func (s *settledAccountingStream) Close() error {
	s.account()
	return s.inner.Close()
}

var _ mesh.Stream = (*settledAccountingStream)(nil)

var _ mesh.RuntimeAdapter = (*invariantAdapter)(nil)
