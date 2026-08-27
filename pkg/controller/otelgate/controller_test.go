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

package otelgate

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/telemetry/toolusage"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := ksquadv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return s
}

func otelcfg(name string, enabled *bool) *ksquadv1alpha1.OTelConfig {
	return &ksquadv1alpha1.OTelConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: ksquadv1alpha1.OTelConfigSpec{
			Traces: &ksquadv1alpha1.SignalRouting{
				Endpoint: "http://collector.ksquad-system.svc:4318/v1/traces",
				Protocol: ksquadv1alpha1.ExportProtocolHTTPProtobuf,
			},
			ToolUsage: &ksquadv1alpha1.ToolUsageConfig{Enabled: enabled},
		},
	}
}

func reconcile(t *testing.T, r *Reconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
}

func ptr(b bool) *bool { return &b }

// The D2 AC: spec.toolUsage.enabled=false flips the process-wide gate off;
// absent or true leaves it on; deleting the config restores default-on.
func TestToggleFlipsGate(t *testing.T) {
	toolusage.SetEnabled(true)
	t.Cleanup(func() { toolusage.SetEnabled(true) })

	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithStatusSubresource(&ksquadv1alpha1.OTelConfig{}).Build()
	r := &Reconciler{Client: cl}

	disabled := otelcfg("platform", ptr(false))
	if err := cl.Create(context.Background(), disabled); err != nil {
		t.Fatalf("create: %v", err)
	}
	reconcile(t, r, "platform")
	if toolusage.Enabled() {
		t.Fatalf("toolUsage.enabled=false must disable the gate")
	}

	// Flip to true: gate reopens.
	var cur ksquadv1alpha1.OTelConfig
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "platform"}, &cur); err != nil {
		t.Fatalf("get: %v", err)
	}
	cur.Spec.ToolUsage.Enabled = ptr(true)
	if err := cl.Update(context.Background(), &cur); err != nil {
		t.Fatalf("update: %v", err)
	}
	reconcile(t, r, "platform")
	if !toolusage.Enabled() {
		t.Fatalf("toolUsage.enabled=true must enable the gate")
	}

	// Absent field: default-on posture (opt-out, plan §5.4).
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "platform"}, &cur); err != nil {
		t.Fatalf("get: %v", err)
	}
	cur.Spec.ToolUsage = nil
	if err := cl.Update(context.Background(), &cur); err != nil {
		t.Fatalf("update: %v", err)
	}
	reconcile(t, r, "platform")
	if !toolusage.Enabled() {
		t.Fatalf("absent toolUsage must default to enabled")
	}

	// Status carries the applied state.
	var got ksquadv1alpha1.OTelConfig
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "platform"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("ObservedGeneration = %d, want %d", got.Status.ObservedGeneration, got.Generation)
	}
	if c := metaFind(got.Status.Conditions, "Ready"); c == nil {
		t.Errorf("Ready condition missing: %+v", got.Status.Conditions)
	}

	// Deletion: no configs remain → default-on.
	if err := cl.Delete(context.Background(), &cur); err != nil {
		t.Fatalf("delete: %v", err)
	}
	toolusage.SetEnabled(false) // prove the reconcile, not inertia, reopened it
	reconcile(t, r, "platform")
	if !toolusage.Enabled() {
		t.Fatalf("no OTelConfig remaining must restore default-on")
	}
}

// Two configs: the disabling one wins while it exists (conjunction), and the
// gate reopens once it is deleted even though the other config was never
// reconciled again.
func TestConjunctionOfConfigs(t *testing.T) {
	toolusage.SetEnabled(true)
	t.Cleanup(func() { toolusage.SetEnabled(true) })

	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithStatusSubresource(&ksquadv1alpha1.OTelConfig{}).Build()
	r := &Reconciler{Client: cl}

	onCfg := otelcfg("a", nil)
	offCfg := otelcfg("b", ptr(false))
	for _, o := range []*ksquadv1alpha1.OTelConfig{onCfg, offCfg} {
		if err := cl.Create(context.Background(), o); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	reconcile(t, r, "b")
	if toolusage.Enabled() {
		t.Fatalf("one disabling config must close the gate")
	}

	if err := cl.Delete(context.Background(), offCfg); err != nil {
		t.Fatalf("delete: %v", err)
	}
	reconcile(t, r, "b")
	if !toolusage.Enabled() {
		t.Fatalf("gate must reopen when the disabling config is gone")
	}
}

func metaFind(conds []metav1.Condition, t string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == t {
			return &conds[i]
		}
	}
	return nil
}
