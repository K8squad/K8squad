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

// adopt.go — ISI-4291 restart reconciliation: the pool's inventory is
// in-memory only, so an operator restart forgets every live warm pod;
// NotifyReady ignores unknown ids (pool.go) and the controller, seeing
// live=0, boots a full replacement set — the previous generation's pods
// become CPU-consuming orphans (observed 3× in one day on k8squad-test).
//
// AdoptOrReap is the leader-elect-start fix. It lists the sandbox pods
// Boot stamped (app=k8squad-sandbox, cluster-wide — the same scope the
// PodWatcher watches) and sorts them into exactly three dispositions:
//
//   - ADOPT: pod Ready, its key PROVABLY a managed key, and no task-io
//     Secret named after it → insert into the pool as a Ready entry. The
//     proof is the full-key annotation set Boot stamps (below): a pod
//     whose annotations decode to a key structurally equal to a managed
//     key belongs to a pool this operator reconciles. The Secret check is
//     the run-ownership discriminator: SecretCredentialWriter mints the
//     per-sandbox Secret at Bind — BEFORE the durable coord.sandbox_bind
//     marker (pkg/coord/prodeffects.go, "a committed marker implies both")
//     — so a pod with a Secret is run-owned and a pod without one was
//     never bound (§9.3 reuse-contamination cannot be reintroduced
//     through the restart door).
//   - LEAVE: a pod with a task-io Secret is a live Run's sandbox — the
//     run-drive lifecycle (ledger reattach via sandbox_ref) owns it.
//     Adopting it would be cross-run contamination; reaping it would kill
//     every in-flight Run on every deploy restart.
//   - REAP: everything else is deleted — pre-change orphans lacking the
//     full-key annotations (they can never be proven safe), not-yet-Ready
//     pods (the ticket's contract adopts READY warmth only; a crashed
//     generation's in-flight replenish boots are cheaper to replace than
//     to babysit), and pods whose decoded key matches no managed key
//     (config drift — no controller will ever claim them).
//
// The one boundary this deliberately does NOT close: a Run whose bind
// crashed between the Secret write and the marker insert leaves a
// Secret-carrying pod that a later Release cannot tear down (byRun was
// in-memory). It is left alive here — killing a possibly-live Run's
// sandbox is strictly worse than one leaked pod per such crash window.

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
package warmpool

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The full-PoolKey annotation set (ISI-4291). Boot stamps every dimension
// individually — including EMPTY values, because presence-with-empty-value
// is exactly what distinguishes "provably the bare posture" from "a
// pre-change pod whose key cannot be proven". poolKeyFromAnnotations
// requires all five keys to be present before it will decode.
const (
	AnnPoolRuntimeClass   = "k8squad.io/pool-runtime-class"
	AnnPoolImage          = "k8squad.io/pool-image"
	AnnPoolNamespace      = "k8squad.io/pool-namespace"
	AnnPoolCapabilityHash = "k8squad.io/pool-capability-hash"
	AnnPoolProjectPVC     = "k8squad.io/pool-project-pvc"

	// AnnRunID carries the Run's id on a run-owned sandbox pod (ADR-0020 §2.3,
	// ISI-4348-S2). Boot stamps only the pool-key dimensions above — a warm pod
	// has no Run — so the Bind-path credential writer stamps this at Bind, the
	// same instant it writes the task-io Secret that marks the pod run-owned.
	// The restart-safe reaper reads it back (runIDFromPod) to ask the settlement
	// reader "did this run's follow settle?".
	AnnRunID = "k8squad.io/run-id"
)

// SettlementReader answers whether a run's a2a follow has durably settled
// (ADR-0020 §2.1 marker on coord.a2a_dispatch). The restart-safe reaper needs
// exactly this one bit; pkg/coord's ProdSettleReader satisfies it. Kept as a
// local interface so warmpool stays free of a coord/DB import.
type SettlementReader interface {
	Settled(ctx context.Context, runID string) (bool, error)
}

// FollowOracle reports whether a live follow goroutine exists for runID in THIS
// operator process — the ADR-0020 §2.3 liveness half of the reap proof.
// internal/a2a.Dispatcher.IsFollowing satisfies it. Local interface for the same
// decoupling reason as SettlementReader.
type FollowOracle interface {
	IsFollowing(runID string) bool
}

// runIDFromPod recovers the Run id AnnRunID stamped on a run-owned pod at Bind.
// Empty when absent — a pre-ADR-0020 pod, or a bound pod whose stamp write
// raced/failed. An empty run id fails the reap decision CLOSED (kept, never
// reaped): without a provable run id the reaper cannot query settlement, and a
// pod that MIGHT belong to a live Run is never killed on a maybe.
func runIDFromPod(pod *corev1.Pod) string {
	return pod.Annotations[AnnRunID]
}

// AdoptReport is the one-pass outcome AdoptOrReap returns (logged by the
// operator at startup — the restart-churn observable).
type AdoptReport struct {
	// Adopted: pre-existing Ready pods inserted as pool warmth.
	Adopted int
	// Reaped: pods deleted (pre-change orphans, unproven keys, non-Ready).
	Reaped int
	// LeftRunOwned: pods skipped because a task-io Secret marks them as a
	// live Run's sandbox (Bind-path ownership — see file header).
	LeftRunOwned int
}

// poolKeyFromAnnotations decodes the full PoolKey Boot stamped. It fails
// (ok=false) unless EVERY dimension annotation is present — a pod without
// the richer annotation set is a pre-change orphan that can never be
// proven to belong to a managed key, and unprovability means reaped.
func poolKeyFromAnnotations(pod *corev1.Pod) (PoolKey, bool) {
	ann := pod.Annotations
	rc, okRC := ann[AnnPoolRuntimeClass]
	img, okImg := ann[AnnPoolImage]
	ns, okNS := ann[AnnPoolNamespace]
	ch, okCH := ann[AnnPoolCapabilityHash]
	pvc, okPVC := ann[AnnPoolProjectPVC]
	if !okRC || !okImg || !okNS || !okCH || !okPVC {
		return PoolKey{}, false
	}
	return PoolKey{
		RuntimeClass:   rc,
		Image:          img,
		Namespace:      ns,
		CapabilityHash: ch,
		ProjectPVC:     pvc,
	}, true
}

// AdoptOrReap reconciles pre-existing sandbox pods into a freshly started
// pool (ISI-4291). Call it ONCE on leader-elect start, BEFORE the warm
// controller's first Tick, so the adopted pods already count toward live
// inventory and no replacement set is booted. Per-pod failures are
// collected into the returned error and do not abort the pass (a wedged
// pod must not shield the others from their disposition).
func AdoptOrReap(ctx context.Context, c client.Client, pool *Pool, settled SettlementReader, following FollowOracle, managed ...PoolKey) (AdoptReport, error) {
	managedSet := make(map[PoolKey]struct{}, len(managed))
	for _, k := range managed {
		managedSet[k] = struct{}{}
	}

	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.MatchingLabels{SandboxAppLabel: SandboxAppValue}); err != nil {
		return AdoptReport{}, fmt.Errorf("warmpool.AdoptOrReap: list sandbox pods: %w", err)
	}

	// Oldest first: adoption order preserves the FIFO the Ready queue
	// would have had (CreationTimestamp is the boot instant).
	items := make([]corev1.Pod, 0, len(pods.Items))
	items = append(items, pods.Items...)
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].CreationTimestamp.Before(&items[j].CreationTimestamp)
	})

	var report AdoptReport
	var errs []error
	for i := range items {
		pod := &items[i]
		if pod.DeletionTimestamp != nil {
			continue // already terminating — a delete is already in flight
		}

		key, proven := poolKeyFromAnnotations(pod)
		if proven {
			if _, managedKey := managedSet[key]; managedKey {
				// Run-ownership discriminator: the Bind-path Secret is
				// written before the durable marker, so its presence
				// means a Run owns this pod — leave it to run-drive.
				var secret corev1.Secret
				err := c.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: pod.Name}, &secret)
				switch {
				case err == nil:
					// Run-owned pod. ADR-0020 §2.3: reap it iff its follow has
					// durably settled AND no live follow exists in this process
					// — a leaked-across-restart sandbox. Otherwise leave it: the
					// agent may still be working, or it settled but is still
					// following (steady state, OnDone will release it).
					reaped, derr := reapIfSettled(ctx, c, pod, settled, following)
					if derr != nil {
						errs = append(errs, derr)
						continue // reader/delete error: keep — never reap on a maybe
					}
					if reaped {
						report.Reaped++
					} else {
						report.LeftRunOwned++
					}
					continue
				case !apierrors.IsNotFound(err):
					errs = append(errs, fmt.Errorf("warmpool.AdoptOrReap: check task-io secret for %s/%s: %w", pod.Namespace, pod.Name, err))
					continue // transient error: neither adopt nor reap on a maybe — leave it for the next restart
				}
				if podReady(pod) {
					if pool.Adopt(Sandbox{
						ID:        pod.Name,
						Key:       key,
						CreatedAt: pod.CreationTimestamp.Time,
						ReadyAt:   podReadySince(pod),
					}) {
						report.Adopted++
						continue
					}
					// Adopt returned false: the id is already tracked —
					// impossible on a fresh start, but treat as done.
					continue
				}
				// Not Ready: a crashed generation's in-flight replenish.
				// Fall through to reap.
			}
		}
		if err := reapPod(ctx, c, pod); err != nil {
			errs = append(errs, err)
			continue
		}
		report.Reaped++
	}
	return report, errors.Join(errs...)
}

// reapIfSettled applies the ADR-0020 §2.3 restart-safe reap decision to ONE
// run-owned pod (task-io Secret already confirmed present by the caller). It
// reaps — foreground delete, which also drops the Secret — iff the pod's run is
// durably settled AND has no live follow in this process, and reports whether it
// did. Every uncertain case fails CLOSED to "keep" (reaped=false, err=nil): no
// stamped run id, a nil reader/oracle, a run not yet settled, or a still-live
// follow all leave the pod for the run-drive lifecycle. Only a settlement-reader
// error or a delete failure returns err (the caller keeps the pod and records
// the error) — never reap on a maybe (F3: protect an agent still working).
func reapIfSettled(ctx context.Context, c client.Client, pod *corev1.Pod, settled SettlementReader, following FollowOracle) (bool, error) {
	if settled == nil || following == nil {
		return false, nil // reaper not wired: leave run-owned pods to run-drive
	}
	runID := runIDFromPod(pod)
	if runID == "" {
		return false, nil // no provable run id ⇒ cannot check settlement ⇒ keep
	}
	isSettled, err := settled.Settled(ctx, runID)
	if err != nil {
		return false, fmt.Errorf("warmpool: settlement check for run %s (pod %s/%s): %w", runID, pod.Namespace, pod.Name, err)
	}
	if !isSettled {
		return false, nil // agent may still be working across the restart (F3)
	}
	if following.IsFollowing(runID) {
		return false, nil // a live follow owns it here — steady state, not leaked
	}
	// Settled ⇒ the follow reached completion; not-following ⇒ none is in flight
	// in this process ⇒ the agent finished and the pod leaked. Safe to reap.
	if err := reapPod(ctx, c, pod); err != nil {
		return false, err
	}
	return true, nil
}

// SweepSettledRunOwned is the ADR-0020 §2.3 steady-state backstop for the
// warm-pool Tick. AdoptOrReap catches the common crash-recovery case on the new
// leader's first pass; this sweep re-checks run-owned sandbox pods on the tick
// cadence so a pod that settles AFTER that pass — or one whose OnDone in-memory
// Release failed and left the Secret — is still reclaimed. It is deliberately
// narrower than AdoptOrReap: it ONLY reaps settled-and-not-following run-owned
// pods and NEVER adopts, scales, or reaps anything else (orphan/unproven-key
// disposition stays AdoptOrReap's job, run once at start). One indexed
// settlement query per run-owned pod; per-pod failures are collected, not fatal.
func SweepSettledRunOwned(ctx context.Context, c client.Client, settled SettlementReader, following FollowOracle, managed ...PoolKey) (AdoptReport, error) {
	if settled == nil || following == nil {
		return AdoptReport{}, nil // reaper not wired
	}
	managedSet := make(map[PoolKey]struct{}, len(managed))
	for _, k := range managed {
		managedSet[k] = struct{}{}
	}

	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.MatchingLabels{SandboxAppLabel: SandboxAppValue}); err != nil {
		return AdoptReport{}, fmt.Errorf("warmpool.SweepSettledRunOwned: list sandbox pods: %w", err)
	}

	var report AdoptReport
	var errs []error
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil {
			continue // already terminating
		}
		key, proven := poolKeyFromAnnotations(pod)
		if !proven {
			continue // orphan/unproven key — AdoptOrReap's job, not the backstop's
		}
		if _, managedKey := managedSet[key]; !managedKey {
			continue // config drift — left to AdoptOrReap
		}
		// Run-ownership discriminator, same as AdoptOrReap: only a pod carrying
		// its task-io Secret is a Run's sandbox worth the settlement check.
		var secret corev1.Secret
		switch err := c.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: pod.Name}, &secret); {
		case apierrors.IsNotFound(err):
			continue // not run-owned (warm/adoptable) — not this sweep's concern
		case err != nil:
			errs = append(errs, fmt.Errorf("warmpool.SweepSettledRunOwned: check task-io secret for %s/%s: %w", pod.Namespace, pod.Name, err))
			continue
		}
		reaped, derr := reapIfSettled(ctx, c, pod, settled, following)
		if derr != nil {
			errs = append(errs, derr)
			continue
		}
		if reaped {
			report.Reaped++
		} else {
			report.LeftRunOwned++
		}
	}
	return report, errors.Join(errs...)
}

// podReadySince returns the Ready condition's transition instant (best
// effort — zero when the condition carries no timestamp, in which case
// Pool.Adopt stamps now).
func podReadySince(pod *corev1.Pod) time.Time {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.LastTransitionTime.Time
		}
	}
	return time.Time{}
}

// reapPod deletes a sandbox pod with foreground propagation (the same
// semantics as KubeProvisioner.TearDown — §9.3 teardown-and-replace).
func reapPod(ctx context.Context, c client.Client, pod *corev1.Pod) error {
	victim := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace}}
	if err := c.Delete(ctx, victim, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("warmpool.AdoptOrReap: reap %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return nil
}
