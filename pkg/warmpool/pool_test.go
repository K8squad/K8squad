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

package warmpool_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/warmpool"
)

// fakeProvisioner records every physical call — the L1 stand-in for the kube
// pod adapter. boots/teardowns are keyed by sandbox id so tests can assert
// the WARM path made ZERO cluster calls (the S9/NFR-PERF1 claim-latency
// pin) and the cold path booted exactly once. blockOnBoot (when non-nil)
// parks every Boot until the channel closes — the in-flight-boot window
// the readiness/release races need; tearDownErr makes teardown fail (a
// failed teardown records nothing — it did not succeed).
type fakeProvisioner struct {
	mu          sync.Mutex
	boots       map[string]warmpool.PoolKey
	teardowns   map[string]int
	bootErr     error
	tearDownErr error
	blockOnBoot chan struct{}
}

func newFakeProvisioner() *fakeProvisioner {
	return &fakeProvisioner{boots: map[string]warmpool.PoolKey{}, teardowns: map[string]int{}}
}

func (f *fakeProvisioner) Boot(_ context.Context, key warmpool.PoolKey, id string) error {
	f.mu.Lock()
	if f.bootErr != nil {
		err := f.bootErr
		f.mu.Unlock()
		return err
	}
	f.boots[id] = key
	block := f.blockOnBoot
	f.mu.Unlock()
	if block != nil {
		<-block // the physical boot is slow / in flight
	}
	return nil
}

func (f *fakeProvisioner) TearDown(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tearDownErr != nil {
		return f.tearDownErr
	}
	f.teardowns[id]++
	return nil
}

func (f *fakeProvisioner) bootCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.boots)
}

func (f *fakeProvisioner) tornDown(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.teardowns[id] > 0
}

func (f *fakeProvisioner) teardownCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.teardowns[id]
}

// bootedCopy is a test helper snapshot of f.boots.
func (f *fakeProvisioner) bootsCopy() map[string]warmpool.PoolKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]warmpool.PoolKey, len(f.boots))
	for id, k := range f.boots {
		out[id] = k
	}
	return out
}

// waitBooted blocks until exactly one Boot call is in flight and returns
// its sandbox id (the cold-boot reservation's id).
func waitBooted(t *testing.T, fp *fakeProvisioner) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if boots := fp.bootsCopy(); len(boots) == 1 {
			for id := range boots {
				return id
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("cold boot never started")
	return ""
}

// newTestPool wires a Pool over a fake provisioner with a static key.
func newTestPool() (*warmpool.Pool, *fakeProvisioner) {
	fp := newFakeProvisioner()
	return warmpool.NewPool(fp), fp
}

// Prewarm boots n sandboxes for key and walks them to Ready — the pool
// state a warm claim expects.
func prewarm(t *testing.T, p *warmpool.Pool, key warmpool.PoolKey, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		id, err := p.Boot(context.Background(), key)
		if err != nil {
			t.Fatalf("prewarm boot: %v", err)
		}
		p.NotifyReady(id)
	}
}

// AC: "a Run reaches ClaimingSandbox → binds a pooled sandbox (claim-time,
// not cold-boot)". The warm path pops a Ready entry and makes ZERO
// provisioner calls — start latency on the warm path is grab-time
// (S9/NFR-PERF1). Differential twin: a cold-boot-on-every-bind binder calls
// Boot on the claim path (≥1 boots); the real warm bind calls none.
func TestPoolWarmBindIsClaimTime(t *testing.T) {
	key := gvisorKey
	pool, fp := newTestPool()
	prewarm(t, pool, key, 2)

	before := fp.bootCount()
	ref, err := pool.Bind(context.Background(), "run-1", key, warmpool.ClassInteractive)
	if err != nil {
		t.Fatalf("warm bind: %v", err)
	}
	if ref == "" {
		t.Fatal("warm bind returned an empty sandbox ref")
	}
	if got := fp.bootCount() - before; got != 0 {
		t.Fatalf("warm bind touched the provisioner %d times — claim-time violated (twin: cold-boot bind would call Boot ≥1)", got)
	}
	if c := pool.Inventory()[key]; c.Ready != 1 || c.Bound != 1 {
		t.Fatalf("post-bind inventory = %+v, want ready=1 bound=1", c)
	}
}

// AC: "or triggers scale-up if the pool is empty" — the empty-pool bind
// cold-boots a DEDICATED sandbox AND fires the bind-miss trigger so the
// controller replenishes immediately. Twin: a no-trigger pool leaves the
// miss unobserved (misses==0); the real pool reports it.
func TestPoolEmptyPoolColdBootsAndTriggersScaleUp(t *testing.T) {
	key := gvisorKey
	pool, fp := newTestPool()

	var misses []warmpool.PoolKey
	pool.SetBindMiss(func(k warmpool.PoolKey, _ warmpool.RunClass) { misses = append(misses, k) })

	ref, err := pool.Bind(context.Background(), "run-1", key, warmpool.ClassInteractive)
	if err != nil {
		t.Fatalf("cold bind: %v", err)
	}
	if fp.bootCount() != 1 {
		t.Fatalf("empty-pool bind booted %d sandboxes, want exactly 1 (the dedicated cold boot)", fp.bootCount())
	}
	if _, booted := fp.boots[ref]; !booted {
		t.Fatalf("returned ref %q was not the booted sandbox — cold bind must bind the sandbox it booted", ref)
	}
	if len(misses) != 1 || misses[0] != key {
		t.Fatalf("bind-miss trigger fired %+v, want exactly [%v] (twin: no trigger leaves the miss unobserved)", misses, key)
	}
	if c := pool.Inventory()[key]; c.Bound != 1 || c.Ready != 0 {
		t.Fatalf("post-cold-bind inventory = %+v, want bound=1 ready=0", c)
	}
}

// §9.2 hybrid regime: batch Runs cold-start (target 0 — zero idle cost, and
// sidesteps reuse-contamination) EVEN when warm entries exist for the key.
func TestPoolBatchClassColdStartsEvenWhenWarm(t *testing.T) {
	key := gvisorKey
	pool, fp := newTestPool()
	prewarm(t, pool, key, 3)
	before := fp.bootCount() // the 3 prewarm boots

	ref, err := pool.Bind(context.Background(), "run-batch", key, warmpool.ClassBatch)
	if err != nil {
		t.Fatalf("batch bind: %v", err)
	}
	if got := fp.bootCount() - before; got != 1 {
		t.Fatalf("batch bind booted %d sandboxes, want 1 (cold-start despite warm inventory)", got)
	}
	if c := pool.Inventory()[key]; c.Ready != 3 {
		t.Fatalf("batch bind consumed a warm entry: ready=%d, want 3 untouched", c.Ready)
	}
	if pool.Ref("run-batch") != ref {
		t.Fatal("batch run is not bound to the ref it booted")
	}
}

// §6.4 at-most-once: Bind is IDEMPOTENT on runID — a re-drive (the crash
// window ProdEffects re-enters) reattaches to the same sandbox_ref with no
// second boot and no second warm pop. Twin: a non-idempotent binder hands
// out a second ref / boots again.
func TestPoolBindIdempotentOnRunID(t *testing.T) {
	key := gvisorKey
	pool, fp := newTestPool()
	prewarm(t, pool, key, 1)

	first, err := pool.Bind(context.Background(), "run-1", key, warmpool.ClassInteractive)
	if err != nil {
		t.Fatalf("first bind: %v", err)
	}
	afterWarm := fp.bootCount() // prewarm(1) boot; the warm bind adds none
	// Re-drive while the first sandbox is live.
	again, err := pool.Bind(context.Background(), "run-1", key, warmpool.ClassInteractive)
	if err != nil {
		t.Fatalf("re-drive bind: %v", err)
	}
	if first != again {
		t.Fatalf("re-drive returned %q, want the SAME ref %q (reattach, never re-provision)", again, first)
	}
	if fp.bootCount() != afterWarm {
		t.Fatalf("re-drive booted again (%d boots, want %d) — at-most-once violated", fp.bootCount(), afterWarm)
	}
	// And the cold path: two binds racing the same un-warmed run reattach
	// to the SAME cold sandbox (the reservation is installed before Boot).
	c1 := make(chan string, 2)
	go func() {
		ref, err := pool.Bind(context.Background(), "run-cold", key, warmpool.ClassInteractive)
		if err == nil {
			c1 <- ref
		}
	}()
	ref, err := pool.Bind(context.Background(), "run-cold", key, warmpool.ClassInteractive)
	if err != nil {
		t.Fatalf("cold bind: %v", err)
	}
	if other, ok := <-c1; ok && other != ref {
		t.Fatalf("concurrent cold binds diverged: %q vs %q — the reservation must make them reattach", other, ref)
	}
	if fp.bootCount() != afterWarm+1 {
		t.Fatalf("concurrent cold binds produced %d boots, want %d (one cold boot)", fp.bootCount(), afterWarm+1)
	}
}

// §9.3 teardown-and-replace: Release DESTROYS the sandbox (never returns it
// to Ready — reuse is structurally impossible), is idempotent, and the
// replacement arrives as a FRESH boot. Twin: a reuse-pool that recycles the
// released sandbox would serve it to the next claim (same ref twice); the
// real pool never does.
func TestPoolReleaseTeardownAndReplace(t *testing.T) {
	key := gvisorKey
	pool, fp := newTestPool()
	prewarm(t, pool, key, 1)

	ref, err := pool.Bind(context.Background(), "run-1", key, warmpool.ClassInteractive)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := pool.Release(context.Background(), "run-1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if !fp.tornDown(ref) {
		t.Fatal("release did not tear the sandbox down")
	}
	// Idempotent teardown: a second Release is a structural no-op.
	if err := pool.Release(context.Background(), "run-1"); err != nil {
		t.Fatalf("second release: %v", err)
	}
	if fp.teardowns[ref] != 1 {
		t.Fatalf("sandbox torn down %d times, want 1 (idempotent)", fp.teardowns[ref])
	}
	if pool.Ref("run-1") != "" {
		t.Fatal("released run still holds a ref")
	}
	// The NEXT claim on a replenished pool must never see the destroyed id.
	fresh, err := pool.Boot(context.Background(), key)
	if err != nil {
		t.Fatalf("replacement boot: %v", err)
	}
	if fresh == ref {
		t.Fatal("replacement boot reused the released sandbox id — teardown-and-replace violated")
	}
}

// Boot failure on the cold path rolls the reservation back so a retry can
// bind afresh (the 0007 marker is written only on success — no orphan).
func TestPoolColdBootFailureRollsBack(t *testing.T) {
	key := gvisorKey
	pool, fp := newTestPool()
	fp.bootErr = errors.New("cluster down")

	if _, err := pool.Bind(context.Background(), "run-1", key, warmpool.ClassInteractive); err == nil {
		t.Fatal("bind succeeded despite provisioner failure")
	}
	if pool.Ref("run-1") != "" {
		t.Fatal("failed bind left a run→sandbox reservation behind")
	}
	if c := pool.Inventory()[key]; c.Warming+c.Ready+c.Bound != 0 {
		t.Fatalf("failed bind leaked inventory: %+v", c)
	}
	// Recovery: clear the failure, retry binds.
	fp.bootErr = nil
	if _, err := pool.Bind(context.Background(), "run-1", key, warmpool.ClassInteractive); err != nil {
		t.Fatalf("retry after recovery: %v", err)
	}
	if pool.Ref("run-1") == "" {
		t.Fatal("retry did not bind")
	}
}

// ScaleDown destroys only READY entries, oldest first, and never touches a
// Bound sandbox — an in-flight claim cannot lose its sandbox to a scale-down.
func TestPoolScaleDownSparesBound(t *testing.T) {
	key := gvisorKey
	pool, fp := newTestPool()
	prewarm(t, pool, key, 2)

	boundRef, err := pool.Bind(context.Background(), "run-1", key, warmpool.ClassInteractive)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if n := pool.ScaleDown(context.Background(), key, 5); n != 1 {
		t.Fatalf("scale-down destroyed %d, want 1 (only the Ready entry)", n)
	}
	if fp.tornDown(boundRef) {
		t.Fatal("scale-down tore down a BOUND sandbox")
	}
	if pool.Ref("run-1") != boundRef {
		t.Fatal("scale-down detached a bound run")
	}
}

// The coord.SandboxBinder adapter: NewBinder + DefaultClassifier satisfies
// the seam ProdEffects drives (structural signature pin in pool.go) and
// delegates with the same idempotency.
func TestBinderAdaptsPoolToCoordSeam(t *testing.T) {
	key := gvisorKey
	pool, _ := newTestPool()
	prewarm(t, pool, key, 1)

	binder := warmpool.NewBinder(pool, warmpool.DefaultClassifier(key, warmpool.ClassInteractive))
	ref1, err := binder.Bind(context.Background(), "11111111-1111-1111-1111-111111111111")
	if err != nil {
		t.Fatalf("binder bind: %v", err)
	}
	ref2, err := binder.Bind(context.Background(), "11111111-1111-1111-1111-111111111111")
	if err != nil {
		t.Fatalf("binder re-drive: %v", err)
	}
	if ref1 != ref2 {
		t.Fatalf("binder re-drive returned %q, want %q", ref2, ref1)
	}
}

// Review blocker (PR #91): a readiness event for a Run's DEDICATED cold
// sandbox — the kube pod-watch fires it while the cold boot is still in
// flight — must NEVER promote that sandbox into the Ready FIFO. The
// state-only-guard twin promotes it, and the next interactive claim hands
// the SAME sandbox to a second Run: the cross-run double hand-out §9.3
// (reuse-contamination) exists to make impossible.
func TestPoolNotifyReadyNeverPromotesRunReservedSandbox(t *testing.T) {
	key := gvisorKey
	pool, fp := newTestPool()
	fp.blockOnBoot = make(chan struct{})

	bindDone := make(chan error, 1)
	go func() {
		_, err := pool.Bind(context.Background(), "run-1", key, warmpool.ClassInteractive)
		bindDone <- err
	}()

	// Fire the readiness event for run-1's reserved sandbox DURING the
	// in-flight boot — the exact production (pod-watch) window.
	id := waitBooted(t, fp)
	pool.NotifyReady(id)
	if inv := pool.Inventory()[key]; inv.Ready != 0 || inv.Reserved != 1 {
		t.Fatalf("in-flight NotifyReady inventory = %+v, want reserved=1 ready=0 (a run-owned sandbox is never claimable warmth)", inv)
	}

	close(fp.blockOnBoot)
	if err := <-bindDone; err != nil {
		t.Fatalf("cold bind: %v", err)
	}
	if pool.Ref("run-1") != id {
		t.Fatal("run-1 lost the sandbox it reserved")
	}

	// The next claim must receive its OWN sandbox — never run-1's.
	ref2, err := pool.Bind(context.Background(), "run-2", key, warmpool.ClassInteractive)
	if err != nil {
		t.Fatalf("second bind: %v", err)
	}
	if ref2 == id {
		t.Fatalf("double hand-out: run-2 received run-1's sandbox %q", ref2)
	}
	if inv := pool.Inventory()[key]; inv.Bound != 2 {
		t.Fatalf("post-claim inventory = %+v, want bound=2 (each run its own sandbox)", inv)
	}
}

// Review blocker (PR #91): a FAILED teardown must not orphan the pod.
// Release returns the error AND keeps the entry tracked so a retry
// re-attempts the teardown. The delete-first twin untracks the sandbox
// anyway — a retry then reports false success while the workspace pod
// keeps running untracked.
func TestPoolReleaseTearDownFailureStaysTracked(t *testing.T) {
	key := gvisorKey
	pool, fp := newTestPool()
	prewarm(t, pool, key, 1)
	ref, err := pool.Bind(context.Background(), "run-1", key, warmpool.ClassInteractive)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}

	fp.tearDownErr = errors.New("pod delete: 503")
	if err := pool.Release(context.Background(), "run-1"); err == nil {
		t.Fatal("release reported success despite the teardown failure")
	}
	if pool.Ref("run-1") != ref {
		t.Fatal("failed teardown untracked the sandbox — a retry cannot re-attempt it")
	}
	if inv := pool.Inventory()[key]; inv.Bound != 1 {
		t.Fatalf("inventory after failed teardown = %+v, want bound=1 (the pod is alive — stay truthful)", inv)
	}

	// The API recovers: the retry tears the SAME sandbox down.
	fp.tearDownErr = nil
	if err := pool.Release(context.Background(), "run-1"); err != nil {
		t.Fatalf("retry release: %v", err)
	}
	if !fp.tornDown(ref) {
		t.Fatal("retry did not tear the sandbox down")
	}
	if pool.Ref("run-1") != "" {
		t.Fatal("successful retry left the ref bound")
	}
	if inv := pool.Inventory()[key]; inv.Warming+inv.Ready+inv.Bound+inv.Reserved != 0 {
		t.Fatalf("post-retry inventory = %+v, want empty", inv)
	}
}

// Review blocker (PR #91), ScaleDown arm: a failed teardown during
// scale-down must not orphan the pod. The delete-first twin returns 0
// destroyed but has already forgotten the victims; the fixed pool
// re-tracks the failed entry so the next pass retries it.
func TestPoolScaleDownTearDownFailureRequeues(t *testing.T) {
	key := gvisorKey
	pool, fp := newTestPool()
	prewarm(t, pool, key, 2)

	fp.tearDownErr = errors.New("pod delete: 503")
	if n := pool.ScaleDown(context.Background(), key, 2); n != 0 {
		t.Fatalf("scale-down destroyed %d against a failing provisioner, want 0 confirmed", n)
	}
	if inv := pool.Inventory()[key]; inv.Ready != 2 {
		t.Fatalf("failed teardown dropped live entries: %+v, want ready=2 (still tracked)", inv)
	}

	fp.tearDownErr = nil
	if n := pool.ScaleDown(context.Background(), key, 2); n != 2 {
		t.Fatalf("retry scale-down destroyed %d, want 2", n)
	}
	if inv := pool.Inventory()[key]; inv.Ready != 0 {
		t.Fatalf("post-retry inventory = %+v, want ready=0", inv)
	}
}

// Review blocker (PR #91), release-during-in-flight-cold-boot race:
// Release's teardown can land BEFORE the concurrent cold Boot creates the
// pod — the Boot then completes with its reservation already gone and
// would return success for a live pod nobody tracks. The fixed Bind
// re-checks the reservation after Boot, destroys the orphan, and fails.
func TestPoolReleaseDuringInFlightColdBootDestroysOrphan(t *testing.T) {
	key := gvisorKey
	pool, fp := newTestPool()
	fp.blockOnBoot = make(chan struct{})

	bindDone := make(chan error, 1)
	go func() {
		_, err := pool.Bind(context.Background(), "run-1", key, warmpool.ClassInteractive)
		bindDone <- err
	}()
	id := waitBooted(t, fp)

	// The run is released while its dedicated boot is still in flight.
	if err := pool.Release(context.Background(), "run-1"); err != nil {
		t.Fatalf("release during in-flight boot: %v", err)
	}
	close(fp.blockOnBoot)

	if err := <-bindDone; err == nil {
		t.Fatal("bind succeeded although the run was released mid-boot — untracked live pod")
	}
	if got := fp.teardownCount(id); got != 2 {
		t.Fatalf("sandbox %s torn down %d times, want 2 (release + the orphan cleanup once Boot completed)", id, got)
	}
	if inv := pool.Inventory()[key]; inv.Warming+inv.Ready+inv.Bound+inv.Reserved != 0 {
		t.Fatalf("post-race inventory = %+v, want empty", inv)
	}
	if pool.Ref("run-1") != "" {
		t.Fatal("released run still holds a ref after the race")
	}
}

// Unknown run classes fail CLOSED (matching Policy.Target) — an unknown
// class must never be silently routed into a warm/cold regime.
func TestPoolBindFailsClosedOnUnknownRunClass(t *testing.T) {
	key := gvisorKey
	pool, fp := newTestPool()
	prewarm(t, pool, key, 1)

	if _, err := pool.Bind(context.Background(), "run-x", key, warmpool.RunClass("serverless")); err == nil {
		t.Fatal("bind accepted an unknown run class — fail-open violates the policy contract")
	}
	if got := fp.bootCount(); got != 1 { // the prewarm boot only
		t.Fatalf("unknown-class bind touched the provisioner %d times, want the 1 prewarm boot only", got)
	}
	if inv := pool.Inventory()[key]; inv.Ready != 1 {
		t.Fatalf("unknown-class bind consumed warmth: %+v", inv)
	}
}

// Surface reads the review's coverage list called out (PR #91): the
// SetClock injection and per-boot id uniqueness.
func TestPoolSetClockAndIDUniqueness(t *testing.T) {
	key := gvisorKey
	pool, _ := newTestPool()
	now := time.Unix(1_750_000_000, 0)
	pool.SetClock(func() time.Time { return now })

	id1, err := pool.Boot(context.Background(), key)
	if err != nil {
		t.Fatalf("boot 1: %v", err)
	}
	pool.NotifyReady(id1)
	id2, err := pool.Boot(context.Background(), key)
	if err != nil {
		t.Fatalf("boot 2: %v", err)
	}
	if id1 == id2 {
		t.Fatalf("two boots minted the same id %q", id1)
	}
	if inv := pool.Inventory()[key]; inv.Ready != 1 || inv.Warming != 1 {
		t.Fatalf("inventory = %+v, want ready=1 warming=1", inv)
	}
}
