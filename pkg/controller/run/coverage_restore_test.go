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

// ISI-6540: restore the pkg/controller/run coverage floor (82.9 → 93.3) that
// the ISI-6539 baseline refresh re-pinned downward. These tests exercise the
// error/branch paths the existing suite left uncovered — Role resolution, the
// S5 go-live gate pause message, and the authoring-token side-channel's
// fail-closed error returns — without touching production code.

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/reconcile"
	"github.com/K8squad/K8squad/pkg/toolchain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- resolveRole: every branch (context.go) ---

func TestResolveRole(t *testing.T) {
	t.Run("empty RoleRef resolves to nil Role", func(t *testing.T) {
		r := &Reconciler{Client: newAuthoringClient(t)}
		agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: asmRunNS}}
		role, err := r.resolveRole(context.Background(), agent)
		require.NoError(t, err)
		assert.Nil(t, role)
	})

	t.Run("resolves a Role in the Agent's namespace when RoleRef omits one", func(t *testing.T) {
		role := &api.Role{ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: asmRunNS}}
		agent := &api.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: asmRunNS},
			Spec:       api.AgentSpec{RoleRef: api.ObjectRef{Name: "dev"}},
		}
		r := &Reconciler{Client: newAuthoringClient(t, role)}
		got, err := r.resolveRole(context.Background(), agent)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "dev", got.Name)
	})

	t.Run("honors an explicit RoleRef.Namespace", func(t *testing.T) {
		role := &api.Role{ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "other-ns"}}
		agent := &api.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: asmRunNS},
			Spec:       api.AgentSpec{RoleRef: api.ObjectRef{Name: "dev", Namespace: "other-ns"}},
		}
		r := &Reconciler{Client: newAuthoringClient(t, role)}
		got, err := r.resolveRole(context.Background(), agent)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "other-ns", got.Namespace)
	})

	t.Run("a missing Role is not an error (returns nil)", func(t *testing.T) {
		agent := &api.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: asmRunNS},
			Spec:       api.AgentSpec{RoleRef: api.ObjectRef{Name: "ghost"}},
		}
		r := &Reconciler{Client: newAuthoringClient(t)} // Role absent
		got, err := r.resolveRole(context.Background(), agent)
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("a non-NotFound Get error fails closed with a wrapped error", func(t *testing.T) {
		agent := &api.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: asmRunNS},
			Spec:       api.AgentSpec{RoleRef: api.ObjectRef{Name: "dev"}},
		}
		c := erroringGetClient(t, apierrors.NewInternalError(assertErr("etcd down")))
		r := &Reconciler{Client: c}
		got, err := r.resolveRole(context.Background(), agent)
		require.Error(t, err)
		assert.Nil(t, got)
		assert.Contains(t, err.Error(), "resolve Role")
	})
}

// --- S5 go-live gate pause message (status.go: GateMessage / gatePauseMessage) ---

func TestGateMessage(t *testing.T) {
	t.Run("empty MissingTypes yields the empty string (caller falls back)", func(t *testing.T) {
		assert.Equal(t, "", PauseDetail{}.GateMessage())
	})

	t.Run("names the ANY-OF missing credential types", func(t *testing.T) {
		msg := PauseDetail{MissingTypes: []string{"oauth", "api_key"}}.GateMessage()
		assert.Contains(t, msg, "oauth, api_key")
		assert.Contains(t, msg, "project settings")
	})
}

func TestGatePauseMessageThroughReadyCondition(t *testing.T) {
	// nil detail → generic fallback hold string.
	t.Run("nil detail falls back to the generic hold", func(t *testing.T) {
		out := ProjectStatusWithPause(api.RunStatus{}, reconcile.StepPausedCredentialRequired, 1, fixedNow, nil)
		cond := out.Conditions[0]
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Contains(t, cond.Message, "required credential type is not configured")
	})

	// detail with missing types → the S5 gate string surfaces.
	t.Run("detail with missing types surfaces the gate string", func(t *testing.T) {
		detail := &PauseDetail{MissingTypes: []string{"oauth"}}
		out := ProjectStatusWithPause(api.RunStatus{}, reconcile.StepPausedCredentialRequired, 1, fixedNow, detail)
		assert.Contains(t, out.Conditions[0].Message, "add any of credential type(s) oauth")
	})

	// detail present but no missing types → GateMessage is "" so we fall back.
	t.Run("detail without missing types falls back to the generic hold", func(t *testing.T) {
		detail := &PauseDetail{Reason: "credential_required"}
		out := ProjectStatusWithPause(api.RunStatus{}, reconcile.StepPausedCredentialRequired, 1, fixedNow, detail)
		assert.Contains(t, out.Conditions[0].Message, "required credential type is not configured")
	})
}

// --- authoring-token fail-closed error paths (assembly.go: ensureAuthoringToken) ---

func TestEnsureAuthoringTokenErrorPaths(t *testing.T) {
	minter := testMinter(t)

	t.Run("a non-NotFound Get on the existing Secret fails closed", func(t *testing.T) {
		run := authoringRun()
		c := erroringGetClient(t, apierrors.NewInternalError(assertErr("apiserver unreachable")), run, teamCR("squad-a", "team-uid-squad-a"))
		asm := &Assembler{Client: c, Minter: minter}
		eps := authoringEps()
		asm.bindAuthoringCredential(run, eps)
		err := asm.ensureAuthoringToken(context.Background(), run, eps)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "get authoring-token secret")
	})

	t.Run("a Create error (non-AlreadyExists) fails closed", func(t *testing.T) {
		run := authoringRun()
		base := newAuthoringClient(t, run, teamCR("squad-a", "team-uid-squad-a"))
		c := interceptorOver(base, interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
				return apierrors.NewInternalError(assertErr("write rejected"))
			},
		})
		asm := &Assembler{Client: c, Minter: minter}
		eps := authoringEps()
		asm.bindAuthoringCredential(run, eps)
		err := asm.ensureAuthoringToken(context.Background(), run, eps)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "create authoring-token secret")
	})

	t.Run("a raced Create (AlreadyExists) is swallowed as success", func(t *testing.T) {
		run := authoringRun()
		base := newAuthoringClient(t, run, teamCR("squad-a", "team-uid-squad-a"))
		c := interceptorOver(base, interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, obj.GetName())
			},
		})
		asm := &Assembler{Client: c, Minter: minter}
		eps := authoringEps()
		asm.bindAuthoringCredential(run, eps)
		require.NoError(t, asm.ensureAuthoringToken(context.Background(), run, eps))
	})
}

// --- discussion/search token fail-closed error paths (mirror authoring) ---

func TestEnsureDiscussionTokenErrorPaths(t *testing.T) {
	minter := testMinter(t)

	t.Run("a non-NotFound Get on the existing Secret fails closed", func(t *testing.T) {
		run := discussionRun()
		c := erroringGetClient(t, apierrors.NewInternalError(assertErr("apiserver unreachable")), run, teamCR("squad-a", "team-uid-squad-a"))
		asm := &Assembler{Client: c, Minter: minter}
		eps := discussionEps()
		asm.bindDiscussionCredential(run, eps)
		err := asm.ensureDiscussionToken(context.Background(), run, eps)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "get discussion-token secret")
	})

	t.Run("a Create error (non-AlreadyExists) fails closed", func(t *testing.T) {
		run := discussionRun()
		base := newAuthoringClient(t, run, teamCR("squad-a", "team-uid-squad-a"))
		c := interceptorOver(base, interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
				return apierrors.NewInternalError(assertErr("write rejected"))
			},
		})
		asm := &Assembler{Client: c, Minter: minter}
		eps := discussionEps()
		asm.bindDiscussionCredential(run, eps)
		err := asm.ensureDiscussionToken(context.Background(), run, eps)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "create discussion-token secret")
	})

	t.Run("a raced Create (AlreadyExists) is swallowed as success", func(t *testing.T) {
		run := discussionRun()
		base := newAuthoringClient(t, run, teamCR("squad-a", "team-uid-squad-a"))
		c := interceptorOver(base, interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, obj.GetName())
			},
		})
		asm := &Assembler{Client: c, Minter: minter}
		eps := discussionEps()
		asm.bindDiscussionCredential(run, eps)
		require.NoError(t, asm.ensureDiscussionToken(context.Background(), run, eps))
	})
}

func TestEnsureSearchTokenErrorPaths(t *testing.T) {
	minter := testMinter(t)

	t.Run("a non-NotFound Get on the existing Secret fails closed", func(t *testing.T) {
		run := authoringRun()
		c := erroringGetClient(t, apierrors.NewInternalError(assertErr("apiserver unreachable")), run, teamCR("squad-a", "team-uid-squad-a"))
		asm := &Assembler{Client: c, Minter: minter}
		eps := searchEps()
		asm.bindSearchCredential(run, eps)
		err := asm.ensureSearchToken(context.Background(), run, eps)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "get search-token secret")
	})

	t.Run("a Create error (non-AlreadyExists) fails closed", func(t *testing.T) {
		run := authoringRun()
		base := newAuthoringClient(t, run, teamCR("squad-a", "team-uid-squad-a"))
		c := interceptorOver(base, interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
				return apierrors.NewInternalError(assertErr("write rejected"))
			},
		})
		asm := &Assembler{Client: c, Minter: minter}
		eps := searchEps()
		asm.bindSearchCredential(run, eps)
		err := asm.ensureSearchToken(context.Background(), run, eps)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "create search-token secret")
	})

	t.Run("a raced Create (AlreadyExists) is swallowed as success", func(t *testing.T) {
		run := authoringRun()
		base := newAuthoringClient(t, run, teamCR("squad-a", "team-uid-squad-a"))
		c := interceptorOver(base, interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, obj.GetName())
			},
		})
		asm := &Assembler{Client: c, Minter: minter}
		eps := searchEps()
		asm.bindSearchCredential(run, eps)
		require.NoError(t, asm.ensureSearchToken(context.Background(), run, eps))
	})
}

// --- ensureContextSnapshot guard + fail-closed branches (context.go) ---

func TestEnsureContextSnapshotBranches(t *testing.T) {
	asm := stubAssemblers{src: stubSources{wiRev: "rev-1", goalRev: "1"}}

	t.Run("non-claiming/running phase is a no-op", func(t *testing.T) {
		run, agent, project := runWithAgent()
		c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run, agent, project, teamFor()).Build()
		r := &Reconciler{Client: c, ContextAssemblers: asm, Now: func() metav1.Time { return fixedNow }}
		desired := &api.RunStatus{Phase: api.RunPhaseSucceeded}
		require.NoError(t, r.ensureContextSnapshot(context.Background(), run, desired))
		assert.Nil(t, desired.ContextSnapshot)
	})

	t.Run("agent-less run is a no-op (no model to key on)", func(t *testing.T) {
		run, _, project := runWithAgent()
		run.Spec.Agents = nil
		c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run, project, teamFor()).Build()
		r := &Reconciler{Client: c, ContextAssemblers: asm, Now: func() metav1.Time { return fixedNow }}
		desired := &api.RunStatus{Phase: api.RunPhaseClaiming}
		require.NoError(t, r.ensureContextSnapshot(context.Background(), run, desired))
		assert.Nil(t, desired.ContextSnapshot)
	})

	t.Run("a missing Agent CR fails closed", func(t *testing.T) {
		run, _, project := runWithAgent()
		c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run, project, teamFor()).Build()
		r := &Reconciler{Client: c, ContextAssemblers: asm, Now: func() metav1.Time { return fixedNow }}
		err := r.ensureContextSnapshot(context.Background(), run, &api.RunStatus{Phase: api.RunPhaseClaiming})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read Agent")
	})

	t.Run("a missing Project CR fails closed", func(t *testing.T) {
		run, agent, _ := runWithAgent()
		c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run, agent, teamFor()).Build()
		r := &Reconciler{Client: c, ContextAssemblers: asm, Now: func() metav1.Time { return fixedNow }}
		err := r.ensureContextSnapshot(context.Background(), run, &api.RunStatus{Phase: api.RunPhaseClaiming})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read Project")
	})

	t.Run("a missing Team CR fails closed", func(t *testing.T) {
		run, agent, project := runWithAgent()
		c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run, agent, project).Build()
		r := &Reconciler{Client: c, ContextAssemblers: asm, Now: func() metav1.Time { return fixedNow }}
		err := r.ensureContextSnapshot(context.Background(), run, &api.RunStatus{Phase: api.RunPhaseRunning})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read Team")
	})
}

// --- RBACRenderer.Release fail-closed delete branches (rbac.go) ---

func TestRBACReleaseDeleteErrors(t *testing.T) {
	run := &api.Run{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: asmRunNS, UID: "uid-r1"}}

	// Error on the namespaced Role delete → covers the first fail-closed branch.
	t.Run("a non-NotFound Role delete error fails closed", func(t *testing.T) {
		base := newAuthoringClient(t)
		c := interceptorOver(base, interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
				return apierrors.NewInternalError(assertErr("delete rejected"))
			},
		})
		rr := &RBACRenderer{Client: c}
		err := rr.Release(context.Background(), run)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "delete run role")
	})

	// Error only on the ClusterRole delete → the namespaced Role/RoleBinding
	// deletes succeed (NotFound, swallowed), driving the cluster-scoped branch.
	t.Run("a non-NotFound ClusterRole delete error fails closed", func(t *testing.T) {
		base := newAuthoringClient(t)
		c := interceptorOver(base, interceptor.Funcs{
			Delete: func(_ context.Context, w client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*rbacv1.ClusterRole); ok {
					return apierrors.NewInternalError(assertErr("delete rejected"))
				}
				return w.Delete(context.Background(), obj, opts...)
			},
		})
		rr := &RBACRenderer{Client: c}
		err := rr.Release(context.Background(), run)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "delete run clusterrole")
	})

	// All deletes succeed as NotFound (nothing rendered) → the happy return.
	t.Run("absent objects release cleanly", func(t *testing.T) {
		rr := &RBACRenderer{Client: newAuthoringClient(t)}
		require.NoError(t, rr.Release(context.Background(), run))
	})
}

// --- EnsureManifest fail-closed projection branches (assembly.go) ---

func TestEnsureManifestProjectionErrors(t *testing.T) {
	t.Run("fresh path: an MCP ConfigMap write error fails closed", func(t *testing.T) {
		base, run, _ := seedAssemblyWorld(t)
		c := interceptorOver(base, interceptor.Funcs{
			Create: func(_ context.Context, w client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok {
					return apierrors.NewInternalError(assertErr("configmap write rejected"))
				}
				return w.Create(context.Background(), obj, opts...)
			},
		})
		_, err := NewAssembler(c, toolchain.PlatformConfig{}).EnsureManifest(context.Background(), run)
		require.Error(t, err)
	})

	t.Run("recorded path: the manifest is re-projected (drift repair)", func(t *testing.T) {
		c, run, _ := seedAssemblyWorld(t)
		asm := NewAssembler(c, toolchain.PlatformConfig{})

		// Stamp the manifest, then drop the IR ConfigMap to force a re-projection
		// down the recorded (Status.CapabilityManifest != nil) branch.
		manifest, err := asm.EnsureManifest(context.Background(), run)
		require.NoError(t, err)
		run.Status.CapabilityManifest = manifest
		require.NoError(t, c.Delete(context.Background(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: asmRunNS, Name: "ksquad-run-asm1-mcp"}}))

		again, err := asm.EnsureManifest(context.Background(), run)
		require.NoError(t, err)
		assert.Equal(t, manifest.CapabilityHash, again.CapabilityHash)
		// The IR ConfigMap is re-created by the recorded-path convergence.
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: asmRunNS, Name: "ksquad-run-asm1-mcp"}, &corev1.ConfigMap{}))
	})

	t.Run("recorded path: a re-projection write error fails closed", func(t *testing.T) {
		base, run, _ := seedAssemblyWorld(t)
		asm := NewAssembler(base, toolchain.PlatformConfig{})
		manifest, err := asm.EnsureManifest(context.Background(), run)
		require.NoError(t, err)
		run.Status.CapabilityManifest = manifest

		// Now wrap with a client that rejects the drift-repair ConfigMap write.
		c := interceptorOver(base, interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return apierrors.NewInternalError(assertErr("rejected"))
			},
			Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok {
					return apierrors.NewInternalError(assertErr("rejected"))
				}
				return nil
			},
		})
		_, err = NewAssembler(c, toolchain.PlatformConfig{}).EnsureManifest(context.Background(), run)
		require.Error(t, err)
	})
}

// --- helpers ---

// assertErr is a tiny error constructor (keeps error literals out of the body).
type assertErr string

func (e assertErr) Error() string { return string(e) }

// interceptorOver wraps a built fake client with interceptor funcs. The fake
// client is a client.WithWatch, which interceptor.NewClient requires.
func interceptorOver(base client.Client, funcs interceptor.Funcs) client.WithWatch {
	return interceptor.NewClient(base.(client.WithWatch), funcs)
}

// erroringGetClient returns a client whose Get always fails with the given
// error — used to drive the fail-closed, non-NotFound Get branches.
func erroringGetClient(t *testing.T, getErr error, objs ...client.Object) client.Client {
	t.Helper()
	base := newAuthoringClient(t, objs...)
	return interceptorOver(base, interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return getErr
		},
	})
}
