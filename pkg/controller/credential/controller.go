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

package credential

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	// DefaultRefreshLead is how long BEFORE expiry the controller refreshes the
	// ~8h access token. A generous lead (arch §21 "refresh lead-time is
	// controller tuning") means a transient provider blip has many retries in
	// hand before the mounted token would actually go stale — the refresh never
	// races the expiry.
	DefaultRefreshLead = 30 * time.Minute

	// DefaultErrorRequeue is the backoff after a TRANSIENT refresh failure
	// (network/5xx). Short enough to recover well within the refresh lead,
	// long enough not to hammer a struggling endpoint.
	DefaultErrorRequeue = 1 * time.Minute

	// minRequeue floors the computed requeue so a token that is already inside
	// the lead window (or has a clock-skewed near-future expiry) cannot spin
	// the reconciler in a hot loop.
	minRequeue = 30 * time.Second
)

// Reconciler is the Story 7.7 leader-elected credential controller. It watches
// per-user human-seat OAuth Secrets and keeps their access token fresh in
// place, so concurrent agent pods that mount the Secret share one login with no
// per-pod refresh and no manual re-token (arch §5.2, §11.1, ADR-032).
type Reconciler struct {
	client.Client

	// Refresher performs the provider OAuth refresh. Required.
	Refresher TokenRefresher

	// RefreshLead overrides DefaultRefreshLead when non-zero (test / tuning).
	RefreshLead time.Duration

	// Now is the clock seam; nil defaults to time.Now (tests inject a fixed
	// clock so the refresh-schedule maths is deterministic).
	Now func() time.Time
}

// now returns the reconciler's clock (test-injectable).
func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// lead returns the configured refresh lead or the default.
func (r *Reconciler) lead() time.Duration {
	if r.RefreshLead > 0 {
		return r.RefreshLead
	}
	return DefaultRefreshLead
}

// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;update;patch

// Reconcile keeps one human-seat OAuth Secret's access token fresh. The Watch
// predicate guarantees req names a Secret labelled human-seat (ADR-041
// enforcement by construction), so this body never touches a service-account
// credential.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var secret corev1.Secret
	if err := r.Get(ctx, req.NamespacedName, &secret); err != nil {
		// Deleted between event and reconcile: nothing to keep fresh.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Belt-and-braces enforcement: even though the predicate filters to
	// human-seat, re-assert it here so a hand-crafted reconcile request (or a
	// predicate regression) can never drive a refresh against the wrong class.
	if secret.Labels[LabelCredentialClass] != ClassHumanSeat {
		return ctrl.Result{}, nil
	}

	cs, err := parseCredential(&secret)
	if err != nil {
		// Misconfigured human-seat Secret (no refresh material / bad expiry):
		// legible StateError, fixed by correcting the Secret. No requeue —
		// re-writing the Secret triggers a fresh event.
		logger.Info("human-seat credential is not refreshable", "secret", req.NamespacedName, "reason", err.Error())
		return r.mark(ctx, &secret, StateError, err.Error(), time.Time{})
	}

	now := r.now()
	refreshAt := cs.expiresAt.Add(-r.lead())

	// Not yet in the refresh window: healthy, requeue at the refresh instant.
	if now.Before(refreshAt) {
		if res, err := r.mark(ctx, &secret, StateConnected, "", cs.expiresAt); err != nil {
			return res, err
		}
		return ctrl.Result{RequeueAfter: requeueAfter(now, refreshAt)}, nil
	}

	// Inside the lead window (or already past expiry): refresh now.
	logger.Info("refreshing human-seat access token", "secret", req.NamespacedName, "expiresAt", cs.expiresAt)
	if _, err := r.mark(ctx, &secret, StateRefreshing, "refreshing access token", cs.expiresAt); err != nil {
		return ctrl.Result{}, err
	}

	refreshed, err := r.Refresher.Refresh(ctx, cs.refreshToken)
	if err != nil {
		if errors.Is(err, ErrRefreshExpired) {
			// Terminal: the refresh window lapsed. Pause path (7.4) + one-click
			// re-login (8.6) take over. No auto-requeue — only a human re-login
			// (Secret update) revives it.
			logger.Info("refresh token rejected; credential expired", "secret", req.NamespacedName)
			return r.mark(ctx, &secret, StateExpired, "refresh token rejected — click to re-login", cs.expiresAt)
		}
		// Transient: keep the still-valid token, retry within the lead window.
		logger.Error(err, "transient refresh failure; will retry", "secret", req.NamespacedName)
		if _, merr := r.mark(ctx, &secret, StateConnected, "refresh retry pending: "+err.Error(), cs.expiresAt); merr != nil {
			return ctrl.Result{}, merr
		}
		return ctrl.Result{RequeueAfter: DefaultErrorRequeue}, nil
	}

	// Success: write the new material back to the SAME Secret in place.
	if err := r.writeBack(ctx, &secret, refreshed, now); err != nil {
		return ctrl.Result{}, fmt.Errorf("write refreshed credential back to Secret %q: %w", req.NamespacedName, err)
	}
	logger.Info("refreshed human-seat access token", "secret", req.NamespacedName, "newExpiresAt", refreshed.ExpiresAt)

	nextRefresh := refreshed.ExpiresAt.Add(-r.lead())
	return ctrl.Result{RequeueAfter: requeueAfter(now, nextRefresh)}, nil
}

// writeBack persists a successful refresh into the SAME Secret: new access
// token, rotated refresh token, and new expiry, plus the connected/last-refresh
// health annotations. It is a single Update so the mounted-Secret update the
// agent pods observe is atomic — pods never see a half-written credential.
func (r *Reconciler) writeBack(ctx context.Context, secret *corev1.Secret, refreshed RefreshedToken, now time.Time) error {
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[KeyAccessToken] = []byte(refreshed.AccessToken)
	secret.Data[KeyRefreshToken] = []byte(refreshed.RefreshToken)
	secret.Data[KeyExpiresAt] = []byte(refreshed.ExpiresAt.UTC().Format(time.RFC3339))
	setAnnotations(secret, StateConnected, "", refreshed.ExpiresAt, now)
	return r.Update(ctx, secret)
}

// mark writes only the health annotations (state/message/expiry) without
// touching token data. Returns a no-requeue Result so terminal states
// (error/expired) can `return r.mark(...)` directly.
func (r *Reconciler) mark(ctx context.Context, secret *corev1.Secret, state, message string, expiresAt time.Time) (ctrl.Result, error) {
	if secret.Annotations[AnnotationState] == state &&
		secret.Annotations[AnnotationMessage] == message {
		// No health change — avoid a needless write (and the resourceVersion
		// churn a self-triggered event would cause).
		return ctrl.Result{}, nil
	}
	setAnnotations(secret, state, message, expiresAt, time.Time{})
	if err := r.Update(ctx, secret); err != nil {
		return ctrl.Result{}, fmt.Errorf("update credential health annotations: %w", err)
	}
	return ctrl.Result{}, nil
}

// setAnnotations stamps the health surface. A zero lastRefresh preserves any
// existing AnnotationLastRefresh (a plain health update must not erase the
// canary heartbeat); a non-zero lastRefresh (a successful refresh) sets it.
func setAnnotations(secret *corev1.Secret, state, message string, expiresAt, lastRefresh time.Time) {
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[AnnotationState] = state
	if message == "" {
		delete(secret.Annotations, AnnotationMessage)
	} else {
		secret.Annotations[AnnotationMessage] = message
	}
	if !expiresAt.IsZero() {
		secret.Annotations[AnnotationExpiresAt] = expiresAt.UTC().Format(time.RFC3339)
	}
	if !lastRefresh.IsZero() {
		secret.Annotations[AnnotationLastRefresh] = lastRefresh.UTC().Format(time.RFC3339)
	}
}

// requeueAfter computes a floored delay from now to target.
func requeueAfter(now, target time.Time) time.Duration {
	d := target.Sub(now)
	if d < minRequeue {
		return minRequeue
	}
	return d
}

// SetupWithManager registers the controller on the manager with a label
// predicate that selects ONLY human-seat OAuth Secrets. Registering on the
// manager (whose LeaderElection is enabled) is what makes this the single,
// leader-elected refresher — controller-runtime runs the controller only on the
// elected leader (arch §5.2: one owner, no thundering-refresh race).
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Refresher == nil {
		return fmt.Errorf("credential controller requires a non-nil Refresher")
	}
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	sel, err := humanSeatSelector()
	if err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("credential").
		For(&corev1.Secret{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			return sel.Matches(labels.Set(o.GetLabels()))
		}))).
		Complete(r)
}

// humanSeatSelector builds the `credential-class == human-seat` label selector
// the Watch predicate applies — the ADR-041 enforcement boundary.
func humanSeatSelector() (labels.Selector, error) {
	req, err := labels.NewRequirement(LabelCredentialClass, selection.Equals, []string{ClassHumanSeat})
	if err != nil {
		return nil, fmt.Errorf("build human-seat label selector: %w", err)
	}
	return labels.NewSelector().Add(*req), nil
}
