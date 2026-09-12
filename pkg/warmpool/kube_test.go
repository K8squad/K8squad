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

package warmpool

import (
	"context"
	"fmt"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/K8squad/K8squad/pkg/taskio"
)

func clientObjectKey(t *testing.T, ns, name string) types.NamespacedName {
	t.Helper()
	return types.NamespacedName{Namespace: ns, Name: name}
}

// TestKubeProvisionerBootsInTeamNamespace (Epic C, ADR-044 step 9): a
// classified key carries the Run's team namespace and the sandbox pod
// boots THERE — per-Run Role binding, pod-level NetworkPolicy and quota
// are namespace-scoped (§12.1). Unclassified (legacy) keys keep the
// provisioner default.
func TestKubeProvisionerBootsInTeamNamespace(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1 scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(s).Build()
	p := NewKubeProvisioner(c, "", "")

	ctx := context.Background()
	key := PoolKey{RuntimeClass: "gvisor", Namespace: "bmad-squad", CapabilityHash: "abc", Image: "reg.example/ksquad-shim-codex:m1"}
	if err := p.Boot(ctx, key, "sbx-1"); err != nil {
		t.Fatalf("boot: %v", err)
	}
	pod := &corev1.Pod{}
	if err := c.Get(ctx, clientObjectKey(t, "bmad-squad", "sbx-1"), pod); err != nil {
		t.Fatalf("get pod in team namespace: %v", err)
	}
	if got := pod.Annotations["k8squad.io/pool-key"]; got != "gvisor/reg.example/ksquad-shim-codex:m1" {
		t.Fatalf("pool-key annotation = %q", got)
	}

	// Legacy key without a namespace: the provisioner default remains.
	legacy := PoolKey{RuntimeClass: "gvisor", Image: "reg.example/ksquad-shim-codex:m1"}
	if err := p.Boot(ctx, legacy, "sbx-2"); err != nil {
		t.Fatalf("boot legacy: %v", err)
	}
	if err := c.Get(ctx, clientObjectKey(t, "default", "sbx-2"), &corev1.Pod{}); err != nil {
		t.Fatalf("legacy pod should boot in the default namespace: %v", err)
	}
}

// TestKubeProvisionerBootStampsTraceContext (Epic D / D1, ISI-3348 finding
// 3): Boot under a traced context stamps the W3C carrier
// (TRACEPARENT/TRACESTATE) into the sandbox env — the shim Extracts those at
// startup to continue the Run's distributed trace. A bare context (pool
// warm-boot, no live Run span) stamps nothing.
func TestKubeProvisionerBootStampsTraceContext(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(s).Build()
	p := NewKubeProvisioner(c, "", "")

	// A real traced context: sampled span, the same shape the run-drive
	// pass opens (run.reconcile) when the binder boots a sandbox. The
	// W3C propagator is what telemetry.Setup installs process-wide in the
	// operator; install it here the same way (Inject reads the global).
	otel.SetTextMapPropagator(propagation.TraceContext{})
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	ctx, span := tp.Tracer("test").Start(context.Background(), "run.reconcile")
	defer span.End()

	key := PoolKey{RuntimeClass: "gvisor", Namespace: "bmad-squad", Image: "reg.example/ksquad-shim-codex:m1"}
	if err := p.Boot(ctx, key, "sbx-traced"); err != nil {
		t.Fatalf("boot: %v", err)
	}
	pod := &corev1.Pod{}
	if err := c.Get(context.Background(), clientObjectKey(t, "bmad-squad", "sbx-traced"), pod); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	env := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	sc := span.SpanContext()
	if !sc.IsValid() {
		t.Fatal("test span context invalid")
	}
	want := fmt.Sprintf("00-%s-%s-01", sc.TraceID().String(), sc.SpanID().String())
	if env["TRACEPARENT"] != want {
		t.Fatalf("TRACEPARENT = %q, want %q", env["TRACEPARENT"], want)
	}
	if env["KSQUAD_TOOL_USAGE_ENABLED"] == "" {
		t.Fatal("tool-usage gate env missing")
	}

	// Bare context: no span → no carrier, honestly.
	if err := p.Boot(context.Background(), key, "sbx-bare"); err != nil {
		t.Fatalf("boot bare: %v", err)
	}
	bare := &corev1.Pod{}
	if err := c.Get(context.Background(), clientObjectKey(t, "bmad-squad", "sbx-bare"), bare); err != nil {
		t.Fatalf("get bare pod: %v", err)
	}
	for _, e := range bare.Spec.Containers[0].Env {
		if e.Name == "TRACEPARENT" || e.Name == "TRACESTATE" {
			t.Fatalf("bare boot stamped %s without a traced context", e.Name)
		}
	}
}

// TestKubeProvisionerTearDownUsesKeyNamespace (ISI-4289): Boot creates the
// sandbox pod in the key's team namespace, so TearDown must delete it THERE —
// the adapter hardcoded `default`, and every team-namespace warm pod outlived
// its own teardown (the delete hit a non-existent pod in `default`, the pool
// parked the sandbox as draining, and the real pod leaked as a permanent
// CPU-request orphan). Legacy keys without a namespace keep the default.
func TestKubeProvisionerTearDownUsesKeyNamespace(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(s).Build()
	p := NewKubeProvisioner(c, "", "")

	ctx := context.Background()
	key := PoolKey{RuntimeClass: "gvisor", Namespace: "bmad-squad", Image: "reg.example/ksquad-shim-codex:m1"}
	if err := p.Boot(ctx, key, "sbx-kill"); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if err := p.TearDown(ctx, key, "sbx-kill"); err != nil {
		t.Fatalf("teardown in team namespace: %v", err)
	}
	if err := c.Get(ctx, clientObjectKey(t, "bmad-squad", "sbx-kill"), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("team-namespace pod after teardown: want NotFound, got %v", err)
	}

	// Legacy key (no namespace): teardown keeps the provisioner default.
	legacy := PoolKey{RuntimeClass: "gvisor", Image: "reg.example/ksquad-shim-codex:m1"}
	if err := p.Boot(ctx, legacy, "sbx-legacy"); err != nil {
		t.Fatalf("boot legacy: %v", err)
	}
	if err := p.TearDown(ctx, legacy, "sbx-legacy"); err != nil {
		t.Fatalf("teardown legacy: %v", err)
	}
	if err := c.Get(ctx, clientObjectKey(t, "default", "sbx-legacy"), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("default-namespace pod after teardown: want NotFound, got %v", err)
	}
}

// TestKubeProvisionerBootMountsCoordSecretVolume (ISI-3614, ADR-0007 channel A):
// Boot mounts the per-sandbox task-io Secret at the coord path so the Bind-path
// writer (topology 2) can deliver a run-scoped credential to an already-running
// pod. The volume is OPTIONAL (the Secret only exists after Bind) and the mount
// is read-only. The Secret is named after the pod (== sandbox_ref).
func TestKubeProvisionerBootMountsCoordSecretVolume(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(s).Build()
	p := NewKubeProvisioner(c, "", "")

	ctx := context.Background()
	if err := p.Boot(ctx, PoolKey{RuntimeClass: "gvisor", Image: "reg.example/ksquad-shim-codex:m1"}, "sbx-coord"); err != nil {
		t.Fatalf("boot: %v", err)
	}
	pod := &corev1.Pod{}
	if err := c.Get(ctx, clientObjectKey(t, "default", "sbx-coord"), pod); err != nil {
		t.Fatalf("get pod: %v", err)
	}

	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == taskio.CoordVolumeName {
			vol = &pod.Spec.Volumes[i]
		}
	}
	if vol == nil {
		t.Fatalf("no %q volume on pod; volumes=%+v", taskio.CoordVolumeName, pod.Spec.Volumes)
	}
	if vol.Secret == nil {
		t.Fatalf("%q volume is not a Secret source: %+v", taskio.CoordVolumeName, vol.VolumeSource)
	}
	if vol.Secret.SecretName != "sbx-coord" {
		t.Errorf("Secret name = %q, want the pod/sandbox_ref name %q", vol.Secret.SecretName, "sbx-coord")
	}
	if vol.Secret.Optional == nil || !*vol.Secret.Optional {
		t.Errorf("Secret volume must be Optional (Secret does not exist until Bind); got %v", vol.Secret.Optional)
	}

	var mount *corev1.VolumeMount
	for i := range pod.Spec.Containers[0].VolumeMounts {
		if pod.Spec.Containers[0].VolumeMounts[i].Name == taskio.CoordVolumeName {
			mount = &pod.Spec.Containers[0].VolumeMounts[i]
		}
	}
	if mount == nil {
		t.Fatalf("sandbox container has no %q mount", taskio.CoordVolumeName)
	}
	if mount.MountPath != taskio.CoordMountPath {
		t.Errorf("mount path = %q, want %q", mount.MountPath, taskio.CoordMountPath)
	}
	if !mount.ReadOnly {
		t.Errorf("coord Secret mount should be read-only")
	}
}

// TestKubeProvisionerMountsProjectWorkspace (ISI-4127): a pool key carrying
// ProjectPVC boots the sandbox pod with that claim mounted team-shared at
// /workspace; a key without one boots exactly as before (no workspace
// volume, no mount).
func TestKubeProvisionerMountsProjectWorkspace(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(s).Build()
	p := NewKubeProvisioner(c, "", "")

	ctx := context.Background()
	// M1.2 merged onto main: Boot refuses a key without an image, so both
	// keys carry one (the classifier guarantees it in production).
	key := PoolKey{RuntimeClass: "gvisor", Namespace: "squad-a", Image: "reg/shim:test", ProjectPVC: "workspace-project-widget"}
	if err := p.Boot(ctx, key, "sbx-ws"); err != nil {
		t.Fatalf("boot: %v", err)
	}
	pod := &corev1.Pod{}
	if err := c.Get(ctx, clientObjectKey(t, "squad-a", "sbx-ws"), pod); err != nil {
		t.Fatalf("get pod: %v", err)
	}

	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == WorkspaceVolumeName {
			vol = &pod.Spec.Volumes[i]
		}
	}
	if vol == nil {
		t.Fatalf("no %q volume on pod; volumes=%+v", WorkspaceVolumeName, pod.Spec.Volumes)
	}
	if vol.PersistentVolumeClaim == nil || vol.PersistentVolumeClaim.ClaimName != "workspace-project-widget" {
		t.Fatalf("workspace volume = %+v, want PVC workspace-project-widget", vol.VolumeSource)
	}
	if vol.PersistentVolumeClaim.ReadOnly {
		t.Errorf("workspace mount must be read-write (team-shared)")
	}

	var mount *corev1.VolumeMount
	for i := range pod.Spec.Containers[0].VolumeMounts {
		if pod.Spec.Containers[0].VolumeMounts[i].Name == WorkspaceVolumeName {
			mount = &pod.Spec.Containers[0].VolumeMounts[i]
		}
	}
	if mount == nil {
		t.Fatalf("sandbox container has no %q mount", WorkspaceVolumeName)
	}
	if mount.MountPath != WorkspaceMountPath {
		t.Errorf("mount path = %q, want %q", mount.MountPath, WorkspaceMountPath)
	}
	if mount.SubPath != "" {
		t.Errorf("subPath = %q; the Boot-time mount is whole-volume (principal unknown until Bind)", mount.SubPath)
	}

	// No PVC in the key: no workspace volume, no workspace mount.
	if err := p.Boot(ctx, PoolKey{RuntimeClass: "gvisor", Namespace: "squad-a", Image: "reg/shim:test"}, "sbx-plain"); err != nil {
		t.Fatalf("boot plain: %v", err)
	}
	plain := &corev1.Pod{}
	if err := c.Get(ctx, clientObjectKey(t, "squad-a", "sbx-plain"), plain); err != nil {
		t.Fatalf("get plain pod: %v", err)
	}
	for _, v := range plain.Spec.Volumes {
		if v.Name == WorkspaceVolumeName {
			t.Fatalf("PVC-less key must not emit a workspace volume")
		}
	}
	for _, m := range plain.Spec.Containers[0].VolumeMounts {
		if m.Name == WorkspaceVolumeName {
			t.Fatalf("PVC-less key must not emit a workspace mount")
		}
	}
}

// TestKubeProvisionerBootStampsWritableWorkDir (ISI-4188 gap 1): every
// sandbox container boots with a writable WorkingDir plus the
// KSQUAD_WORKDIR/HOME env pair — without it the runtime CLIs run at cwd "/"
// and die on their first cache write (`EACCES: mkdir '/.local'`), before any
// model call or span. A per-Project workspace mount wins over the /tmp
// fallback.
func TestKubeProvisionerBootStampsWritableWorkDir(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(s).Build()
	p := NewKubeProvisioner(c, "", "")

	ctx := context.Background()
	if err := p.Boot(ctx, PoolKey{RuntimeClass: "runc", Image: "reg.example/ksquad-shim-opencode:m1"}, "sbx-workdir"); err != nil {
		t.Fatalf("boot: %v", err)
	}
	pod := &corev1.Pod{}
	if err := c.Get(ctx, clientObjectKey(t, "default", "sbx-workdir"), pod); err != nil {
		t.Fatalf("get pod: %v", err)
	}

	ctr := pod.Spec.Containers[0]
	if ctr.WorkingDir != sandboxWorkDir {
		t.Errorf("container WorkingDir = %q, want %q", ctr.WorkingDir, sandboxWorkDir)
	}
	env := map[string]string{}
	for _, e := range ctr.Env {
		env[e.Name] = e.Value
	}
	if env["KSQUAD_WORKDIR"] != sandboxWorkDir {
		t.Errorf("KSQUAD_WORKDIR = %q, want %q", env["KSQUAD_WORKDIR"], sandboxWorkDir)
	}
	if env["HOME"] != sandboxWorkDir {
		t.Errorf("HOME = %q, want %q (cache roots must be writable)", env["HOME"], sandboxWorkDir)
	}

	// With a per-Project workspace the shared mount is the workdir.
	if err := p.Boot(ctx, PoolKey{RuntimeClass: "runc", Image: "reg.example/ksquad-shim-opencode:m1", ProjectPVC: "workspace-project-x"}, "sbx-ws"); err != nil {
		t.Fatalf("boot ws: %v", err)
	}
	ws := &corev1.Pod{}
	if err := c.Get(ctx, clientObjectKey(t, "default", "sbx-ws"), ws); err != nil {
		t.Fatalf("get ws pod: %v", err)
	}
	if got := ws.Spec.Containers[0].WorkingDir; got != WorkspaceMountPath {
		t.Errorf("workspace WorkingDir = %q, want %q", got, WorkspaceMountPath)
	}
	wsEnv := map[string]string{}
	for _, e := range ws.Spec.Containers[0].Env {
		wsEnv[e.Name] = e.Value
	}
	if wsEnv["KSQUAD_WORKDIR"] != WorkspaceMountPath || wsEnv["HOME"] != WorkspaceMountPath {
		t.Errorf("workspace env = %q, want KSQUAD_WORKDIR/HOME = %q", wsEnv, WorkspaceMountPath)
	}
}

// TestKubeProvisionerBootRightSizesRequests (ISI-4208): WithRequests decouples
// the scheduling reservation from the burst ceiling — a warm sandbox reserves
// 500m CPU while keeping the 1-CPU limit. Without WithRequests the historical
// requests==limits (Guaranteed QoS) posture is preserved.
func TestKubeProvisionerBootRightSizesRequests(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(s).Build()
	ctx := context.Background()
	img := PoolKey{RuntimeClass: "runc", Image: "reg.example/ksquad-shim-opencode:m1"}

	p := NewKubeProvisioner(c, "1", "512Mi").WithRequests("500m", "512Mi")
	if err := p.Boot(ctx, img, "sbx-rightsized"); err != nil {
		t.Fatalf("boot: %v", err)
	}
	sized := &corev1.Pod{}
	if err := c.Get(ctx, clientObjectKey(t, "default", "sbx-rightsized"), sized); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	res := sized.Spec.Containers[0].Resources
	if res.Requests.Cpu().Cmp(resource.MustParse("500m")) != 0 {
		t.Errorf("cpu request = %v, want 500m", res.Requests.Cpu())
	}
	if res.Limits.Cpu().Cmp(resource.MustParse("1")) != 0 {
		t.Errorf("cpu limit = %v, want 1 (burst ceiling unchanged)", res.Limits.Cpu())
	}
	if res.Requests.Memory().Cmp(resource.MustParse("512Mi")) != 0 {
		t.Errorf("memory request = %v, want 512Mi", res.Requests.Memory())
	}
	if res.Limits.Memory().Cmp(resource.MustParse("512Mi")) != 0 {
		t.Errorf("memory limit = %v, want 512Mi", res.Limits.Memory())
	}

	// Default (no WithRequests): requests==limits, Guaranteed QoS preserved.
	dflt := NewKubeProvisioner(c, "1", "512Mi")
	if err := dflt.Boot(ctx, img, "sbx-guaranteed"); err != nil {
		t.Fatalf("boot default: %v", err)
	}
	pod := &corev1.Pod{}
	if err := c.Get(ctx, clientObjectKey(t, "default", "sbx-guaranteed"), pod); err != nil {
		t.Fatalf("get default pod: %v", err)
	}
	dres := pod.Spec.Containers[0].Resources
	if dres.Requests.Cpu().Cmp(*dres.Limits.Cpu()) != 0 {
		t.Errorf("default cpu request %v should equal limit %v", dres.Requests.Cpu(), dres.Limits.Cpu())
	}
}
