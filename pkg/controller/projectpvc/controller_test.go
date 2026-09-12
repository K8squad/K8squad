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

package projectpvc

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/workspace"
)

const teamSandboxNS = "ksquad-team-squad-a-ab12cd34"

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := api.AddToScheme(s); err != nil {
		t.Fatalf("add api scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	return s
}

func newProject(name string, spec *api.PVCSpec) *api.Project {
	p := &api.Project{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "squad-a", UID: types.UID("uid-" + name)},
		Spec: api.ProjectSpec{
			Repo: api.RepoSpec{URL: "https://github.com/acme/widget"},
		},
	}
	p.Spec.WorkspacePVC = spec
	return p
}

// newTeam builds a Team in squad-a consuming the named projects; sandboxNS
// is the team reconciler's resolved status.namespace ("" = not yet
// resolved).
func newTeam(name string, sandboxNS string, projects ...string) *api.Team {
	team := &api.Team{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "squad-a", UID: types.UID("uid-" + name)},
	}
	for _, p := range projects {
		team.Spec.Projects = append(team.Spec.Projects, api.ObjectRef{Name: p})
	}
	team.Status.Namespace = sandboxNS
	return team
}

func newReconciler(t *testing.T, class string, objs ...client.Object) (*Reconciler, client.Client) {
	t.Helper()
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(objs...).WithStatusSubresource(&api.Project{}, &api.Team{}).Build()
	r := NewReconciler(cl)
	r.storageClass = class
	return r, cl
}

func reconcileProject(t *testing.T, r *Reconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "squad-a", Name: name},
	}); err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
}

func getPVC(t *testing.T, cl client.Client, ns, name string) *corev1.PersistentVolumeClaim {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, pvc); err != nil {
		t.Fatalf("get PVC %s/%s: %v", ns, name, err)
	}
	return pvc
}

func workspaceCondition(t *testing.T, cl client.Client, project string) *metav1.Condition {
	t.Helper()
	p := &api.Project{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "squad-a", Name: project}, p); err != nil {
		t.Fatalf("get project %s: %v", project, err)
	}
	return apimeta.FindStatusCondition(p.Status.Conditions, ConditionWorkspaceReady)
}

// TestProvisionCreatesClaimInTeamNamespace (ISI-4127 AC1 as fixed by
// ISI-4302): a Project with spec.workspacePVC consumed by a Team gets
// exactly one claim in the TEAM'S SANDBOX NAMESPACE — the only namespace
// a sandbox-pod mount can resolve in — sized/classed/moded from the spec,
// identified by labels + created-by annotation (no cross-ns owner ref),
// with WorkspaceReady=True naming the namespace.
func TestProvisionCreatesClaimInTeamNamespace(t *testing.T) {
	spec := &api.PVCSpec{
		Size:        resource.MustParse("25Gi"),
		Class:       "longhorn",
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
	}
	r, cl := newReconciler(t, "", newProject("widget", spec), newTeam("alpha", teamSandboxNS, "widget"))

	reconcileProject(t, r, "widget")

	pvc := getPVC(t, cl, teamSandboxNS, workspace.ProjectPVCName("widget"))
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got != resource.MustParse("25Gi") {
		t.Fatalf("size = %s, want 25Gi", got.String())
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "longhorn" {
		t.Fatalf("class = %v, want longhorn", pvc.Spec.StorageClassName)
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteMany {
		t.Fatalf("accessModes = %v, want [RWX]", pvc.Spec.AccessModes)
	}
	if pvc.Labels[workspace.LabelProject] != "widget" {
		t.Fatalf("project label = %q", pvc.Labels[workspace.LabelProject])
	}
	if pvc.Annotations["k8squad.io/created-by"] != createdByProjectPVC {
		t.Fatalf("created-by annotation = %q", pvc.Annotations["k8squad.io/created-by"])
	}
	if len(pvc.OwnerReferences) != 0 {
		t.Fatalf("team-ns claim must carry no owner ref (cross-ns refs are invalid), got %+v", pvc.OwnerReferences)
	}
	// The Project's own namespace stays claim-free (ISI-4302: a claim
	// there can never bind or be mounted).
	var ownNS corev1.PersistentVolumeClaimList
	if err := cl.List(context.Background(), &ownNS, client.InNamespace("squad-a")); err != nil {
		t.Fatalf("list squad-a PVCs: %v", err)
	}
	if len(ownNS.Items) != 0 {
		t.Fatalf("expected no PVC in the project namespace, got %d", len(ownNS.Items))
	}

	cond := workspaceCondition(t, cl, "widget")
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != reasonProvisioned {
		t.Fatalf("condition = %+v", cond)
	}
	if !strings.Contains(cond.Message, teamSandboxNS) {
		t.Fatalf("condition message %q must name the team namespace", cond.Message)
	}
}

// TestClassResolutionChain: spec.class wins; the operator env fallback
// applies when the spec leaves class empty; with NEITHER, the reconcile
// fails closed — no claim, WorkspaceReady=False/StorageClassUnresolved
// (the PVCSpec anti-cluster-default contract; evaluated before team
// resolution, so no Team is needed for the fail-closed leg).
func TestClassResolutionChain(t *testing.T) {
	spec := &api.PVCSpec{Size: resource.MustParse("10Gi")}

	// Env fallback resolves into the team namespace.
	r, cl := newReconciler(t, "local-path", newProject("envproj", spec), newTeam("alpha", teamSandboxNS, "envproj"))
	reconcileProject(t, r, "envproj")
	pvc := getPVC(t, cl, teamSandboxNS, workspace.ProjectPVCName("envproj"))
	if *pvc.Spec.StorageClassName != "local-path" {
		t.Fatalf("class = %q, want env fallback local-path", *pvc.Spec.StorageClassName)
	}
	// Default access mode is RWO (EffectiveAccessModes, §9.4).
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Fatalf("default accessModes = %v, want [RWO]", pvc.Spec.AccessModes)
	}

	// Neither spec nor env: fail closed.
	r, cl = newReconciler(t, "", newProject("noclass", spec))
	reconcileProject(t, r, "noclass")
	var pvcs corev1.PersistentVolumeClaimList
	if err := cl.List(context.Background(), &pvcs); err != nil {
		t.Fatalf("list PVCs: %v", err)
	}
	if len(pvcs.Items) != 0 {
		t.Fatalf("fail-closed violation: %d PVCs created without a class", len(pvcs.Items))
	}
	cond := workspaceCondition(t, cl, "noclass")
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reasonStorageClassUnresolved {
		t.Fatalf("condition = %+v", cond)
	}
}

// TestNoConsumingTeam (ISI-4302): a workspace-backed Project no Team lists
// gets NO claim anywhere and an actionable WorkspaceReady=False/
// NoConsumingTeam — not a silently-Pending claim in a namespace nothing
// mounts from.
func TestNoConsumingTeam(t *testing.T) {
	spec := &api.PVCSpec{Size: resource.MustParse("10Gi"), Class: "longhorn"}
	r, cl := newReconciler(t, "", newProject("orphan", spec))

	reconcileProject(t, r, "orphan")

	var pvcs corev1.PersistentVolumeClaimList
	if err := cl.List(context.Background(), &pvcs); err != nil {
		t.Fatalf("list PVCs: %v", err)
	}
	if len(pvcs.Items) != 0 {
		t.Fatalf("expected no PVCs without a consuming Team, got %d", len(pvcs.Items))
	}
	cond := workspaceCondition(t, cl, "orphan")
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reasonNoConsumingTeam {
		t.Fatalf("condition = %+v", cond)
	}
}

// TestTeamNamespacePendingThenProvisioned: a referencing Team without a
// resolved status.namespace reports TeamNamespacePending and creates
// nothing; once the team reconciler records the namespace (requeueing this
// Project through the Team watch), the claim lands there.
func TestTeamNamespacePendingThenProvisioned(t *testing.T) {
	spec := &api.PVCSpec{Size: resource.MustParse("10Gi"), Class: "longhorn"}
	team := newTeam("alpha", "", "widget")
	r, cl := newReconciler(t, "", newProject("widget", spec), team)

	reconcileProject(t, r, "widget")
	cond := workspaceCondition(t, cl, "widget")
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reasonTeamNamespacePending {
		t.Fatalf("condition = %+v", cond)
	}
	var pvcs corev1.PersistentVolumeClaimList
	if err := cl.List(context.Background(), &pvcs); err != nil {
		t.Fatalf("list PVCs: %v", err)
	}
	if len(pvcs.Items) != 0 {
		t.Fatalf("expected no PVCs while the team ns is unresolved, got %d", len(pvcs.Items))
	}

	// The team reconciler resolves status.namespace; the Team watch
	// requeues the Project and the claim lands in the sandbox namespace.
	team.Status.Namespace = teamSandboxNS
	if err := cl.Status().Update(context.Background(), team); err != nil {
		t.Fatalf("update team status: %v", err)
	}
	reconcileProject(t, r, "widget")
	getPVC(t, cl, teamSandboxNS, workspace.ProjectPVCName("widget"))
	cond = workspaceCondition(t, cl, "widget")
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("condition after ns resolution = %+v", cond)
	}
}

// TestMultipleTeamsGetClaimsEach: two Teams (one referencing via an
// explicit cross-namespace ref) each get a claim in their own sandbox
// namespace; per-Project data is shared within a team, and cross-team
// isolation follows namespace isolation.
func TestMultipleTeamsGetClaimsEach(t *testing.T) {
	spec := &api.PVCSpec{Size: resource.MustParse("10Gi"), Class: "longhorn"}
	teamA := newTeam("alpha", teamSandboxNS, "widget")
	teamB := newTeam("beta", "ksquad-team-squad-b-99aa88bb", "other")
	teamB.Spec.Projects[0] = api.ObjectRef{Name: "widget", Namespace: "squad-a"}

	r, cl := newReconciler(t, "", newProject("widget", spec), teamA, teamB)

	reconcileProject(t, r, "widget")

	getPVC(t, cl, teamSandboxNS, workspace.ProjectPVCName("widget"))
	getPVC(t, cl, "ksquad-team-squad-b-99aa88bb", workspace.ProjectPVCName("widget"))

	cond := workspaceCondition(t, cl, "widget")
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("condition = %+v", cond)
	}
}

// TestReconcileIsIdempotent: a second pass neither duplicates the claim nor
// rewrites an unchanged status.
func TestReconcileIsIdempotent(t *testing.T) {
	spec := &api.PVCSpec{Size: resource.MustParse("10Gi"), Class: "longhorn"}
	r, cl := newReconciler(t, "", newProject("widget", spec), newTeam("alpha", teamSandboxNS, "widget"))

	reconcileProject(t, r, "widget")
	reconcileProject(t, r, "widget")

	var pvcs corev1.PersistentVolumeClaimList
	if err := cl.List(context.Background(), &pvcs); err != nil {
		t.Fatalf("list PVCs: %v", err)
	}
	if len(pvcs.Items) != 1 {
		t.Fatalf("expected exactly 1 PVC after two reconciles, got %d", len(pvcs.Items))
	}
}

// TestConvergeExpandsOnly: a larger requested size patches the claim in
// place; a smaller one is clamped (no shrink, no error); a class or
// access-mode change is reported as SpecConflict and never mutated.
func TestConvergeExpandsOnly(t *testing.T) {
	spec := &api.PVCSpec{Size: resource.MustParse("10Gi"), Class: "longhorn"}
	r, cl := newReconciler(t, "", newProject("widget", spec), newTeam("alpha", teamSandboxNS, "widget"))
	reconcileProject(t, r, "widget")

	// The reconcile's status patch bumps resourceVersion, so every spec edit
	// must re-fetch first.
	mutate := func(fn func(p *api.Project)) {
		t.Helper()
		p := &api.Project{}
		if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "squad-a", Name: "widget"}, p); err != nil {
			t.Fatalf("get project: %v", err)
		}
		fn(p)
		if err := cl.Update(context.Background(), p); err != nil {
			t.Fatalf("update project: %v", err)
		}
	}

	// Grow: applied.
	mutate(func(p *api.Project) { p.Spec.WorkspacePVC.Size = resource.MustParse("20Gi") })
	reconcileProject(t, r, "widget")
	pvc := getPVC(t, cl, teamSandboxNS, workspace.ProjectPVCName("widget"))
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got != resource.MustParse("20Gi") {
		t.Fatalf("after grow size = %s, want 20Gi", got.String())
	}

	// Shrink: clamped.
	mutate(func(p *api.Project) { p.Spec.WorkspacePVC.Size = resource.MustParse("5Gi") })
	reconcileProject(t, r, "widget")
	pvc = getPVC(t, cl, teamSandboxNS, workspace.ProjectPVCName("widget"))
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got != resource.MustParse("20Gi") {
		t.Fatalf("after shrink size = %s, want clamped 20Gi", got.String())
	}

	// Class change: conflict reported, claim untouched.
	mutate(func(p *api.Project) { p.Spec.WorkspacePVC.Class = "ceph" })
	reconcileProject(t, r, "widget")
	pvc = getPVC(t, cl, teamSandboxNS, workspace.ProjectPVCName("widget"))
	if *pvc.Spec.StorageClassName != "longhorn" {
		t.Fatalf("class mutated to %q; immutable field must stay longhorn", *pvc.Spec.StorageClassName)
	}
	cond := workspaceCondition(t, cl, "widget")
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reasonSpecConflict {
		t.Fatalf("condition = %+v", cond)
	}
}

// TestForeignClaimIsNeverAdopted: a same-named claim in the team namespace
// that is not ours (no project label/annotation identity) is reported
// (NameConflict) and left byte-identical — data safety beats name
// convergence.
func TestForeignClaimIsNeverAdopted(t *testing.T) {
	spec := &api.PVCSpec{Size: resource.MustParse("10Gi"), Class: "longhorn"}
	foreign := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workspace.ProjectPVCName("widget"),
			Namespace: teamSandboxNS,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	r, cl := newReconciler(t, "", newProject("widget", spec), newTeam("alpha", teamSandboxNS, "widget"), foreign)

	reconcileProject(t, r, "widget")

	pvc := getPVC(t, cl, teamSandboxNS, workspace.ProjectPVCName("widget"))
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got != resource.MustParse("1Gi") {
		t.Fatalf("foreign claim mutated to %s", got.String())
	}
	cond := workspaceCondition(t, cl, "widget")
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reasonNameConflict {
		t.Fatalf("condition = %+v", cond)
	}
}

// TestNoWorkspaceSpecIsNoOp: a Project without spec.workspacePVC gets no
// claim and no condition — and spec removal never deletes an existing claim
// (data safety: this loop never destroys claims).
func TestNoWorkspaceSpecIsNoOp(t *testing.T) {
	r, cl := newReconciler(t, "local-path", newProject("bare", nil), newTeam("alpha", teamSandboxNS, "bare"))
	reconcileProject(t, r, "bare")

	var pvcs corev1.PersistentVolumeClaimList
	if err := cl.List(context.Background(), &pvcs); err != nil {
		t.Fatalf("list PVCs: %v", err)
	}
	if len(pvcs.Items) != 0 {
		t.Fatalf("expected no PVCs, got %d", len(pvcs.Items))
	}
	if cond := workspaceCondition(t, cl, "bare"); cond != nil {
		t.Fatalf("unexpected condition %+v", cond)
	}
}
