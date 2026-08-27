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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
