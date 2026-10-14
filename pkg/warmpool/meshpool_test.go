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
