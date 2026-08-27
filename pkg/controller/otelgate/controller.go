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

// Package otelgate is the Epic D story-D2 wiring between the OTelConfig CRD's
// tool-usage pipeline toggle and the process-wide instrumentation gate
// (pkg/telemetry/toolusage). One leader-elected reconciler watches every
// OTelConfig and applies spec.toolUsage.enabled (absent = enabled, plan §5.4)
// to toolusage.SetEnabled — so flipping the field stops and resumes
// gen_ai.tool.call / skill.load / mcp.call spans and the ksquad_* tool metrics
// in the operator, mid-process, with no restart.
//
// Semantics with multiple OTelConfig objects (the CRD is cluster-scoped and
// singular in practice, but the type does not enforce it): the gate is the
// conjunction — every applied config must leave it on. A config that disables
// wins until it is deleted or re-enabled, at which point the surviving
// configs' posture is re-derived on the next reconcile of each. Deletion of
// the last config returns the platform to the default: enabled.
package otelgate

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/telemetry/toolusage"
)

// Reconciler applies OTelConfig.spec.toolUsage onto the tool-usage gate and
// reports readiness on the CRD's status.
type Reconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=ksquad.io,resources=otelconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=ksquad.io,resources=otelconfigs/status,verbs=get;update;patch

// Reconcile applies one OTelConfig's tool-usage toggle. NotFound (the config
// was deleted) re-derives the gate from every remaining config — the default
// posture when none remain is enabled.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var cfg ksquadv1alpha1.OTelConfig
	err := r.Get(ctx, req.NamespacedName, &cfg)
	if apierrors.IsNotFound(err) {
		if derr := r.derive(ctx); derr != nil {
			return ctrl.Result{}, derr
		}
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("otelgate: get otelconfig %s: %w", req.Name, err)
	}

	want, err := deriveGate(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	logger.Info("otelgate: applied tool-usage pipeline toggle", "otelconfig", req.Name, "enabled", want)

	next := cfg.DeepCopy()
	next.Status.ObservedGeneration = cfg.Generation
	meta.SetStatusCondition(&next.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "ToolUsageGateApplied",
		Message:            fmt.Sprintf("tool-usage pipeline %s (spans + ksquad_* metrics)", enabledWord(want)),
		ObservedGeneration: cfg.Generation,
	})
	if uerr := r.Status().Update(ctx, next); uerr != nil {
		return ctrl.Result{}, fmt.Errorf("otelgate: update otelconfig %s status: %w", req.Name, uerr)
	}
	return ctrl.Result{}, nil
}

// deriveGate lists every OTelConfig and applies the conjunction of their
// tool-usage toggles, returning the derived value. With no configs the gate
// opens (default-on, plan §5.4).
func deriveGate(ctx context.Context, c client.Client) (bool, error) {
	var list ksquadv1alpha1.OTelConfigList
	if err := c.List(ctx, &list); err != nil {
		return false, fmt.Errorf("otelgate: list otelconfigs: %w", err)
	}
	on := true
	for _, cfg := range list.Items {
		if !cfg.Spec.ToolUsage.EnabledOrDefault() {
			on = false
			break
		}
	}
	toolusage.SetEnabled(on)
	return on, nil
}

func (r *Reconciler) derive(ctx context.Context) error {
	on, err := deriveGate(ctx, r.Client)
	if err != nil {
		return err
	}
	log.FromContext(ctx).Info("otelgate: re-derived tool-usage gate", "enabled", on)
	return nil
}

func enabledWord(on bool) string {
	if on {
		return "enabled"
	}
	return "disabled"
}

// SetupWithManager registers the otelgate controller on the manager
// (leader-elected like every platform controller).
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("otelgate").
		For(&ksquadv1alpha1.OTelConfig{}).
		Complete(r)
}
