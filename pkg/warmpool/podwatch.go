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

// podwatch.go — the production NotifyReady caller (M1.2): the Provisioner
// contract (pool.go) says "readiness is reported to the pool via NotifyReady
// (the kube adapter calls it from the pod watch)" — this controller IS that
// pod watch. Without it, warm boots never left StateWarming outside tests
// and the pool's Ready FIFO stayed permanently empty.
//
// It watches Pods carrying the sandbox label warmpool.Boot stamps
// (app=k8squad-sandbox) and, whenever a sandbox pod's Ready condition flips
// true — the in-pod supervisor (ADR-0007 D1) serving /health+/ready and the
// kubelet accepting it — reports it to the pool. Pool.NotifyReady's own
// state machine does the rest: pool-replenish entries join the Ready FIFO,
// run-reserved (cold-path) entries only record their readiness instant.
package warmpool

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// SandboxAppLabel is the label Boot stamps on every sandbox pod; the watcher
// filters on it so only pool-owned pods reconcile here.
const SandboxAppLabel = "app"

// SandboxAppValue is SandboxAppLabel's sandbox value.
const SandboxAppValue = "k8squad-sandbox"

// PodWatcher feeds kube pod readiness into the pool.
type PodWatcher struct {
	client client.Client
	pool   *Pool
}

// NewPodWatcher builds the watcher over pool.
func NewPodWatcher(c client.Client, pool *Pool) *PodWatcher {
	return &PodWatcher{client: c, pool: pool}
}

// SetupWithManager registers the pod watch with the manager.
func (w *PodWatcher) SetupWithManager(mgr ctrl.Manager) error {
	return builder.ControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(o client.Object) bool {
			return o.GetLabels()[SandboxAppLabel] == SandboxAppValue
		})).
		Complete(w)
}

// Reconcile reports Ready sandbox pods to the pool. Idempotent: NotifyReady
// ignores unknown/non-Warming ids, so re-reconciles of long-bound pods are
// no-ops.
func (w *PodWatcher) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pod corev1.Pod
	if err := w.client.Get(ctx, req.NamespacedName, &pod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if podReady(&pod) {
		w.pool.NotifyReady(pod.Name)
	}
	return ctrl.Result{}, nil
}

// podReady reports the pod's Ready condition.
func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
