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
	key := PoolKey{RuntimeClass: "gvisor", Namespace: "bmad-squad", CapabilityHash: "abc"}
	if err := p.Boot(ctx, key, "sbx-1"); err != nil {
		t.Fatalf("boot: %v", err)
	}
	pod := &corev1.Pod{}
	if err := c.Get(ctx, clientObjectKey(t, "bmad-squad", "sbx-1"), pod); err != nil {
		t.Fatalf("get pod in team namespace: %v", err)
	}
	if got := pod.Annotations["k8squad.io/pool-key"]; got != "gvisor/" {
		t.Fatalf("pool-key annotation = %q", got)
	}

	// Legacy key without a namespace: the provisioner default remains.
	legacy := PoolKey{RuntimeClass: "gvisor"}
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

	key := PoolKey{RuntimeClass: "gvisor", Namespace: "bmad-squad"}
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
	if err := p.Boot(ctx, PoolKey{RuntimeClass: "gvisor"}, "sbx-coord"); err != nil {
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
