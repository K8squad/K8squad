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

// pool.go — the §9.2/§9.3 physical warm-pool MECHANISM (Story 3.4 / ISI-2885).
// sizing.go (Story 3.5) is the POLICY: the one place the target N is derived.
// This file is the mechanism that policy drives — mirroring the split the
// sizing.go header states ("the Story 3.4 controller mechanism (claim-time
// bind, scale-up trigger, replenish-toward-target) consumes the target
// produced here opaquely").
//
// Three pieces land here:
//
//   - Provisioner — the physical sandbox boot/teardown seam. Production
//     adapter: kube-client pod create/delete carrying the key's RuntimeClass
//     and AgentRuntime image (§9.1/§9.2, arch table row `SandboxPool`);
//     tests: an in-memory fake. The pool NEVER talks to the cluster except
//     through this seam.
//   - Pool — the per-(RuntimeClass × image) warm inventory: Ready sandboxes
//     waiting to be claimed (claim-time bind), the run→sandbox index that
//     makes Bind idempotent (reattach, §6.4 at-most-once), and the
//     teardown-and-replace release path (§9.3 — a released sandbox is
//     DESTROYED, never returned to Ready).
//   - Binder — the coord.SandboxBinder adapter (pkg/coord/prodeffects.go)
//     that feeds Pool.Bind to ProdEffects — the physical binder the 0007
//     migration comment defers to Story 3.4. Constructing it in the
//     production assembly (over the kube provisioner) is the execution-
//     layer adapter's job; until that lands, ProdEffects keeps its
//     documented ledger-only default.
//
// The load-bearing property is the §6.4/0007 contract: Bind is idempotent on
// runID. ProdEffects gates the physical call behind the coord.sandbox_bind
// marker (run_id PK) for crash-safety across processes; Pool enforces the
// SAME keying in memory so the two guards compose — a re-drive reattaches to
// the same sandbox_ref instead of double-provisioning (the comment on
// ProdEffects.BindSandbox relies on exactly this: "the physical binder
// (itself run_id-keyed) is called at most once").
//
// Warm path latency (S9, NFR-PERF1): a warm Bind pops a Ready entry under
// the pool mutex and touches NO cluster API — the provisioner is not on the
// claim critical path (pool_controller_test.go pins this: zero Boot calls on
// a warm hit). Cold path (empty pool, or §9.2 batch class): Boot a dedicated
// sandbox synchronously and trigger the scale-up callback so the controller
// replenishes toward target immediately.
package warmpool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// SandboxState is the §9.2/§9.3 lifecycle of one physical warm-pool sandbox.
//
//	Warming → Ready → Bound → (Release → destroyed)
//	    └──────────────────────→ Bound (cold path: bound directly, never Ready)
type SandboxState string

const (
	// StateWarming: booted, not yet Ready (image pull / sentry start). A
	// Warming entry with BoundRun set is the cold path's Run-reserved
	// boot in flight — it never becomes claimable warmth.
	StateWarming SandboxState = "warming"

	// StateReady: pre-booted, image-pre-pulled, claimable at grab-time.
	StateReady SandboxState = "ready"

	// StateBound: claimed by exactly one Run (via its runID).
	StateBound SandboxState = "bound"

	// StateDraining: a teardown was ISSUED but not confirmed. The pod may
	// or may not still be alive (a timeout is not proof the delete did
	// not land) — so a draining sandbox is NEVER claimable warmth, never
	// counted as live pool capacity, and stays tracked until a retry
	// confirms the teardown. This is the state a failed TearDown parks
	// its victim in: recovery must not re-arm a delete-attempted sandbox
	// as Ready (§9.3 reuse-contamination through the back door).
	StateDraining SandboxState = "draining"
)

// Sandbox is one physical warm-pool sandbox pod (§9.2): it carries only the
// agent base — skill toolchains attach per-Run via init packs, which is why
// the pool keys stay one-dimensional (RuntimeClass × image).
type Sandbox struct {
	// ID is the pool-assigned opaque identifier (also the sandbox_ref the
	// binder returns; the kube adapter stamps it as the pod name).
	ID string

	// Key is the (RuntimeClass × AgentRuntime image) pool this sandbox
	// belongs to — sized by its OWN measured replenish time (sizing.go).
	Key PoolKey

	// State is the lifecycle state above.
	State SandboxState

	// BoundRun is the runID holding this sandbox — set from the moment the
	// cold path RESERVES it (while still StateWarming) so a readiness
	// event can never hand another claim a sandbox a Run already owns.
	BoundRun string

	// CreatedAt / ReadyAt record the boot and readiness instants (FIFO
	// drain order and replenish-duration observability, obs §5.3).
	CreatedAt time.Time
	ReadyAt   time.Time
}

// Provisioner is the physical sandbox seam (Story 3.4): everything the pool
// does to the cluster goes through here, so the mechanism is provable L1
// against a fake and the kube adapter is a drop-in (pod create/delete with
// runtimeClassName + the key's image, §9.1 assembly).
//
// Both methods must be safe for concurrent use. Boot returns WITHOUT waiting
// for readiness — readiness is reported to the pool via NotifyReady (the
// kube adapter calls it from the pod watch), keeping warm-up off every
// synchronous path.
type Provisioner interface {
	// Boot starts a fresh sandbox pod for key under the pool-assigned id.
	Boot(ctx context.Context, key PoolKey, sandboxID string) error

	// TearDown destroys the sandbox pod (§9.3 teardown-and-replace: the
	// pod is the disposable unit; a sandbox is NEVER reused across Runs).
	TearDown(ctx context.Context, sandboxID string) error
}

// BindMissFunc is the scale-up trigger the controller registers (Story 3.4
// AC: "or triggers scale-up if the pool is empty"): the pool fires it
// whenever a bind had to cold-boot because no Ready entry existed for the
// key (or the run class cold-starts by policy). It is invoked OUTSIDE the
// pool mutex — re-entering Pool methods from it is safe (they take the
// lock themselves); only the firing Bind itself must not be re-entered.
type BindMissFunc func(key PoolKey, class RunClass)

// Pool is the physical warm-pool inventory. It is the claim-time source of
// sandboxes (Ready entries) and the runID→sandbox bookkeeping that makes
// Bind idempotent. All exported methods are safe for concurrent use.
type Pool struct {
	provisioner Provisioner
	now         func() time.Time

	// onBindMiss is the controller's scale-up trigger (nil = no trigger).
	onBindMiss BindMissFunc

	mu sync.Mutex

	// entries is every live (non-destroyed) sandbox by id.
	entries map[string]*Sandbox

	// ready is the per-key FIFO of Ready entry ids (front = oldest warm).
	ready map[PoolKey][]string

	// byRun indexes Bound sandboxes by runID — the §6.4 idempotency key.
	byRun map[string]*Sandbox

	// draining is the per-key retry queue of sandboxes whose teardown was
	// issued but not confirmed (StateDraining). Retried by ScaleDown.
	draining map[PoolKey][]*Sandbox

	// seq disambiguates sandbox ids within this process.
	seq uint64
}

// NewPool returns a Pool driving provisioner. provisioner may be nil — that
// selects a LEDGER-ONLY pool (Bind still records/reattaches markers and the
// warm path still pops Ready entries, but cold binds synthesize a handle
// without any physical boot) — useful for the same ledger-first mode
// ProdEffects documents ahead of the kube adapter.
func NewPool(provisioner Provisioner) *Pool {
	return &Pool{
		provisioner: provisioner,
		now:         time.Now,
		entries:     make(map[string]*Sandbox),
		ready:       make(map[PoolKey][]string),
		byRun:       make(map[string]*Sandbox),
		draining:    make(map[PoolKey][]*Sandbox),
	}
}

// SetBindMiss registers the controller's scale-up trigger (Story 3.4). The
// callback is invoked WITHOUT the pool mutex held — re-entering Pool methods
// from it is safe (they take the lock themselves).
func (p *Pool) SetBindMiss(fn BindMissFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onBindMiss = fn
}

// SetClock overrides the time source (tests / fake clocks).
func (p *Pool) SetClock(now func() time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.now = now
}

// newID mints an opaque, collision-resistant sandbox id (64 random bits,
// hex-encoded; falls back to a time+seq id if the system entropy source
// fails).
func (p *Pool) newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		p.seq++
		return fmt.Sprintf("sandbox-%d-%d", p.now().UnixNano(), p.seq)
	}
	return "sandbox-" + hex.EncodeToString(b[:])
}

// Bind provisions or reattaches the sandbox for runID (the §6.4 physical
// half of coord.ProdEffects.BindSandbox). Exactly one of:
//
//   - runID is already bound → return its sandbox_ref (reattach; no state
//     change, no cluster call) — the at-most-once contract.
//   - class interactive AND a Ready entry exists for key → pop it (FIFO),
//     mark Bound(runID), return its ref. This is the WARM path: grab-time,
//     no provisioner call (S9 / NFR-PERF1).
//   - otherwise (pool empty, or class batch per §9.2 hybrid regime) →
//     COLD path: Boot a dedicated sandbox for runID and bind it directly
//     (never passing through Ready), fire the scale-up trigger, return ref.
//
// The byRun index entry is installed BEFORE the physical Boot so a Bind
// re-entered while the boot is still in flight reattaches to the same ref.
func (p *Pool) Bind(ctx context.Context, runID string, key PoolKey, class RunClass) (string, error) {
	if runID == "" {
		return "", fmt.Errorf("warmpool.Pool.Bind: runID is required")
	}
	p.mu.Lock()

	// Reattach FIRST: a run that already owns a sandbox must always be
	// able to recover its ref — validation errors must never strand a
	// bound pod (the guard below only gates NEW binds).
	if sb, ok := p.byRun[runID]; ok {
		ref := sb.ID
		p.mu.Unlock()
		return ref, nil
	}

	if class != ClassInteractive && class != ClassBatch {
		// Fail closed like Policy.Target — an unknown class must never be
		// silently treated as a warm-eligible (or cold) regime.
		p.mu.Unlock()
		return "", fmt.Errorf("warmpool.Pool.Bind: unknown run class %q (want %q or %q)", class, ClassInteractive, ClassBatch)
	}

	// Warm path: pop the OLDEST Ready entry for the key (FIFO — warmed
	// investment is spent first, §9.2 base-stock).
	if ids := p.ready[key]; class == ClassInteractive && len(ids) > 0 {
		id := ids[0]
		p.ready[key] = ids[1:]
		sb, ok := p.entries[id]
		if !ok {
			// Defensive: a ready id must exist in entries; skip the
			// phantom slot instead of panicking on the claim hot path.
			p.mu.Unlock()
			return p.Bind(ctx, runID, key, class)
		}
		sb.State = StateBound
		sb.BoundRun = runID
		p.byRun[runID] = sb
		ref := sb.ID
		p.mu.Unlock()
		return ref, nil // claim-time: no cluster call on this path
	}

	// Cold path: scale-up territory. Reserve the run→sandbox edge first so
	// a concurrent/re-entered Bind reattaches instead of double-booting.
	id := p.newID()
	sb := &Sandbox{
		ID:        id,
		Key:       key,
		State:     StateWarming,
		BoundRun:  runID,
		CreatedAt: p.now(),
	}
	p.entries[id] = sb
	p.byRun[runID] = sb
	miss := p.onBindMiss
	p.mu.Unlock()

	// The claiming run's own boot goes FIRST — the miss trigger below fans
	// out up to maxBootPerTick replenish boots, and the run's cold start
	// (S9) must not queue behind them.
	bootErr := error(nil)
	if p.provisioner != nil {
		bootErr = p.provisioner.Boot(ctx, key, id)
	}
	fireMiss := func() {
		if miss != nil {
			miss(key, class) // the controller's immediate scale-up trigger
		}
	}

	p.mu.Lock()
	if bootErr != nil {
		// Boot failed: roll the reservation back so a retry can try
		// afresh (the caller's durable marker — 0007 — was not yet
		// written, so no orphan marker exists).
		delete(p.entries, id)
		delete(p.byRun, runID)
		p.mu.Unlock()
		fireMiss() // the pool is still empty — the controller should still replenish
		return "", fmt.Errorf("warmpool.Pool.Bind: cold boot %s: %w", id, bootErr)
	}
	if _, tracked := p.entries[id]; !tracked {
		// Released while the boot was in flight: Release's TearDown ran
		// BEFORE this Boot created the pod, so the pod now exists with
		// nothing tracking it. Destroy it and fail the bind — never leave
		// an untracked live pod. Cleanup runs on a DETACHED context (the
		// caller's ctx is frequently the cancelled context that caused
		// the bind to be abandoned) and a FAILED cleanup keeps the pod
		// tracked as Draining so a later ScaleDown retry reclaims it —
		// the caller is told cleanup failed, never told "destroyed"
		// while the pod lives.
		p.mu.Unlock()
		cleanupErr := error(nil)
		if p.provisioner != nil {
			cleanupCtx := context.WithoutCancel(ctx)
			cleanupErr = p.provisioner.TearDown(cleanupCtx, id)
		}
		if cleanupErr != nil {
			p.mu.Lock()
			sb.State = StateDraining
			sb.BoundRun = "" // the run is free; the pod is un-owned debt
			p.entries[id] = sb
			p.draining[key] = append(p.draining[key], sb)
			p.mu.Unlock()
			fireMiss()
			return "", fmt.Errorf("warmpool.Pool.Bind: run %s released while its cold boot %s was in flight; cleanup of the orphaned sandbox FAILED (pod tracked as draining for retry): %w", runID, id, cleanupErr)
		}
		fireMiss()
		return "", fmt.Errorf("warmpool.Pool.Bind: run %s released while its cold boot %s was in flight; the orphaned sandbox was destroyed", runID, id)
	}
	sb.State = StateBound
	p.mu.Unlock()
	fireMiss()
	return id, nil
}

// NotifyReady reports that a Warming sandbox finished booting (the kube
// adapter calls this from the pod watch). A POOL-REPLENISH entry (BoundRun
// empty) joins its key's Ready FIFO — replenish-toward-target made visible
// to claims. A run-RESERVED entry (the cold path's dedicated boot, BoundRun
// set) records its readiness instant but is never promoted: handing a
// sandbox one Run already owns into the Ready FIFO would hand the same
// sandbox to the next claim — the cross-run double hand-out §9.3
// (reuse-contamination) exists to make impossible; Bind completes the
// Warming→Bound transition itself. Unknown or non-Warming ids are ignored
// (idempotent, late arrivals after scale-down).
func (p *Pool) NotifyReady(sandboxID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sb, ok := p.entries[sandboxID]
	if !ok || sb.State != StateWarming {
		return
	}
	if sb.BoundRun != "" {
		sb.ReadyAt = p.now() // readiness observed — but never claimable warmth
		return
	}
	sb.State = StateReady
	sb.ReadyAt = p.now()
	p.ready[sb.Key] = append(p.ready[sb.Key], sb.ID)
}

// Release destroys the sandbox bound to runID — §9.3 teardown-and-replace:
// the pod is destroyed and a FRESH one is replenished by the controller's
// next pass toward target; the released sandbox NEVER re-enters Ready
// (reuse-contamination is structurally impossible). A second Release for
// the same runID is a no-op (idempotent teardown).
//
// If the physical TearDown FAILS, the sandbox is parked in StateDraining
// (tracked — inventory stays truthful — and retried by ScaleDown) and
// Release returns the error instead of a false success. byRun is restored
// ONLY if the run has not re-bound in the window: clobbering a newer
// binding would orphan the run's live sandbox from every index.
func (p *Pool) Release(ctx context.Context, runID string) error {
	p.mu.Lock()
	sb, ok := p.byRun[runID]
	if !ok {
		p.mu.Unlock()
		return nil // already released (or never bound) — no-op
	}
	id, key := sb.ID, sb.Key
	delete(p.byRun, runID)
	delete(p.entries, id)
	// Defensively drop the id from any stale Ready slot (cannot happen for
	// a Bound entry; guards a future state-machine change).
	if ids := p.ready[key]; len(ids) > 0 {
		kept := ids[:0]
		for _, x := range ids {
			if x != id {
				kept = append(kept, x)
			}
		}
		p.ready[key] = kept
	}
	p.mu.Unlock()

	if p.provisioner != nil {
		if err := p.provisioner.TearDown(ctx, id); err != nil {
			// Teardown failed: the pod may still be alive. Keep it
			// tracked as Draining (never claimable) so a later pass
			// re-attempts the teardown — never orphan a live pod the
			// pool has forgotten. Restore the run's index slot ONLY if
			// the run has not already re-bound to a fresh sandbox.
			p.mu.Lock()
			sb.State = StateDraining
			if _, rebound := p.byRun[runID]; !rebound {
				p.byRun[runID] = sb // keep BoundRun: a retry re-attempts this teardown
			} else {
				sb.BoundRun = "" // the run owns a newer sandbox; this pod is un-owned debt
			}
			p.entries[id] = sb
			p.draining[key] = append(p.draining[key], sb)
			p.mu.Unlock()
			return fmt.Errorf("warmpool.Pool.Release: teardown %s: %w", id, err)
		}
	}
	return nil
}

// Counts returns the per-key live inventory snapshot the controller's
// reconcile reads.
type Counts struct {
	// Warming: pool-replenish boots in flight (not yet Ready, claimed by
	// no one) — these count toward the reconcile's live count.
	Warming int

	// Ready: pre-booted, claimable at grab-time — the warm inventory.
	Ready int

	// Bound: claimed by exactly one Run.
	Bound int

	// Reserved: warming entries that are a specific Run's DEDICATED cold
	// boot materializing (BoundRun set). They are the run's own sandbox,
	// not pool warmth: the controller does NOT count them toward live
	// (claiming one must not shrink the replenish deficit), and
	// NotifyReady never promotes them into Ready.
	Reserved int

	// Draining: sandboxes whose teardown was issued but not confirmed.
	// Never claimable, never counted toward live; retried by ScaleDown
	// until the teardown confirms. This is leaked-capacity debt made
	// visible — the number the kube adapter's alerts will watch.
	Draining int
}

// Inventory returns the per-key live counts (the controller's observed
// state). Deterministic key order is the caller's concern (maps iterate
// unordered); the controller registers its own key set.
func (p *Pool) Inventory() map[PoolKey]Counts {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[PoolKey]Counts)
	for _, sb := range p.entries {
		c := out[sb.Key]
		switch {
		case sb.State == StateWarming && sb.BoundRun != "":
			c.Reserved++
		case sb.State == StateWarming:
			c.Warming++
		case sb.State == StateReady:
			c.Ready++
		case sb.State == StateBound:
			c.Bound++
		case sb.State == StateDraining:
			c.Draining++
		}
		out[sb.Key] = c
	}
	return out
}

// Ref returns the sandbox_ref currently bound to runID ("" when unbound) —
// the reconciliation read for callers that need to reattach by ref.
func (p *Pool) Ref(runID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if sb, ok := p.byRun[runID]; ok {
		return sb.ID
	}
	return ""
}

// retryDraining re-attempts the teardown of every draining sandbox for key
// (teardown previously issued but unconfirmed). Confirmed teardowns are
// untracked; still-failing entries stay Draining. Returns how many were
// reclaimed. A wedged entry cannot block anything: it never re-enters the
// Ready FIFO (a delete-attempted sandbox is never re-armed as claimable
// warmth) and healthy victim selection in ScaleDown never queues behind it.
//
// Must NOT be called with p.mu held.
func (p *Pool) retryDraining(ctx context.Context, key PoolKey) int {
	p.mu.Lock()
	var attempt []*Sandbox
	for _, sb := range p.draining[key] {
		if cur, ok := p.entries[sb.ID]; ok && cur.State == StateDraining {
			attempt = append(attempt, cur)
		}
	}
	p.draining[key] = nil // rebuilt after the physical calls
	p.mu.Unlock()

	reclaimed := 0
	var still []*Sandbox
	for _, sb := range attempt {
		if p.provisioner == nil {
			reclaimed++ // ledger-only: nothing physical to confirm
			continue
		}
		if err := p.provisioner.TearDown(ctx, sb.ID); err != nil {
			still = append(still, sb)
			continue
		}
		reclaimed++
	}

	p.mu.Lock()
	// Re-adopt entries that drained while the calls ran (failed Release
	// restore, concurrent ScaleDown victim), keep the still-failing ones,
	// untrack the confirmed ones.
	p.draining[key] = append(still, p.draining[key]...)
	keep := make(map[string]struct{}, len(still))
	for _, sb := range still {
		keep[sb.ID] = struct{}{}
	}
	for _, sb := range attempt {
		if _, kept := keep[sb.ID]; !kept {
			if cur, ok := p.entries[sb.ID]; ok && cur.State == StateDraining {
				delete(p.entries, sb.ID)
			}
		}
	}
	p.mu.Unlock()
	return reclaimed
}

// ScaleDown destroys up to n Ready entries for key (oldest first — spent
// warmth drains before fresh), returning how many were actually destroyed
// (confirmed teardowns — draining retries plus fresh victims). The
// controller calls this after the autoscaler's stabilization band commits a
// lower target; destroying only Ready entries means an in-flight claim
// never loses its sandbox. A teardown that FAILS parks its victim in
// StateDraining: tracked, never claimable, retried by the next pass, and
// unable to block the destruction of healthy surplus.
func (p *Pool) ScaleDown(ctx context.Context, key PoolKey, n int) int {
	if n <= 0 {
		return 0
	}
	destroyed := p.retryDraining(ctx, key)

	p.mu.Lock()
	var victims []*Sandbox
	ids := p.ready[key]
	for len(victims) < n && len(ids) > 0 {
		if sb, ok := p.entries[ids[0]]; ok && sb.State == StateReady {
			victims = append(victims, sb)
		}
		ids = ids[1:]
	}
	p.ready[key] = ids
	if p.provisioner == nil {
		// Ledger-only: nothing physical to confirm — untrack now.
		for _, sb := range victims {
			delete(p.entries, sb.ID)
		}
		p.mu.Unlock()
		return destroyed + len(victims)
	}
	for _, sb := range victims {
		// Park as Draining BEFORE the physical call: whatever the
		// teardown does, this sandbox is never claimable warmth again.
		sb.State = StateDraining
		p.draining[key] = append(p.draining[key], sb)
	}
	p.mu.Unlock()

	for _, sb := range victims {
		if err := p.provisioner.TearDown(ctx, sb.ID); err != nil {
			continue // stays Draining; the next pass retries it
		}
		p.mu.Lock()
		delete(p.entries, sb.ID)
		p.mu.Unlock()
		destroyed++
	}
	return destroyed
}

// Boot starts one fresh Warming sandbox for key WITHOUT binding it — the
// controller's replenish primitive (scale-up toward target). Returns the
// new sandbox id. Readiness arrives later via NotifyReady.
func (p *Pool) Boot(ctx context.Context, key PoolKey) (string, error) {
	p.mu.Lock()
	id := p.newID()
	p.entries[id] = &Sandbox{ID: id, Key: key, State: StateWarming, CreatedAt: p.now()}
	p.mu.Unlock()

	if p.provisioner != nil {
		if err := p.provisioner.Boot(ctx, key, id); err != nil {
			p.mu.Lock()
			delete(p.entries, id)
			p.mu.Unlock()
			return "", fmt.Errorf("warmpool.Pool.Boot: %w", err)
		}
	}
	return id, nil
}

// RunClassifier resolves the pool key and run class for a Run at bind time.
// The coord.SandboxBinder seam carries ONLY the runID (pkg/coord is a
// custody port, not a config channel — see its doc comment), so the adapter
// closes over how to derive (key, class) from that id: production resolves
// them from the Run CRD's spec.sandboxPolicy + the AgentRuntime image
// (apiserver read); single-key deployments use DefaultClassifier.
type RunClassifier func(ctx context.Context, runID string) (PoolKey, RunClass, error)

// DefaultClassifier pins one pool key/class for every Run — the single-pool
// deployment shape (and the honest default until the Run-CRD resolver lands
// with the execution-layer adapters, ISI-2883/ISI-2889).
func DefaultClassifier(key PoolKey, class RunClass) RunClassifier {
	return func(context.Context, string) (PoolKey, RunClass, error) { return key, class, nil }
}

// Binder adapts a Pool to the coord.SandboxBinder port (pkg/coord/
// prodeffects.go): ProdEffects invokes it at most once per Run — gated by
// the coord.sandbox_bind run_id-PK marker — and this adapter resolves the
// key/class then delegates to Pool.Bind (itself runID-keyed, so the two
// guards compose into at-most-once even across a crash window).
type Binder struct {
	pool     *Pool
	classify RunClassifier
}

// NewBinder returns the coord.SandboxBinder over pool, resolving each run's
// pool key and class through classify.
func NewBinder(pool *Pool, classify RunClassifier) *Binder {
	return &Binder{pool: pool, classify: classify}
}

// Bind implements coord.SandboxBinder.
func (b *Binder) Bind(ctx context.Context, runID string) (string, error) {
	key, class, err := b.classify(ctx, runID)
	if err != nil {
		return "", fmt.Errorf("warmpool.Binder.Bind: classify run %s: %w", runID, err)
	}
	return b.pool.Bind(ctx, runID, key, class)
}

// Compile-time proof the adapter satisfies the coord seam ProdEffects
// drives — the drop-in replacement for its nil-binder ledger-only mode.
var _ interface {
	Bind(ctx context.Context, runID string) (sandboxRef string, err error)
} = (*Binder)(nil)
