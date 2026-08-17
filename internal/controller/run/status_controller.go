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

// Package run hosts the Run reconcile controller — the production wiring that
// binds the crash-safe reconcile machine (pkg/reconcile) to the coordination
// Postgres and the Run CRD (arch §5.2/§6.4, Story 3.1 production wiring,
// ISI-2655).
//
// This file is the STATUS-PROJECTION slice: a read-only reconciler that
// projects the durable coord reconcile_step onto Run.status (phase +
// observedGeneration + a Synced condition) so the CRD observes the §6.4 source
// of truth. It deliberately does NOT drive the machine or fire Effects
// (warm-pool bind / A2A dispatch / artifact upsert) — that is the follow-up
// Effects slice, which depends on primitives not yet in the tree. Being
// read-only, it cannot regress a Run: worst case it requeues.
package run

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/reconcile"
)

// preClaimRequeue is the backoff for a Run whose coord claim row has not landed
// yet (the apiserver claim path is Story 3.2). The Run is legitimately Pending
// until then; we requeue rather than treating the absent row as an error.
const preClaimRequeue = 5 * time.Second

// StatusReconciler projects the durable coord reconcile_step onto Run.status. It
// is READ-ONLY against the coordination Postgres — a fresh ProdReconcileStore is
// built per pass (AC2: the durable step is the only recovery state) and only
// Step()/Err() are called; nothing advances the machine or fires side effects.
type StatusReconciler struct {
	client.Client
	// DB is the coordination Postgres handle (the coord.claim source of truth).
	DB *sql.DB
}

// Reconcile reads the Run's durable step and projects the mapped §8 phase onto
// Run.status. It is level-triggered and idempotent: every pass reconstructs the
// position from Postgres and patches status only when it actually changed.
func (r *StatusReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var run ksquadv1alpha1.Run
	if err := r.Get(ctx, req.NamespacedName, &run); err != nil {
		// Not-found is normal after a delete — nothing to project.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Bind a durable Store to this Run's claim row. runID is the Run uid (the
	// idempotency key the machine threads); principal is the owner ref.
	store, err := coord.NewProdReconcileStore(ctx, r.DB,
		run.Spec.WorkItemRef, string(run.UID), string(run.Spec.OwnedBy), "")
	if err != nil {
		// A malformed Run (missing workItemRef/ownedBy) cannot be projected;
		// surface it on status rather than hot-looping on a hard error.
		l.Error(err, "cannot bind durable store for Run", "run", req.NamespacedName)
		return r.project(ctx, &run, reconcile.PhasePending, "StoreBindFailed", err.Error())
	}

	step := store.Step()
	if serr := store.Err(); serr != nil {
		if errors.Is(serr, sql.ErrNoRows) {
			// No claim row yet: the Run is genuinely Pending until the claim
			// path lands its row. Reflect Pending and requeue.
			res, perr := r.project(ctx, &run, reconcile.PhasePending, "AwaitingClaim",
				"no coord claim row yet; Run is pending claim")
			if perr != nil {
				return res, perr
			}
			return ctrl.Result{RequeueAfter: preClaimRequeue}, nil
		}
		// Transient infrastructure error — requeue with backoff, never read the
		// empty step as terminal.
		return ctrl.Result{}, fmt.Errorf("reading durable step: %w", serr)
	}

	return r.project(ctx, &run, reconcile.PhaseOf(step), "DurableStepProjected",
		fmt.Sprintf("phase projected from durable reconcile_step %q", step))
}

// project patches Run.status to the desired phase + observedGeneration + a
// Synced condition, writing only when something changed (a no-op reconcile
// makes no API write).
func (r *StatusReconciler) project(ctx context.Context, run *ksquadv1alpha1.Run,
	phase reconcile.Phase, reason, msg string) (ctrl.Result, error) {

	desired := ksquadv1alpha1.RunPhase(string(phase))
	changed := false

	if run.Status.Phase != desired {
		run.Status.Phase = desired
		changed = true
	}
	if run.Status.ObservedGeneration != run.Generation {
		run.Status.ObservedGeneration = run.Generation
		changed = true
	}
	if apimeta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               "Synced",
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: run.Generation,
	}) {
		changed = true
	}

	if !changed {
		return ctrl.Result{}, nil
	}
	if err := r.Status().Update(ctx, run); err != nil {
		return ctrl.Result{}, fmt.Errorf("projecting Run status: %w", err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the status-projection controller. Leader election
// (enabled on the manager, §5.2 AC4) guarantees a single active projector across
// replicas, so two pods never race the same Run's status subresource.
func (r *StatusReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ksquadv1alpha1.Run{}).
		Named("run-status-projection").
		Complete(r)
}
