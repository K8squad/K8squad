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

package run

// ISI-6750: the final climb on the pkg/controller/run coverage floor (90.4 →
// 93.3) left by the ISI-6540 first slice. These tests reach the branches the
// first slice did not: SetupWithManager (0% — needs a real manager), the
// recordPhaseTransition terminal/unknown-token edges, and the RBAC renderer's
// per-object write-error fail-closed returns. No production code is touched.

import (
	"context"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	crconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/event"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/toolchain"
)

// --- resolution-error fail-closed returns (assembly.Resolve / RBACRenderer.Ensure) ---

func TestResolutionErrorsFailClosed(t *testing.T) {
	run := authoringRun()
	getErr := apierrors.NewInternalError(assertErr("apiserver unreachable"))

	t.Run("Assembler.Resolve fails closed on a capability-collect read error", func(t *testing.T) {
		c := erroringGetClient(t, getErr, run)
		_, err := NewAssembler(c, toolchain.PlatformConfig{}).Resolve(context.Background(), run)
		if err == nil {
			t.Fatal("a read error during Resolve must fail closed, got nil")
		}
	})

	t.Run("RBACRenderer.Ensure fails closed on a toolchain-ref read error", func(t *testing.T) {
		c := erroringGetClient(t, getErr, run)
		rr := &RBACRenderer{Client: c}
		if _, err := rr.Ensure(context.Background(), run); err == nil {
			t.Fatal("a read error during Ensure must fail closed, got nil")
		}
	})
}

// --- SetupWithManager (controller.go: 0% → both the default and PhaseKicks legs) ---

func TestSetupWithManager(t *testing.T) {
	newMgr := func(t *testing.T) ctrl.Manager {
		t.Helper()
		skip := true
		mgr, err := ctrl.NewManager(&rest.Config{Host: "localhost"}, ctrl.Options{
			Scheme:     newScheme(t),
			Metrics:    metricsserver.Options{BindAddress: "0"},
			Controller: crconfig.Controller{SkipNameValidation: &skip},
		})
		if err != nil {
			t.Fatalf("new manager: %v", err)
		}
		return mgr
	}

	t.Run("defaults the embedded client and recorder from the manager", func(t *testing.T) {
		mgr := newMgr(t)
		r := &Reconciler{} // the un-wired shape: no Client, no Recorder
		if err := r.SetupWithManager(mgr); err != nil {
			t.Fatalf("SetupWithManager: %v", err)
		}
		if r.Client == nil {
			t.Fatal("SetupWithManager left the embedded client nil — every reconcile panics at the first r.Get")
		}
		if r.Recorder == nil {
			t.Fatal("SetupWithManager left the recorder nil — phase Events never emit")
		}
	})

	t.Run("wires the PhaseKicks channel source when present", func(t *testing.T) {
		mgr := newMgr(t)
		kicks := make(chan event.TypedGenericEvent[client.Object])
		r := &Reconciler{PhaseKicks: kicks}
		if err := r.SetupWithManager(mgr); err != nil {
			t.Fatalf("SetupWithManager with PhaseKicks: %v", err)
		}
	})
}

// --- recordPhaseTransition terminal + unknown-token edges (controller.go) ---

func TestRecordPhaseTransitionEdges(t *testing.T) {
	newReconciler := func(jr PhaseJournal) *Reconciler {
		c := fake.NewClientBuilder().WithScheme(newScheme(t)).
			WithObjects(projectCR("proj-a", "uid-proj-a"), teamCRns("team-a", "uid-team-a")).
			Build()
		return &Reconciler{Client: c, PhaseJournal: jr}
	}

	t.Run("an unrecognised target phase folds to no chart band and is skipped", func(t *testing.T) {
		jr := &fakeJournal{}
		r := newReconciler(jr)
		// to == a bogus phase ⇒ phaseToken !ok ⇒ early return before any tenancy read.
		r.recordPhaseTransition(context.Background(), newRunWithTenancy(), api.RunPhaseRunning, api.RunPhase("Bogus"), fixedNow)
		if len(jr.edges) != 0 {
			t.Fatalf("unknown token should journal nothing, got %d edges", len(jr.edges))
		}
	})

	t.Run("a terminal failed edge journals and increments the error outcome", func(t *testing.T) {
		jr := &fakeJournal{}
		r := newReconciler(jr) // Health nil — ObserveRunEnd is nil-safe
		r.recordPhaseTransition(context.Background(), newRunWithTenancy(), api.RunPhaseRunning, api.RunPhaseFailed, fixedNow)
		if len(jr.edges) != 1 {
			t.Fatalf("a running→failed edge should journal exactly one row, got %d", len(jr.edges))
		}
		if jr.edges[0].ToPhase != phaseTokenFailed {
			t.Fatalf("journaled token = %q, want %q", jr.edges[0].ToPhase, phaseTokenFailed)
		}
	})
}

// --- RBAC renderer per-object write-error fail-closed returns (rbac.go) ---

func TestRBACEnsureWriteErrors(t *testing.T) {
	run := &api.Run{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: asmRunNS, UID: "uid-r1"}}
	rules := []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}}}

	// Fail the write for the given concrete object type; everything else passes
	// through to the base fake client so the earlier converge steps succeed.
	failCreateUpdateFor := func(base client.Client, match func(client.Object) bool) client.WithWatch {
		return interceptorOver(base, interceptor.Funcs{
			Create: func(ctx context.Context, w client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if match(obj) {
					return apierrors.NewInternalError(assertErr("write rejected"))
				}
				return w.Create(ctx, obj, opts...)
			},
		})
	}

	t.Run("namespaced Role write error fails closed", func(t *testing.T) {
		c := failCreateUpdateFor(newAuthoringClient(t), func(o client.Object) bool { _, ok := o.(*rbacv1.Role); return ok })
		rr := &RBACRenderer{Client: c}
		_, err := rr.ensureNamespaced(context.Background(), run, rules)
		if err == nil || !strings.Contains(err.Error(), "ensure run role") {
			t.Fatalf("want ensure-run-role error, got %v", err)
		}
	})

	t.Run("namespaced RoleBinding write error fails closed", func(t *testing.T) {
		c := failCreateUpdateFor(newAuthoringClient(t), func(o client.Object) bool { _, ok := o.(*rbacv1.RoleBinding); return ok })
		rr := &RBACRenderer{Client: c}
		_, err := rr.ensureNamespaced(context.Background(), run, rules)
		if err == nil || !strings.Contains(err.Error(), "ensure run rolebinding") {
			t.Fatalf("want ensure-run-rolebinding error, got %v", err)
		}
	})

	t.Run("cluster-scoped ClusterRole write error fails closed", func(t *testing.T) {
		c := failCreateUpdateFor(newAuthoringClient(t), func(o client.Object) bool { _, ok := o.(*rbacv1.ClusterRole); return ok })
		rr := &RBACRenderer{Client: c}
		_, err := rr.ensureClusterScoped(context.Background(), run, rules)
		if err == nil || !strings.Contains(err.Error(), "ensure run clusterrole") {
			t.Fatalf("want ensure-run-clusterrole error, got %v", err)
		}
	})

	t.Run("cluster-scoped ClusterRoleBinding write error fails closed", func(t *testing.T) {
		c := failCreateUpdateFor(newAuthoringClient(t), func(o client.Object) bool { _, ok := o.(*rbacv1.ClusterRoleBinding); return ok })
		rr := &RBACRenderer{Client: c}
		_, err := rr.ensureClusterScoped(context.Background(), run, rules)
		if err == nil || !strings.Contains(err.Error(), "ensure run clusterrolebinding") {
			t.Fatalf("want ensure-run-clusterrolebinding error, got %v", err)
		}
	})
}

// --- runClusterObjectName DNS-subdomain truncation (rbac.go) ---

func TestRunClusterObjectNameTruncates(t *testing.T) {
	run := &api.Run{ObjectMeta: metav1.ObjectMeta{
		Namespace: strings.Repeat("n", 200),
		Name:      strings.Repeat("r", 200),
	}}
	got := runClusterObjectName(run)
	if len(got) != 253 {
		t.Fatalf("over-long name should truncate to the 253-char DNS limit, got %d", len(got))
	}
}
