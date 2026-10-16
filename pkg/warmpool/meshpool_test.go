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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/K8squad/K8squad/pkg/warmpool"
)

func meshScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newMeshProvisioner(t *testing.T) (*warmpool.KubeProvisioner, client.Client) {
	t.Helper()
	cl := fake.NewClientBuilder().WithScheme(meshScheme(t)).Build()
	return warmpool.NewKubeProvisioner(cl, "1", "512Mi"), cl
}

// BootMeshPool creates the standing gateway pod with the D1.2/D2.3 shape:
// Args=["gateway"], the KSQUAD_MESH_* envelope env, mesh labels (disjoint
// from the legacy sandbox selector), no task-io volume, and the :8080 probe
// contract.
func TestBootMeshPool_PodShape(t *testing.T) {
	k, cl := newMeshProvisioner(t)
	err := k.BootMeshPool(context.Background(), warmpool.MeshPoolSpec{
		Team:       "team-acme",
		Role:       "coder",
		Image:      "ghcr.io/k8squad/shim-opencode:dev",
		IngressURL: "http://operator:8090/mesh",
		Model:      "qwen3-coder",
		Skills:     []string{"go", "k8s"},
		ProjectPVC: "proj-workspace",
	})
	if err != nil {
		t.Fatalf("BootMeshPool: %v", err)
	}

	var pod corev1.Pod
	if err := cl.Get(context.Background(), client.ObjectKey{
		Namespace: "default", Name: warmpool.MeshPoolPodName("team-acme", "coder")}, &pod); err != nil {
		t.Fatalf("get pod: %v", err)
	}

	c := pod.Spec.Containers[0]
	if c.Name != "gateway" || len(c.Args) != 1 || c.Args[0] != "gateway" {
		t.Fatalf("container = %s args = %v, want gateway PID1", c.Name, c.Args)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	for name, want := range map[string]string{
		"KSQUAD_MESH_TEAM":              "team-acme",
		"KSQUAD_MESH_ROLE":              "coder",
		"KSQUAD_MESH_MIN":               "1",
		"KSQUAD_MESH_BASE_PORT":         "8081",
		"KSQUAD_MESH_MODEL":             "qwen3-coder",
		"KSQUAD_MESH_SKILLS":            "go,k8s",
		"KSQUAD_SUPERVISOR_INGRESS_URL": "http://operator:8090/mesh",
		"KSQUAD_WORKDIR":                warmpool.WorkspaceMountPath,
	} {
		if env[name] != want {
			t.Fatalf("env %s = %q, want %q", name, env[name], want)
		}
	}
	// The mesh labels must be disjoint from the legacy sandbox selector so
	// adopt/reap never touch a standing pool pod.
	if pod.Labels[warmpool.SandboxAppLabel] == warmpool.SandboxAppValue {
		t.Fatal("mesh pod must not carry the sandbox selector label")
	}
	if pod.Labels[warmpool.MeshAppLabel] != warmpool.MeshAppValue ||
		pod.Labels[warmpool.MeshTeamLabel] != "team-acme" ||
		pod.Labels[warmpool.MeshRoleLabel] != "coder" {
		t.Fatalf("mesh labels = %v", pod.Labels)
	}
	// No task-io volume: a mesh pod never Binds.
	for _, v := range pod.Spec.Volumes {
		if v.Name == "taskio-coord" {
			t.Fatal("mesh pod must not mount the task-io Secret volume")
		}
	}
	// The workspace PVC rides when configured (workdir becomes /workspace).
	if c.WorkingDir != warmpool.WorkspaceMountPath {
		t.Fatalf("workdir = %s, want the workspace mount path", c.WorkingDir)
	}
	// Probes hit the gateway's own :8080 contract.
	if c.LivenessProbe.HTTPGet.Path != "/health" || c.ReadinessProbe.HTTPGet.Path != "/ready" {
		t.Fatalf("probe paths = %s/%s", c.LivenessProbe.HTTPGet.Path, c.ReadinessProbe.HTTPGet.Path)
	}
	// Hardened posture: non-root, no privilege escalation, capabilities dropped.
	if c.SecurityContext.RunAsNonRoot == nil || !*c.SecurityContext.RunAsNonRoot ||
		c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Fatal("mesh container must keep the restricted-PSA posture")
	}
}

// BootMeshPool is idempotent: a second boot with the same (team, role)
// finds the standing pod and creates nothing.
func TestBootMeshPool_Idempotent(t *testing.T) {
	k, cl := newMeshProvisioner(t)
	spec := warmpool.MeshPoolSpec{Team: "team-acme", Role: "coder", Image: "img"}
	if err := k.BootMeshPool(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if err := k.BootMeshPool(context.Background(), spec); err != nil {
		t.Fatalf("re-boot: %v", err)
	}
	var pods corev1.PodList
	if err := cl.List(context.Background(), &pods); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("pods = %d, want exactly 1 standing pool pod", len(pods.Items))
	}
}

// Missing identity or image fails closed before any pod is created.
func TestBootMeshPool_Validation(t *testing.T) {
	k, cl := newMeshProvisioner(t)
	cases := []warmpool.MeshPoolSpec{
		{Role: "coder", Image: "img"},
		{Team: "t", Image: "img"},
		{Team: "t", Role: "coder"},
	}
	for i, spec := range cases {
		if err := k.BootMeshPool(context.Background(), spec); err == nil {
			t.Fatalf("case %d: expected validation error", i)
		}
	}
	var pods corev1.PodList
	if err := cl.List(context.Background(), &pods); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("validation failures must create nothing, got %d pods", len(pods.Items))
	}
}

// Min clamps to 1 — the P1.1 always-up floor.
func TestBootMeshPool_MinClampsToOne(t *testing.T) {
	k, cl := newMeshProvisioner(t)
	if err := k.BootMeshPool(context.Background(), warmpool.MeshPoolSpec{Team: "t", Role: "coder", Image: "img", Min: 0}); err != nil {
		t.Fatal(err)
	}
	var pod corev1.Pod
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "mesh-t-coder"}, &pod); err != nil {
		t.Fatal(err)
	}
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "KSQUAD_MESH_MIN" && e.Value != "1" {
			t.Fatalf("KSQUAD_MESH_MIN = %s, want 1 (clamped)", e.Value)
		}
	}
}

// envOf extracts one env value from the booted pod.
func envOf(t *testing.T, pod *corev1.Pod, name string) string {
	t.Helper()
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// The lifecycle knobs (D4.2) stamp the pod as KSQUAD_MESH_LIFECYCLE_* env —
// only when set (the minimal-env invariant); alwaysUp stamps as "true".
func TestBootMeshPool_LifecycleEnv(t *testing.T) {
	k, cl := newMeshProvisioner(t)
	err := k.BootMeshPool(context.Background(), warmpool.MeshPoolSpec{
		Team: "t", Role: "coordinator", Image: "img",
		Lifecycle: &warmpool.MeshLifecycleSpec{
			CheckInterval:   "45s",
			IdleToHibernate: "2m",
			HibernateToStop: "1h",
			AlwaysUp:        true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var pod corev1.Pod
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "mesh-t-coordinator"}, &pod); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"KSQUAD_MESH_LIFECYCLE_CHECK_INTERVAL":    "45s",
		"KSQUAD_MESH_LIFECYCLE_IDLE_TO_HIBERNATE": "2m",
		"KSQUAD_MESH_LIFECYCLE_HIBERNATE_TO_STOP": "1h",
		"KSQUAD_MESH_LIFECYCLE_ALWAYS_UP":         "true",
	} {
		if got := envOf(t, &pod, name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}

	// Unset knobs stamp nothing — the gateway keeps its defaults.
	k2, cl2 := newMeshProvisioner(t)
	if err := k2.BootMeshPool(context.Background(), warmpool.MeshPoolSpec{Team: "t", Role: "coder", Image: "img"}); err != nil {
		t.Fatal(err)
	}
	var bare corev1.Pod
	if err := cl2.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "mesh-t-coder"}, &bare); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"KSQUAD_MESH_LIFECYCLE_CHECK_INTERVAL",
		"KSQUAD_MESH_LIFECYCLE_IDLE_TO_HIBERNATE",
		"KSQUAD_MESH_LIFECYCLE_HIBERNATE_TO_STOP",
		"KSQUAD_MESH_LIFECYCLE_ALWAYS_UP",
	} {
		if v := envOf(t, &bare, name); v != "" {
			t.Fatalf("%s stamped %q on a spec with no lifecycle — minimal-env violation", name, v)
		}
	}
}

// The S4 hibernation arm (ISI-5831): KSQUAD_MESH_HIBERNATE_ENABLED=true on
// the operator stamps KSQUAD_MESH_LIFECYCLE_HIBERNATE=true into every mesh
// pool pod (arming the two-tier ladder); the default (unset) stamps NOTHING
// — the pod keeps the collapsed P1.2 ladder, so the S4 rollout is an
// explicit, reversible operator switch independent of KSQUAD_MESH_TEAMS.
func TestBootMeshPool_S4HibernateArm(t *testing.T) {
	t.Run("armed", func(t *testing.T) {
		t.Setenv("KSQUAD_MESH_HIBERNATE_ENABLED", "true")
		k, cl := newMeshProvisioner(t)
		if err := k.BootMeshPool(context.Background(), warmpool.MeshPoolSpec{Team: "t", Role: "coder", Image: "img"}); err != nil {
			t.Fatal(err)
		}
		var pod corev1.Pod
		if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "mesh-t-coder"}, &pod); err != nil {
			t.Fatal(err)
		}
		if got := envOf(t, &pod, "KSQUAD_MESH_LIFECYCLE_HIBERNATE"); got != "true" {
			t.Fatalf("KSQUAD_MESH_LIFECYCLE_HIBERNATE = %q, want true (operator switch on)", got)
		}
	})
	t.Run("default-dark", func(t *testing.T) {
		k, cl := newMeshProvisioner(t)
		if err := k.BootMeshPool(context.Background(), warmpool.MeshPoolSpec{Team: "t", Role: "coder", Image: "img"}); err != nil {
			t.Fatal(err)
		}
		var pod corev1.Pod
		if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "mesh-t-coder"}, &pod); err != nil {
			t.Fatal(err)
		}
		if got := envOf(t, &pod, "KSQUAD_MESH_LIFECYCLE_HIBERNATE"); got != "" {
			t.Fatalf("KSQUAD_MESH_LIFECYCLE_HIBERNATE = %q, want unset (S4 default is dark)", got)
		}
	})
}

// The resolved runtime type (P2 close-out follow-up, ISI-5826 comment
// b09e1970): a typed pool stamps KSQUAD_RUNTIME_TYPE so the in-pod
// composition root binds the matching RuntimeAdapter; an untyped pool
// stamps nothing — byte-identical to every pod booted before the seam.
func TestBootMeshPool_RuntimeTypeEnv(t *testing.T) {
	t.Run("typed", func(t *testing.T) {
		k, cl := newMeshProvisioner(t)
		if err := k.BootMeshPool(context.Background(), warmpool.MeshPoolSpec{
			Team: "t", Role: "coder", Image: "img", RuntimeType: "codex",
		}); err != nil {
			t.Fatal(err)
		}
		var pod corev1.Pod
		if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "mesh-t-coder"}, &pod); err != nil {
			t.Fatal(err)
		}
		if got := envOf(t, &pod, "KSQUAD_RUNTIME_TYPE"); got != "codex" {
			t.Fatalf("KSQUAD_RUNTIME_TYPE = %q, want codex", got)
		}
	})
	t.Run("untyped-keeps-default", func(t *testing.T) {
		k, cl := newMeshProvisioner(t)
		if err := k.BootMeshPool(context.Background(), warmpool.MeshPoolSpec{Team: "t", Role: "coder", Image: "img"}); err != nil {
			t.Fatal(err)
		}
		var pod corev1.Pod
		if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "mesh-t-coder"}, &pod); err != nil {
			t.Fatal(err)
		}
		if got := envOf(t, &pod, "KSQUAD_RUNTIME_TYPE"); got != "" {
			t.Fatalf("KSQUAD_RUNTIME_TYPE = %q, want unset (minimal-env invariant)", got)
		}
	})
}
