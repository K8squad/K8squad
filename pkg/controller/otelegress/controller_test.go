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

package otelegress

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
)

const ns = "ksquad-system"

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgo scheme: %v", err)
	}
	if err := ksquadv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("ksquad scheme: %v", err)
	}
	return s
}

func collectorDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      "rel-otel-collector",
			Labels:    map[string]string{componentLabelKey: collectorComponent},
		},
	}
}

func otelConfig(name string, gen int64) *ksquadv1alpha1.OTelConfig {
	return &ksquadv1alpha1.OTelConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: gen},
		Spec: ksquadv1alpha1.OTelConfigSpec{
			Traces: &ksquadv1alpha1.SignalRouting{
				Endpoint: "https://otlp.dynatrace.com/api/v2/otlp/v1/traces",
				Protocol: ksquadv1alpha1.ExportProtocolHTTPProtobuf,
				Auth:     &ksquadv1alpha1.SecretKeyReference{Name: "otlp-token", Key: "token"},
			},
		},
	}
}

func newReconciler(t *testing.T, objs ...client.Object) *Reconciler {
	t.Helper()
	cl := fake.NewClientBuilder().
		WithScheme(scheme(t)).
		WithStatusSubresource(&ksquadv1alpha1.OTelConfig{}).
		WithObjects(objs...).
		Build()
	return &Reconciler{Client: cl, Namespace: ns}
}

func reconcile(t *testing.T, r *Reconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func getEgress(t *testing.T, r *Reconciler) *corev1.ConfigMap {
	t.Helper()
	var cm corev1.ConfigMap
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "rel-otel-collector-egress"}, &cm); err != nil {
		t.Fatalf("get egress configmap: %v", err)
	}
	return &cm
}

func getCollector(t *testing.T, r *Reconciler) *appsv1.Deployment {
	t.Helper()
	var dep appsv1.Deployment
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "rel-otel-collector"}, &dep); err != nil {
		t.Fatalf("get collector: %v", err)
	}
	return &dep
}

// The 13.8 AC: an applied OTelConfig writes the egress overlay and rolls the
// collector, with auth as env-ref only (never a token value).
func TestReconcileWritesOverlayAndRolls(t *testing.T) {
	r := newReconciler(t, collectorDeployment(), otelConfig("default", 3))
	reconcile(t, r)

	cm := getEgress(t, r)
	overlay := cm.Data[egressConfigKey]
	if !strings.Contains(overlay, "otlphttp/vendor_traces") {
		t.Errorf("overlay missing vendor exporter:\n%s", overlay)
	}
	if strings.Contains(overlay, "dt0c01") {
		t.Errorf("overlay leaked a token value:\n%s", overlay)
	}
	if !strings.Contains(overlay, "${env:KSQUAD_OTLP_AUTH}") {
		t.Errorf("overlay missing auth env ref:\n%s", overlay)
	}
	if cm.Labels[egressSourceLabelKey] != egressSourceOperator {
		t.Errorf("egress-source label = %q, want operator", cm.Labels[egressSourceLabelKey])
	}

	dep := getCollector(t, r)
	if got := dep.Spec.Template.Annotations[rolloutAnnotationKey]; got != "3" {
		t.Errorf("rollout annotation = %q, want 3", got)
	}
}

// A second reconcile of an unchanged config must not re-roll (idempotent) — the
// annotation stays at the same generation and no spurious restart is triggered.
func TestReconcileIdempotent(t *testing.T) {
	r := newReconciler(t, collectorDeployment(), otelConfig("default", 3))
	reconcile(t, r)
	first := getCollector(t, r).Spec.Template.Annotations[rolloutAnnotationKey]
	reconcile(t, r)
	second := getCollector(t, r).Spec.Template.Annotations[rolloutAnnotationKey]
	if first != "3" || second != "3" {
		t.Errorf("annotation drifted across idempotent reconciles: %q → %q", first, second)
	}
}

// Taking ownership of a Helm-bootstrap egress ConfigMap: the reconciler
// overwrites the bootstrap content and flips the source label to operator.
func TestReconcileTakesOverBootstrapConfigMap(t *testing.T) {
	bootstrap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      "rel-otel-collector-egress",
			Labels:    map[string]string{egressSourceLabelKey: "helm-bootstrap"},
		},
		Data: map[string]string{egressConfigKey: "exporters: {}\n"},
	}
	r := newReconciler(t, collectorDeployment(), bootstrap, otelConfig("default", 1))
	reconcile(t, r)
	cm := getEgress(t, r)
	if cm.Labels[egressSourceLabelKey] != egressSourceOperator {
		t.Errorf("did not take ownership: label = %q", cm.Labels[egressSourceLabelKey])
	}
	if !strings.Contains(cm.Data[egressConfigKey], "otlphttp/vendor_traces") {
		t.Errorf("bootstrap content not replaced:\n%s", cm.Data[egressConfigKey])
	}
}

// No OTelConfig: the reconciler leaves the egress ConfigMap untouched so the
// bootstrap fallback survives (opt-in preserved).
func TestReconcileNoConfigLeavesEgress(t *testing.T) {
	bootstrap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      "rel-otel-collector-egress",
			Labels:    map[string]string{egressSourceLabelKey: "helm-bootstrap"},
		},
		Data: map[string]string{egressConfigKey: "exporters: {}\n"},
	}
	r := newReconciler(t, collectorDeployment(), bootstrap)
	reconcile(t, r)
	cm := getEgress(t, r)
	if cm.Labels[egressSourceLabelKey] != "helm-bootstrap" {
		t.Errorf("bootstrap overlay was mutated with no OTelConfig present")
	}
}

// Collector not up yet: reconcile surfaces an error (to requeue) and does not
// create an orphan egress ConfigMap against an unknown collector name.
func TestReconcileCollectorNotFound(t *testing.T) {
	r := newReconciler(t, otelConfig("default", 1))
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}); err == nil {
		t.Fatal("expected error when collector Deployment is absent")
	}
}

func TestSelectConfig(t *testing.T) {
	if SelectConfig(nil) != nil {
		t.Error("empty set must select nil")
	}
	items := []ksquadv1alpha1.OTelConfig{
		{ObjectMeta: metav1.ObjectMeta{Name: "zeta"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "alpha"}},
	}
	if got := SelectConfig(items); got.Name != "default" {
		t.Errorf("selected %q, want default", got.Name)
	}
	if got := SelectConfig(items[:1]); got.Name != "zeta" {
		t.Errorf("single-item select %q, want zeta", got.Name)
	}
	noDefault := []ksquadv1alpha1.OTelConfig{
		{ObjectMeta: metav1.ObjectMeta{Name: "zeta"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "alpha"}},
	}
	if got := SelectConfig(noDefault); got.Name != "alpha" {
		t.Errorf("no-default select %q, want alpha (lexically first)", got.Name)
	}
}
