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

	"github.com/K8squad/K8squad/pkg/warmpool"
)

// fakeProvisioner records every physical call — the L1 stand-in for the kube
// pod adapter. boots/teardowns are keyed by sandbox id so tests can assert
// the WARM path made ZERO cluster calls (the S9/NFR-PERF1 claim-latency
// pin) and the cold path booted exactly once.
type fakeProvisioner struct {
	mu        sync.Mutex
	boots     map[string]warmpool.PoolKey
	teardowns map[string]int
	bootErr   error
}

func newFakeProvisioner() *fakeProvisioner {
	return &fakeProvisioner{boots: map[string]warmpool.PoolKey{}, teardowns: map[string]int{}}
}

func (f *fakeProvisioner) Boot(_ context.Context, key warmpool.PoolKey, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bootErr != nil {
		return f.bootErr
	}
	f.boots[id] = key
	return nil
}

func (f *fakeProvisioner) TearDown(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
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
