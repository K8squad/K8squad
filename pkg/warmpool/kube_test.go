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
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/K8squad/K8squad/pkg/capability"
	"github.com/K8squad/K8squad/pkg/taskio"
	"github.com/K8squad/K8squad/pkg/toolchain"
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
	if err := p.Boot(ctx, key, "sbx-1", "", BootWarm); err != nil {
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
	if err := p.Boot(ctx, legacy, "sbx-2", "", BootWarm); err != nil {
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
	if err := p.Boot(ctx, key, "sbx-traced", "", BootWarm); err != nil {
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
	if err := p.Boot(context.Background(), key, "sbx-bare", "", BootWarm); err != nil {
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

// TestKubeProvisionerBootStampsPodEnvForEveryPurpose (ISI-7690 / ISI-7686
// FQDN-BYO-proxy branch): the KSQUAD_SANDBOX_*_PROXY values are appended to the
// one shared provisioner via WithPodEnv (cmd/operator/main.go), and Boot stamps
// k.podEnv into the SINGLE container env assembly regardless of BootPurpose.
// ISI-7695 live-verified that warm & Run pods therefore cannot diverge on the
// proxy env; this test is the code-level regression guard for that invariant,
// so a future per-purpose bind/cold-boot builder refactor (the ISI-7511 sticky
// admission / ISI-7487 pre-bind queueing class) cannot silently reintroduce the
// "Run cold-boot pods lack the proxy env" divergence that was hypothesized here.
func TestKubeProvisionerBootStampsPodEnvForEveryPurpose(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(s).Build()

	proxyEnv := []corev1.EnvVar{
		{Name: "KSQUAD_SANDBOX_HTTPS_PROXY", Value: "http://egress-proxy.ksquad-egress.svc.cluster.local:3128"},
		{Name: "KSQUAD_SANDBOX_HTTP_PROXY", Value: "http://egress-proxy.ksquad-egress.svc.cluster.local:3128"},
		{Name: "KSQUAD_SANDBOX_NO_PROXY", Value: "10.0.0.0/8,.svc,.cluster.local"},
	}
	p := NewKubeProvisioner(c, "", "").WithPodEnv(proxyEnv...)

	ctx := context.Background()
	key := PoolKey{RuntimeClass: "gvisor", Namespace: "bmad-squad", Image: "reg.example/ksquad-shim-codex:m1"}

	// Every BootPurpose must carry the podEnv — most critically BootRun, the
	// cold-boot path a Run takes when no warm pod is available, since that is
	// the path FQDN BYO model endpoints hit under ksquad-default-deny.
	for _, purpose := range []BootPurpose{BootWarm, BootRun} {
		id := fmt.Sprintf("sbx-%s", purpose)
		if err := p.Boot(ctx, key, id, "run-"+id, purpose); err != nil {
			t.Fatalf("boot %s: %v", purpose, err)
		}
		pod := &corev1.Pod{}
		if err := c.Get(ctx, clientObjectKey(t, "bmad-squad", id), pod); err != nil {
			t.Fatalf("get %s pod: %v", purpose, err)
		}
		env := map[string]string{}
		for _, e := range pod.Spec.Containers[0].Env {
			env[e.Name] = e.Value
		}
		for _, want := range proxyEnv {
			if got := env[want.Name]; got != want.Value {
				t.Errorf("purpose %s: pod env %s = %q, want %q (FQDN BYO endpoints egress DIRECT without it)", purpose, want.Name, got, want.Value)
			}
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
	if err := p.Boot(ctx, key, "sbx-kill", "", BootWarm); err != nil {
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
	if err := p.Boot(ctx, legacy, "sbx-legacy", "", BootWarm); err != nil {
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
	if err := p.Boot(ctx, PoolKey{RuntimeClass: "gvisor", Image: "reg.example/ksquad-shim-codex:m1"}, "sbx-coord", "", BootWarm); err != nil {
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
	if err := p.Boot(ctx, key, "sbx-ws", "", BootWarm); err != nil {
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
	if err := p.Boot(ctx, PoolKey{RuntimeClass: "gvisor", Namespace: "squad-a", Image: "reg/shim:test"}, "sbx-plain", "", BootWarm); err != nil {
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
	if err := p.Boot(ctx, PoolKey{RuntimeClass: "runc", Image: "reg.example/ksquad-shim-opencode:m1"}, "sbx-workdir", "", BootWarm); err != nil {
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
	if err := p.Boot(ctx, PoolKey{RuntimeClass: "runc", Image: "reg.example/ksquad-shim-opencode:m1", ProjectPVC: "workspace-project-x"}, "sbx-ws", "", BootWarm); err != nil {
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
	if err := p.Boot(ctx, img, "sbx-rightsized", "", BootWarm); err != nil {
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
	if err := dflt.Boot(ctx, img, "sbx-guaranteed", "", BootWarm); err != nil {
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

// TestKubeProvisionerBootEphemeralStorage (ISI-5558): every sandbox pod boots
// with an ephemeral-storage request AND limit so the scheduler stops
// over-packing disk-hungry sandboxes onto a small worker partition (the
// node-wide DiskPressure trigger) and the kubelet evicts a runaway rather than
// a bystander. The guard defaults on (bare caller) and is operator-tunable.
func TestKubeProvisionerBootEphemeralStorage(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(s).Build()
	ctx := context.Background()
	img := PoolKey{RuntimeClass: "runc", Image: "reg.example/ksquad-shim-opencode:m1"}

	// Default (no WithEphemeralStorage): built-in 1Gi request / 2Gi limit.
	dflt := NewKubeProvisioner(c, "", "")
	if err := dflt.Boot(ctx, img, "sbx-es-default", "", BootWarm); err != nil {
		t.Fatalf("boot default: %v", err)
	}
	pod := &corev1.Pod{}
	if err := c.Get(ctx, clientObjectKey(t, "default", "sbx-es-default"), pod); err != nil {
		t.Fatalf("get default pod: %v", err)
	}
	dres := pod.Spec.Containers[0].Resources
	if got := dres.Requests[corev1.ResourceEphemeralStorage]; got.Cmp(resource.MustParse("1Gi")) != 0 {
		t.Errorf("default ephemeral-storage request = %v, want 1Gi", got)
	}
	if got := dres.Limits[corev1.ResourceEphemeralStorage]; got.Cmp(resource.MustParse("2Gi")) != 0 {
		t.Errorf("default ephemeral-storage limit = %v, want 2Gi", got)
	}

	// Operator-tuned: WithEphemeralStorage overrides both halves.
	tuned := NewKubeProvisioner(c, "", "").WithEphemeralStorage("4Gi", "8Gi")
	if err := tuned.Boot(ctx, img, "sbx-es-tuned", "", BootWarm); err != nil {
		t.Fatalf("boot tuned: %v", err)
	}
	tpod := &corev1.Pod{}
	if err := c.Get(ctx, clientObjectKey(t, "default", "sbx-es-tuned"), tpod); err != nil {
		t.Fatalf("get tuned pod: %v", err)
	}
	tres := tpod.Spec.Containers[0].Resources
	if got := tres.Requests[corev1.ResourceEphemeralStorage]; got.Cmp(resource.MustParse("4Gi")) != 0 {
		t.Errorf("tuned ephemeral-storage request = %v, want 4Gi", got)
	}
	if got := tres.Limits[corev1.ResourceEphemeralStorage]; got.Cmp(resource.MustParse("8Gi")) != 0 {
		t.Errorf("tuned ephemeral-storage limit = %v, want 8Gi", got)
	}
	// CPU/memory Guaranteed QoS posture is untouched by the disk guard.
	if tres.Requests.Cpu().Cmp(*tres.Limits.Cpu()) != 0 {
		t.Errorf("cpu request %v should still equal limit %v", tres.Requests.Cpu(), tres.Limits.Cpu())
	}
}

// ISI-4315: purpose-keyed scheduling priority. Idle warm boots stamp the
// WARM class, Run cold boots stamp the RUN class (warm < run so the
// scheduler never hands a freed CPU slot to an older queued warm pod while
// a project Run's cold boot waits); WithPriorities unset stamps nothing
// (pre-ISI-4315 cluster-default priority).
func TestKubeProvisionerStampsPurposePriority(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1 scheme: %v", err)
	}
	key := PoolKey{RuntimeClass: "runc", Image: "reg.example/ksquad-shim-codex:m1", Namespace: "squad-a"}

	stamping := NewKubeProvisioner(fake.NewClientBuilder().WithScheme(s).Build(), "", "").
		WithPriorities("ksquad-sandbox-warm", "ksquad-sandbox-run")
	if err := stamping.Boot(context.Background(), key, "sbx-warm-prio", "", BootWarm); err != nil {
		t.Fatalf("warm boot: %v", err)
	}
	if err := stamping.Boot(context.Background(), key, "sbx-run-prio", "", BootRun); err != nil {
		t.Fatalf("run boot: %v", err)
	}

	var warmPod, runPod corev1.Pod
	c := stamping.client
	if err := c.Get(context.Background(), clientObjectKey(t, "squad-a", "sbx-warm-prio"), &warmPod); err != nil {
		t.Fatalf("get warm pod: %v", err)
	}
	if err := c.Get(context.Background(), clientObjectKey(t, "squad-a", "sbx-run-prio"), &runPod); err != nil {
		t.Fatalf("get run pod: %v", err)
	}
	if got := warmPod.Spec.PriorityClassName; got != "ksquad-sandbox-warm" {
		t.Errorf("warm boot PriorityClassName = %q, want ksquad-sandbox-warm", got)
	}
	if got := runPod.Spec.PriorityClassName; got != "ksquad-sandbox-run" {
		t.Errorf("run boot PriorityClassName = %q, want ksquad-sandbox-run", got)
	}

	// Unset priorities: no stamping — the cluster default stands.
	plain := NewKubeProvisioner(fake.NewClientBuilder().WithScheme(s).Build(), "", "")
	if err := plain.Boot(context.Background(), key, "sbx-plain-prio", "", BootWarm); err != nil {
		t.Fatalf("plain warm boot: %v", err)
	}
	var plainPod corev1.Pod
	if err := plain.client.Get(context.Background(), clientObjectKey(t, "squad-a", "sbx-plain-prio"), &plainPod); err != nil {
		t.Fatalf("get plain pod: %v", err)
	}
	if got := plainPod.Spec.PriorityClassName; got != "" {
		t.Errorf("unstamped boot PriorityClassName = %q, want empty (cluster default)", got)
	}
}

// TestKubeProvisionerStagesToolchainInitPacks (ISI-5221): the BootRun cold path
// attaches the Run's resolved toolchain init packs — the edge that was missing,
// so live sandboxes booted with no git/curl and git/dtctl skills were unusable.
// A resolver returning git+curl must yield one `stage-<name>` init container per
// toolchain, the shared /tools emptyDir, the agent container's read-only mount,
// and /tools/bin on PATH.
func TestKubeProvisionerStagesToolchainInitPacks(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1 scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(s).Build()
	p := NewKubeProvisioner(c, "", "").WithToolchainResolver(
		func(_ context.Context, runID string) ([]toolchain.Resolved, error) {
			if runID != "run-uid-1" {
				t.Errorf("resolver got runID %q, want run-uid-1", runID)
			}
			return []toolchain.Resolved{
				{Name: "git", Version: "2.45", Image: "reg.example/toolchain-git:2.45"},
				{Name: "curl", Version: "8.7", Image: "reg.example/toolchain-curl:8.7"},
			}, nil
		})

	ctx := context.Background()
	key := PoolKey{RuntimeClass: "gvisor", Namespace: "bmad-squad", CapabilityHash: "cap-hash", Image: "reg.example/ksquad-shim-opencode:m1"}
	if err := p.Boot(ctx, key, "sbx-cap", "run-uid-1", BootRun); err != nil {
		t.Fatalf("boot: %v", err)
	}
	pod := &corev1.Pod{}
	if err := c.Get(ctx, clientObjectKey(t, "bmad-squad", "sbx-cap"), pod); err != nil {
		t.Fatalf("get pod: %v", err)
	}

	// One stage-<name> init container per toolchain, resolver order re-sorted
	// by name (curl before git).
	if got := len(pod.Spec.InitContainers); got != 2 {
		t.Fatalf("init containers = %d, want 2 (%+v)", got, pod.Spec.InitContainers)
	}
	wantInit := map[string]string{
		"stage-git":  "reg.example/toolchain-git:2.45",
		"stage-curl": "reg.example/toolchain-curl:8.7",
	}
	for _, ic := range pod.Spec.InitContainers {
		img, ok := wantInit[ic.Name]
		if !ok {
			t.Errorf("unexpected init container %q", ic.Name)
			continue
		}
		if ic.Image != img {
			t.Errorf("init %q image = %q, want %q", ic.Name, ic.Image, img)
		}
		// ISI-5493: staging init containers must satisfy the restricted
		// PodSecurity standard squad namespaces enforce, or the API server
		// rejects the pod and the run never binds a sandbox.
		sc := ic.SecurityContext
		if sc == nil {
			t.Errorf("init %q has no securityContext", ic.Name)
			continue
		}
		if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
			t.Errorf("init %q runAsNonRoot = %v, want true", ic.Name, sc.RunAsNonRoot)
		}
		if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("init %q seccompProfile = %+v, want RuntimeDefault", ic.Name, sc.SeccompProfile)
		}
		if sc.RunAsUser == nil || *sc.RunAsUser == 0 {
			t.Errorf("init %q runAsUser = %v, want a non-root UID", ic.Name, sc.RunAsUser)
		}
		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Errorf("init %q allowPrivilegeEscalation = %v, want false", ic.Name, sc.AllowPrivilegeEscalation)
		}
		if sc.Capabilities == nil || len(sc.Capabilities.Drop) == 0 || sc.Capabilities.Drop[0] != "ALL" {
			t.Errorf("init %q capabilities.drop = %+v, want [ALL]", ic.Name, sc.Capabilities)
		}
	}

	// ISI-5493: the pod-level posture carries runAsNonRoot + seccompProfile
	// (not just a non-root UID) so the whole pod is uniformly restricted.
	if psc := pod.Spec.SecurityContext; psc == nil {
		t.Errorf("pod has no securityContext")
	} else {
		if psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot {
			t.Errorf("pod runAsNonRoot = %v, want true", psc.RunAsNonRoot)
		}
		if psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("pod seccompProfile = %+v, want RuntimeDefault", psc.SeccompProfile)
		}
	}

	// The shared tool volume is present.
	foundVol := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == capability.ToolVolumeName {
			foundVol = true
			if v.EmptyDir == nil {
				t.Errorf("tool volume %q is not an emptyDir", v.Name)
			}
		}
	}
	if !foundVol {
		t.Errorf("tool volume %q missing from pod (%+v)", capability.ToolVolumeName, pod.Spec.Volumes)
	}

	// The agent container mounts /tools read-only and carries /tools/bin on PATH.
	agent := pod.Spec.Containers[0]
	foundMount := false
	for _, m := range agent.VolumeMounts {
		if m.Name == capability.ToolVolumeName {
			foundMount = true
			if m.MountPath != capability.ToolMountPath || !m.ReadOnly {
				t.Errorf("tool mount = %+v, want %s read-only", m, capability.ToolMountPath)
			}
		}
	}
	if !foundMount {
		t.Errorf("agent container missing tool mount (%+v)", agent.VolumeMounts)
	}
	foundPath := false
	for _, e := range agent.Env {
		if e.Name == "PATH" {
			foundPath = true
			if e.Value != capability.ToolPathValue {
				t.Errorf("PATH = %q, want %q", e.Value, capability.ToolPathValue)
			}
		}
	}
	if !foundPath {
		t.Errorf("agent container missing PATH env (%+v)", agent.Env)
	}
	// ISI-5562: the agent also carries LD_LIBRARY_PATH=/tools/lib so a staged
	// dynamically linked tool (musl git) finds its shared-library closure.
	foundLibPath := false
	for _, e := range agent.Env {
		if e.Name == "LD_LIBRARY_PATH" {
			foundLibPath = true
			if e.Value != capability.ToolLibPathValue {
				t.Errorf("LD_LIBRARY_PATH = %q, want %q", e.Value, capability.ToolLibPathValue)
			}
		}
	}
	if !foundLibPath {
		t.Errorf("agent container missing LD_LIBRARY_PATH env (%+v)", agent.Env)
	}
}

// TestKubeProvisionerBareWhenNoToolchains (ISI-5221): a warm boot (no Run) and a
// resolver that returns no toolchains both leave the sandbox bare — no init
// containers, no tool volume — so bare Runs keep sharing bare warm stock.
func TestKubeProvisionerBareWhenNoToolchains(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1 scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(s).Build()
	resolverCalls := 0
	p := NewKubeProvisioner(c, "", "").WithToolchainResolver(
		func(_ context.Context, _ string) ([]toolchain.Resolved, error) {
			resolverCalls++
			return nil, nil // bare envelope
		})
	ctx := context.Background()
	key := PoolKey{RuntimeClass: "gvisor", Namespace: "bmad-squad", Image: "reg.example/ksquad-shim-opencode:m1"}

	// Warm boot: the resolver must NOT even be consulted (no Run).
	if err := p.Boot(ctx, key, "sbx-warm", "", BootWarm); err != nil {
		t.Fatalf("warm boot: %v", err)
	}
	if resolverCalls != 0 {
		t.Errorf("warm boot consulted the toolchain resolver %d times, want 0", resolverCalls)
	}
	warm := &corev1.Pod{}
	if err := c.Get(ctx, clientObjectKey(t, "bmad-squad", "sbx-warm"), warm); err != nil {
		t.Fatalf("get warm pod: %v", err)
	}
	if len(warm.Spec.InitContainers) != 0 {
		t.Errorf("warm boot has %d init containers, want 0 (bare)", len(warm.Spec.InitContainers))
	}

	// BootRun with a bare-envelope resolver: consulted, but nothing staged.
	if err := p.Boot(ctx, key, "sbx-bare-run", "run-uid-bare", BootRun); err != nil {
		t.Fatalf("bare run boot: %v", err)
	}
	if resolverCalls != 1 {
		t.Errorf("BootRun resolver calls = %d, want 1", resolverCalls)
	}
	bare := &corev1.Pod{}
	if err := c.Get(ctx, clientObjectKey(t, "bmad-squad", "sbx-bare-run"), bare); err != nil {
		t.Fatalf("get bare-run pod: %v", err)
	}
	if len(bare.Spec.InitContainers) != 0 {
		t.Errorf("bare-envelope run has %d init containers, want 0", len(bare.Spec.InitContainers))
	}
}

// TestKubeProvisionerFailsClosedOnResolverError (ISI-5221): a capability Run
// whose toolchains cannot be resolved must NOT boot a bare (tool-less) sandbox —
// the boot fails so the bind re-drives instead of silently degrading.
func TestKubeProvisionerFailsClosedOnResolverError(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1 scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(s).Build()
	p := NewKubeProvisioner(c, "", "").WithToolchainResolver(
		func(_ context.Context, _ string) ([]toolchain.Resolved, error) {
			return nil, fmt.Errorf("manifest not stamped yet")
		})
	ctx := context.Background()
	key := PoolKey{RuntimeClass: "gvisor", Namespace: "bmad-squad", CapabilityHash: "cap", Image: "reg.example/ksquad-shim-opencode:m1"}
	err := p.Boot(ctx, key, "sbx-fail", "run-uid-x", BootRun)
	if err == nil {
		t.Fatalf("boot succeeded, want fail-closed on resolver error")
	}
	// The pod must not have been created.
	if getErr := c.Get(ctx, clientObjectKey(t, "bmad-squad", "sbx-fail"), &corev1.Pod{}); !apierrors.IsNotFound(getErr) {
		t.Errorf("pod was created despite resolver error (get err = %v)", getErr)
	}
}

// bootCapturingCtx builds a context carrying a capturing logr sink so a test
// can assert on what KubeProvisioner.Boot logs via log.FromContext. Injecting
// the logger into the context (not the package global) is the deterministic
// path — earlier harness calls in the suite may already have bound the global.
func bootCapturingCtx(lines *[]string) context.Context {
	return crlog.IntoContext(context.Background(), funcr.New(func(_, msg string) {
		*lines = append(*lines, msg)
	}, funcr.Options{}))
}

func bootProxyGuardScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return s
}

// TestKubeProvisionerBootRunProxyGuard (ISI-7691): the fail-loud boot guard's
// acceptance rule must match the consumer's (pkg/shim.agentProxyEnv trims and
// drops whitespace-only values). Every row here pins a mutation that Cursor's
// review of PR #1062 showed surviving the suite: NO_PROXY-only counting as
// proxied, an empty value counting as proxied, a whitespace value counting as
// proxied, and NO_PROXY=* bypassing the proxy for every host — each MUST warn.
func TestKubeProvisionerBootRunProxyGuard(t *testing.T) {
	rows := []struct {
		name     string
		podEnv   []corev1.EnvVar
		wantWarn bool
	}{
		{"no env at all warns", nil, true},
		{"NO_PROXY only is not proxied", []corev1.EnvVar{{Name: "KSQUAD_SANDBOX_NO_PROXY", Value: ".svc,.cluster.local"}}, true},
		{"empty HTTPS_PROXY is not proxied", []corev1.EnvVar{{Name: "KSQUAD_SANDBOX_HTTPS_PROXY", Value: ""}}, true},
		{"whitespace HTTPS_PROXY is not proxied (consumer trims)", []corev1.EnvVar{{Name: "KSQUAD_SANDBOX_HTTPS_PROXY", Value: "  \t\n "}}, true},
		{"NO_PROXY=* bypasses the proxy for all hosts", []corev1.EnvVar{
			{Name: "KSQUAD_SANDBOX_HTTPS_PROXY", Value: "http://ksquad-egress.ksquad-system.svc:8888"},
			{Name: "KSQUAD_SANDBOX_NO_PROXY", Value: "*"},
		}, true},
		{"HTTPS_PROXY set stays silent", []corev1.EnvVar{{Name: "KSQUAD_SANDBOX_HTTPS_PROXY", Value: "http://ksquad-egress.ksquad-system.svc:8888"}}, false},
		{"HTTP_PROXY set stays silent", []corev1.EnvVar{{Name: "KSQUAD_SANDBOX_HTTP_PROXY", Value: "http://ksquad-egress.ksquad-system.svc:8888"}}, false},
		{"HTTPS_PROXY plus scoped NO_PROXY stays silent", []corev1.EnvVar{
			{Name: "KSQUAD_SANDBOX_HTTPS_PROXY", Value: "http://ksquad-egress.ksquad-system.svc:8888"},
			{Name: "KSQUAD_SANDBOX_NO_PROXY", Value: ".svc,.cluster.local,10.0.0.185"},
		}, false},
	}
	for i, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(bootProxyGuardScheme(t)).Build()
			p := NewKubeProvisioner(c, "", "").WithPodEnv(row.podEnv...)

			var lines []string
			ctx := bootCapturingCtx(&lines)
			key := PoolKey{RuntimeClass: "gvisor", Namespace: "ksquad-team-squad", CapabilityHash: "abc", Image: "reg.example/ksquad-shim-codex:m1"}
			if err := p.Boot(ctx, key, fmt.Sprintf("sbx-guard-%d", i), "run-uid-guard", BootRun); err != nil {
				t.Fatalf("boot: %v", err)
			}
			warned := false
			for _, l := range lines {
				if strings.Contains(l, "NO effective KSQUAD_SANDBOX_*_PROXY") {
					warned = true
				}
			}
			if warned != row.wantWarn {
				t.Fatalf("wantWarn=%v got warned=%v; log lines=%v", row.wantWarn, warned, lines)
			}
			if p.SandboxEgressProxied() == row.wantWarn {
				t.Fatalf("SandboxEgressProxied()=%v inconsistent with wantWarn=%v", p.SandboxEgressProxied(), row.wantWarn)
			}
		})
	}
}

// TestKubeProvisionerBootRunProxyGuardSpanAttr (ISI-7691 review #3): the
// guard stamps ksquad.sandbox.egress.proxied onto the boot's trace span so the
// posture is queryable in coord telemetry, not just in the operator log.
func TestKubeProvisionerBootRunProxyGuardSpanAttr(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	c := fake.NewClientBuilder().WithScheme(bootProxyGuardScheme(t)).Build()
	p := NewKubeProvisioner(c, "", "")

	var lines []string
	ctx, span := tp.Tracer("test").Start(bootCapturingCtx(&lines), "run.reconcile")
	key := PoolKey{RuntimeClass: "gvisor", Namespace: "ksquad-team-squad", CapabilityHash: "abc", Image: "reg.example/ksquad-shim-codex:m1"}
	if err := p.Boot(ctx, key, "sbx-span", "run-uid-span", BootRun); err != nil {
		t.Fatalf("boot: %v", err)
	}
	span.End()

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("want 1 ended span, got %d", len(spans))
	}
	var got *bool
	for _, a := range spans[0].Attributes() {
		if a.Key == "ksquad.sandbox.egress.proxied" {
			v := a.Value.AsBool()
			got = &v
		}
	}
	if got == nil {
		t.Fatalf("span %q missing ksquad.sandbox.egress.proxied attribute; attrs=%v", spans[0].Name(), spans[0].Attributes())
	}
	if *got != false {
		t.Fatalf("ksquad.sandbox.egress.proxied = %v, want false for a proxy-less cold boot", *got)
	}
}

// TestBootMeshPoolProxyGuard (ISI-7691 review #2): a mesh pool pod dials the
// model endpoint through its standing shim-supervisor children, so BootMeshPool
// must fail loud when the pod carries no effective proxy env — and stay quiet
// when it does.
func TestBootMeshPoolProxyGuard(t *testing.T) {
	spec := MeshPoolSpec{
		Team: "squad", Role: "coder", Image: "reg.example/ksquad-mesh:m1",
		Namespace: "ksquad-team-squad", Model: "https://api.deepseek.com/v1/chat/completions",
	}

	t.Run("proxy-less mesh pod warns", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(bootProxyGuardScheme(t)).Build()
		p := NewKubeProvisioner(c, "", "")
		var lines []string
		if err := p.BootMeshPool(bootCapturingCtx(&lines), spec); err != nil {
			t.Fatalf("BootMeshPool: %v", err)
		}
		warned := false
		for _, l := range lines {
			if strings.Contains(l, "mesh pool pod has NO effective KSQUAD_SANDBOX_*_PROXY") {
				warned = true
			}
		}
		if !warned {
			t.Fatalf("proxy-less mesh boot emitted no fail-loud warning; lines=%v", lines)
		}
	})

	t.Run("proxied mesh pod stays silent", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(bootProxyGuardScheme(t)).Build()
		p := NewKubeProvisioner(c, "", "").WithPodEnv(
			corev1.EnvVar{Name: "KSQUAD_SANDBOX_HTTPS_PROXY", Value: "http://ksquad-egress.ksquad-system.svc:8888"},
		)
		var lines []string
		if err := p.BootMeshPool(bootCapturingCtx(&lines), spec); err != nil {
			t.Fatalf("BootMeshPool: %v", err)
		}
		for _, l := range lines {
			if strings.Contains(l, "NO effective KSQUAD_SANDBOX_*_PROXY") {
				t.Fatalf("proxied mesh boot still warned: %q", l)
			}
		}
	})
}

// TestKubeProvisionerBootRunSilentWithProxy (ISI-7691): when the operator
// stamped a forward-proxy endpoint (WithPodEnv, the ISI-5477 path), that env
// reaches the Run cold-boot pod spec end-to-end AND the fail-loud guard stays
// quiet — proxied egress is the healthy case.
func TestKubeProvisionerBootRunSilentWithProxy(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(bootProxyGuardScheme(t)).Build()
	p := NewKubeProvisioner(c, "", "").WithPodEnv(
		corev1.EnvVar{Name: "KSQUAD_SANDBOX_HTTPS_PROXY", Value: "http://ksquad-egress.ksquad-system.svc:8888"},
		corev1.EnvVar{Name: "KSQUAD_SANDBOX_NO_PROXY", Value: ".svc,.cluster.local"},
	)

	var lines []string
	ctx := bootCapturingCtx(&lines)
	key := PoolKey{RuntimeClass: "gvisor", Namespace: "ksquad-team-squad", CapabilityHash: "abc", Image: "reg.example/ksquad-shim-codex:m1"}
	if err := p.Boot(ctx, key, "sbx-proxied", "run-uid-2", BootRun); err != nil {
		t.Fatalf("boot: %v", err)
	}

	for _, l := range lines {
		if strings.Contains(l, "NO effective KSQUAD_SANDBOX_*_PROXY") {
			t.Fatalf("proxied Run cold-boot still emitted the missing-proxy warning: %q", l)
		}
	}

	// End-to-end: the proxy env must actually reach the Run cold-boot pod spec
	// (pool.go BootRun -> kube.go env append), the gap ISI-7686 was chasing.
	pod := &corev1.Pod{}
	if err := c.Get(ctx, clientObjectKey(t, "ksquad-team-squad", "sbx-proxied"), pod); err != nil {
		t.Fatalf("get run pod: %v", err)
	}
	var gotProxy string
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "KSQUAD_SANDBOX_HTTPS_PROXY" {
			gotProxy = e.Value
		}
	}
	if gotProxy != "http://ksquad-egress.ksquad-system.svc:8888" {
		t.Fatalf("KSQUAD_SANDBOX_HTTPS_PROXY on run pod = %q, want the stamped egress proxy", gotProxy)
	}
}

// TestKubeProvisionerBootWarmNeverWarnsProxy (ISI-7691): a warm (idle) boot
// dials nothing and must not trip the Run-cold-boot proxy guard — the guard is
// BootRun-scoped so steady-state warm replenishment stays quiet.
func TestKubeProvisionerBootWarmNeverWarnsProxy(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(bootProxyGuardScheme(t)).Build()
	p := NewKubeProvisioner(c, "", "")

	var lines []string
	ctx := bootCapturingCtx(&lines)
	key := PoolKey{RuntimeClass: "gvisor", Namespace: "ksquad-team-squad", CapabilityHash: "abc", Image: "reg.example/ksquad-shim-codex:m1"}
	if err := p.Boot(ctx, key, "sbx-warm", "", BootWarm); err != nil {
		t.Fatalf("boot: %v", err)
	}
	for _, l := range lines {
		if strings.Contains(l, "NO effective KSQUAD_SANDBOX_*_PROXY") {
			t.Fatalf("warm boot tripped the Run-cold-boot proxy guard: %q", l)
		}
	}
}
