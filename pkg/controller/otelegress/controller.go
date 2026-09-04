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

// Package otelegress is story 13.8 (ISI-3724): the operator half of the
// OTelConfig → collector vendor-egress reconcile, per the Architect-ratified
// ADR-0008 (M1 + config-ownership (b), layered multi-`--config` merge).
//
// The 13.7 collector gateway loads two `--config` sources that confmap
// deep-merges. Helm owns the base (receivers + every processor INCLUDING the
// redaction pipeline + the safe stdout/debug default exporters). This
// reconciler owns only the second source: a small `<collector>-egress`
// ConfigMap holding the vendor `exporters:` block and the per-signal
// `service.pipelines.*.exporters` override, rendered from the applied
// OTelConfig CR by telemetry.RenderEgressOverlay. Because confmap replaces
// sequences, the overlay's `exporters:` list wins over the base's `[debug]`
// default while redaction stays a base-pipeline processor upstream — an
// external endpoint never bypasses PII/secret stripping (obs-plan §6 / §13.8).
//
// The collector has no hot-reload, so after writing the overlay the reconciler
// rolls the collector by stamping the `ksquad.io/otelconfig-generation`
// annotation on its pod template. With no OTelConfig CR the reconciler leaves
// the egress ConfigMap untouched — the deprecated Helm bootstrap overlay
// (ADR-0008 §Migration) stays the fallback until it is removed, and opt-in is
// preserved. Auth is only ever the `${env:KSQUAD_OTLP_AUTH}` indirection the
// base Deployment injects from the Secret; no token value is written anywhere.
package otelegress

import (
	"context"
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/telemetry"
)

const (
	componentLabelKey     = "app.kubernetes.io/component"
	collectorComponent    = "otel-collector"
	egressConfigMapSuffix = "-egress"
	egressConfigKey       = "egress.yaml"
	egressSourceLabelKey  = "ksquad.io/egress-source"
	egressSourceOperator  = "operator"
	rolloutAnnotationKey  = "ksquad.io/otelconfig-generation"
)

// Reconciler applies an OTelConfig's per-signal routing onto the collector's
// vendor egress overlay ConfigMap and rolls the collector when it changes.
type Reconciler struct {
	client.Client
	// Namespace is where the collector runs (the operator's own release
	// namespace, wired from POD_NAMESPACE). Empty means "search all namespaces"
	// when locating the collector by label.
	Namespace string
}

// +kubebuilder:rbac:groups=ksquad.io,resources=otelconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=ksquad.io,resources=otelconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;patch

// Reconcile renders the egress overlay from the selected OTelConfig, writes it
// to the collector's `<collector>-egress` ConfigMap, and rolls the collector if
// the overlay changed. A reconcile of any OTelConfig re-derives from the full
// set (the selected config is the one named "default", else the lexically
// first) so add/delete/edit of any config converges to one overlay.
func (r *Reconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var list ksquadv1alpha1.OTelConfigList
	if err := r.List(ctx, &list); err != nil {
		return ctrl.Result{}, fmt.Errorf("otelegress: list otelconfigs: %w", err)
	}
	sel := SelectConfig(list.Items)
	if sel == nil {
		// No CR: leave the (possibly Helm-bootstrap) egress overlay in place so
		// the deprecation-window fallback and opt-in default are preserved.
		logger.Info("otelegress: no OTelConfig; leaving collector egress untouched")
		return ctrl.Result{}, nil
	}

	overlay, err := telemetry.RenderEgressOverlay(sel)
	if err != nil {
		// A bad/invalid routing spec is a real export-config failure: erroring.
		return ctrl.Result{}, r.reportNotReady(ctx, sel, "RenderFailed", err, ksquadv1alpha1.SignalStateErroring)
	}

	collector, err := r.findCollector(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if collector == nil {
		// Collector not up yet; requeue by returning an error is noisy — report
		// and let the next OTelConfig event (or the collector appearing) retry.
		logger.Info("otelegress: collector Deployment not found yet; will retry on next event")
		// Collector still coming up during bootstrap — transient, not an export
		// failure: pending, so the Console card doesn't false-alarm as erroring.
		return ctrl.Result{}, r.reportNotReady(ctx, sel, "CollectorNotFound",
			fmt.Errorf("collector Deployment (label %s=%s) not found in %q", componentLabelKey, collectorComponent, r.Namespace),
			ksquadv1alpha1.SignalStatePending)
	}

	changed, err := r.applyOverlay(ctx, collector.Namespace, collector.Name+egressConfigMapSuffix, overlay)
	if err != nil {
		return ctrl.Result{}, err
	}
	if changed {
		if err := r.roll(ctx, collector, sel.Generation); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("otelegress: applied egress overlay and rolled collector",
			"otelconfig", sel.Name, "collector", collector.Name, "generation", sel.Generation)
	}

	return ctrl.Result{}, r.reportReady(ctx, sel, changed)
}

// SelectConfig picks the authoritative OTelConfig from the cluster set: the one
// named "default" if present, else the lexically-first by name. Returns nil for
// an empty set. Deterministic, mirroring the apiserver read model (Story A-AC6).
func SelectConfig(items []ksquadv1alpha1.OTelConfig) *ksquadv1alpha1.OTelConfig {
	if len(items) == 0 {
		return nil
	}
	sorted := make([]ksquadv1alpha1.OTelConfig, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for i := range sorted {
		if sorted[i].Name == "default" {
			return &sorted[i]
		}
	}
	return &sorted[0]
}

// findCollector locates the collector Deployment by its component label. Returns
// (nil, nil) when none exists yet (first boot before the chart's collector is
// ready), and an error only on a genuine API failure.
func (r *Reconciler) findCollector(ctx context.Context) (*appsv1.Deployment, error) {
	var deps appsv1.DeploymentList
	opts := []client.ListOption{client.MatchingLabels{componentLabelKey: collectorComponent}}
	if r.Namespace != "" {
		opts = append(opts, client.InNamespace(r.Namespace))
	}
	if err := r.List(ctx, &deps, opts...); err != nil {
		return nil, fmt.Errorf("otelegress: list collector deployments: %w", err)
	}
	if len(deps.Items) == 0 {
		return nil, nil
	}
	// Deterministic if more than one matches (should be singular).
	sort.Slice(deps.Items, func(i, j int) bool { return deps.Items[i].Name < deps.Items[j].Name })
	return &deps.Items[0], nil
}

// applyOverlay upserts the egress ConfigMap and reports whether its content
// changed (so the caller only rolls the collector on a real change).
func (r *Reconciler) applyOverlay(ctx context.Context, ns, name, overlay string) (bool, error) {
	var cm corev1.ConfigMap
	err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &cm)
	switch {
	case apierrors.IsNotFound(err):
		cm = corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns,
				Name:      name,
				Labels: map[string]string{
					componentLabelKey:    collectorComponent,
					egressSourceLabelKey: egressSourceOperator,
				},
			},
			Data: map[string]string{egressConfigKey: overlay},
		}
		if err := r.Create(ctx, &cm); err != nil {
			return false, fmt.Errorf("otelegress: create egress configmap %s/%s: %w", ns, name, err)
		}
		return true, nil
	case err != nil:
		return false, fmt.Errorf("otelegress: get egress configmap %s/%s: %w", ns, name, err)
	}

	if cm.Data[egressConfigKey] == overlay && cm.Labels[egressSourceLabelKey] == egressSourceOperator {
		return false, nil
	}
	next := cm.DeepCopy()
	if next.Data == nil {
		next.Data = map[string]string{}
	}
	next.Data[egressConfigKey] = overlay
	if next.Labels == nil {
		next.Labels = map[string]string{}
	}
	// Take ownership from the Helm bootstrap overlay (ADR-0008 §Migration).
	next.Labels[egressSourceLabelKey] = egressSourceOperator
	if err := r.Update(ctx, next); err != nil {
		return false, fmt.Errorf("otelegress: update egress configmap %s/%s: %w", ns, name, err)
	}
	return true, nil
}

// roll stamps the rollout annotation on the collector pod template so a
// no-hot-reload collector re-reads both `--config` files (ADR-0008 §Mechanism.5).
func (r *Reconciler) roll(ctx context.Context, collector *appsv1.Deployment, generation int64) error {
	next := collector.DeepCopy()
	if next.Spec.Template.Annotations == nil {
		next.Spec.Template.Annotations = map[string]string{}
	}
	next.Spec.Template.Annotations[rolloutAnnotationKey] = fmt.Sprintf("%d", generation)
	if err := r.Patch(ctx, next, client.MergeFrom(collector)); err != nil {
		return fmt.Errorf("otelegress: roll collector %s/%s: %w", collector.Namespace, collector.Name, err)
	}
	return nil
}

func (r *Reconciler) reportReady(ctx context.Context, cfg *ksquadv1alpha1.OTelConfig, changed bool) error {
	msg := "collector egress overlay in sync with OTelConfig"
	if changed {
		msg = "collector egress overlay applied from OTelConfig; collector rolled"
	}
	return r.setCondition(ctx, cfg, metav1.ConditionTrue, "EgressApplied", msg, ksquadv1alpha1.SignalStateHealthy)
}

func (r *Reconciler) reportNotReady(ctx context.Context, cfg *ksquadv1alpha1.OTelConfig, reason string, cause error, signalState ksquadv1alpha1.SignalState) error {
	// Report the readiness regression but surface the original cause to requeue.
	_ = r.setCondition(ctx, cfg, metav1.ConditionFalse, reason, cause.Error(), signalState)
	return cause
}

// setCondition updates the OTelConfig's EgressReady condition and, per D-AC2
// (ISI-3621), the per-signal export health on status.signals via SetSignal. A
// status-update failure is logged, not fatal — the overlay is already applied
// by then.
func (r *Reconciler) setCondition(ctx context.Context, cfg *ksquadv1alpha1.OTelConfig, status metav1.ConditionStatus, reason, msg string, configuredState ksquadv1alpha1.SignalState) error {
	next := cfg.DeepCopy()
	next.Status.ObservedGeneration = cfg.Generation
	meta.SetStatusCondition(&next.Status.Conditions, metav1.Condition{
		Type:               "EgressReady",
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: cfg.Generation,
	})
	applySignals(&next.Status, next.Spec, configuredState, msg)
	if err := r.Status().Update(ctx, next); err != nil {
		log.FromContext(ctx).Error(err, "otelegress: update otelconfig status", "otelconfig", cfg.Name)
		return nil
	}
	return nil
}

// applySignals populates status.signals for the three OTLP signals (D-AC2 /
// ISI-3621). A signal with no routing in the spec is `disabled`; a configured
// signal takes configuredState — `healthy` when the egress overlay reconciled
// cleanly, `erroring` (with a secret-free reason) on a config/render failure,
// or `pending` while the collector has not come up yet. This is reconcile-
// derived health (ADR W6, reconcile-scoped): `healthy` means the routing is
// applied to the collector, not that the vendor acknowledged datapoints.
// detail is only attached to non-healthy states and never carries a token value
// — it is the EgressReady condition message, built from spec fields and Secret
// reference *names* only (D-AC3).
func applySignals(st *ksquadv1alpha1.OTelConfigStatus, spec ksquadv1alpha1.OTelConfigSpec, configuredState ksquadv1alpha1.SignalState, detail string) {
	set := func(key string, routing *ksquadv1alpha1.SignalRouting) {
		if routing == nil {
			st.SetSignal(key, ksquadv1alpha1.SignalStateDisabled, "")
			return
		}
		d := ""
		if configuredState != ksquadv1alpha1.SignalStateHealthy {
			d = detail
		}
		st.SetSignal(key, configuredState, d)
	}
	set("traces", spec.Traces)
	set("metrics", spec.Metrics)
	set("logs", spec.Logs)
}

// SetupWithManager registers the otelegress controller. It reconciles on every
// OTelConfig change and also watches the egress ConfigMap so manual drift (or
// the Helm bootstrap overlay) is corrected back to the CRD-derived state.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("otelegress").
		For(&ksquadv1alpha1.OTelConfig{}).
		Complete(r)
}
