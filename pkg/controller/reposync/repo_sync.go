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

// Package reposync hosts the story-11.1 repo-sync reconciler: the
// LEVEL-TRIGGERED per-Project mirror loop behind the pkg/scm provider seam
// (arch §5.4, ADR-018).
//
// One loop, two triggers, no third path:
//
//   - a good-signature webhook (cmd/scm-webhook) bumps the
//     ksquad.io/scm-sync-trigger annotation, the Project watch fires, and
//     this reconciler runs — the webhook payload is never written anywhere;
//   - RequeueAfter = spec.repo.sync.pollIntervalSeconds (from values,
//     default 300s) re-runs the SAME reconcile on a timer, so a lost
//     webhook is never permanent drift (AC3).
//
// Every reconcile is identical: read the provider's CURRENT state through
// the SourceControlProvider seam, echo-suppress our own reflected writes,
// and idempotent-upsert the whole snapshot into the untrusted-external,
// provenanced scm mirror keyed by external id (AC1/AC2/AC6). Redelivery,
// re-poll and racing triggers all converge to the same bytes — the sync is
// convergent, not oscillating (OQ13).
package reposync

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ksquadapi "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/scm"
)

// TriggerAnnotation is stamped (server-side patch) by the verified webhook
// ingress to trigger an immediate reconcile. The annotation VALUE is a
// timestamp — a redelivered webhook bumps it again, which is fine: the
// reconcile is idempotent, so the extra wake is a no-op pass (AC2).
const TriggerAnnotation = "ksquad.io/scm-sync-trigger"

// ConditionSyncReady summarizes the repo-sync loop on Project.status.
const ConditionSyncReady = "SyncReady"

const (
	reasonSynced       = "Synced"
	reasonUnconfigured = "SyncNotConfigured"
	reasonNoCredential = "CredentialMissing"
	reasonProviderFail = "ProviderError"
	reasonMirrorFail   = "MirrorWriteError"
)

// DefaultPollIntervalSeconds is used when spec.repo.sync.pollIntervalSeconds
// is unset (0). The CRD defaulting stamps 300 at admission; this is the
// in-process fallback for objects created before that default existed.
const DefaultPollIntervalSeconds int32 = 300

// minPollIntervalSeconds clamps a misconfigured low interval: hammering the
// provider API faster than once a minute buys nothing (the poll is a
// fallback, not a realtime feed).
const minPollIntervalSeconds int32 = 60

// tokenSecretKey is the key inside the BYO provider Secret holding the
// mirror-read token. The value is read into the provider client and then
// dropped — it is never logged, never echoed, never placed in a Run env
// (AC5, NFR-SEC8; there is no code path from this package to Run pods).
const tokenSecretKey = "token"

// Reconciler is the repo-sync reconciler (story 11.1). It talks ONLY to
// the scm.SourceControlProvider seam and the scm.MirrorStore seam; the
// provider name → constructor mapping lives in the scm.ProviderRegistry
// (composition root), and the Postgres dependency lives behind MirrorStore.
type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// APIReader reads Secrets WITHOUT the manager's cache: a cached Secret
	// read would start a cluster-wide Secret informer, holding every Secret
	// in the cluster in operator memory and widening a compromise's blast
	// radius far beyond the per-Project BYO tokens this loop needs. One
	// uncached read per reconcile is nothing at a 300s poll. SetupWithManager
	// wires mgr.GetAPIReader(); when nil (unit tests) the embedded client is
	// used directly.
	APIReader client.Reader

	// Providers resolves the Project's provider behind the seam. Required.
	Providers *scm.ProviderRegistry

	// Store is the scm mirror the snapshot is upserted into. Required.
	Store scm.MirrorStore

	// BotActor is the echo-suppression identity (default scm.DefaultBotActor):
	// provider records authored by this actor are OUR reflected writes and are
	// dropped on the way in (AC6).
	BotActor string
}

// +kubebuilder:rbac:groups=ksquad.io,resources=projects,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=ksquad.io,resources=projects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

// Reconcile runs one level-triggered mirror pass for the requested Project
// and schedules the poll fallback. Missing Projects are not errors (deleted
// mid-queue); provider/store failures set SyncReady=False and return the
// error so controller-runtime requeues with backoff.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	project := &ksquadapi.Project{}
	if err := r.Get(ctx, req.NamespacedName, project); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	sync := project.Spec.Repo.Sync
	if sync == nil {
		// Repo-sync not configured: nothing to mirror, no poll to schedule.
		// A Project without sync is not an error state (§5.4).
		return ctrl.Result{}, nil
	}

	// Resolve the BYO credential per Project (AC5): the token comes from the
	// referenced Secret, is handed to the provider factory, and leaves no
	// other trace — no field on this struct, no log line, no Run path.
	creds, err := r.resolveCredentials(ctx, project)
	if err != nil {
		logger.Error(err, "repo-sync: BYO credential not resolvable", "project", req.NamespacedName)
		r.patchStatus(ctx, project, statusPatch{condition: syncReadyFalse(reasonNoCredential, err.Error())})
		// Error only: controller-runtime ignores RequeueAfter alongside a
		// non-nil error (it requeues with backoff instead).
		return ctrl.Result{}, err
	}

	provider, err := r.Providers.Provider(ctx, sync.Provider, creds)
	if err != nil {
		logger.Error(err, "repo-sync: provider not resolvable", "project", req.NamespacedName)
		r.patchStatus(ctx, project, statusPatch{condition: syncReadyFalse(reasonProviderFail, err.Error())})
		return ctrl.Result{}, err
	}

	// ── the level-triggered pass: provider snapshot → mirror upsert (AC2) ──
	records, err := provider.Snapshot(ctx, project.Spec.Repo.URL, r.snapshotOptions(sync))
	if err != nil {
		logger.Error(err, "repo-sync: provider snapshot failed", "project", req.NamespacedName)
		// A provider rate limit gets a respectful scheduled retry at the
		// provider's own Retry-After — not exponential backoff fighting it.
		var rateLimited *scm.RateLimitedError
		if errors.As(err, &rateLimited) {
			delay := rateLimited.RetryAfter
			if delay < time.Second {
				delay = time.Second
			}
			r.patchStatus(ctx, project, statusPatch{condition: syncReadyFalse(reasonProviderFail, err.Error())})
			return ctrl.Result{RequeueAfter: delay}, nil
		}
		r.patchStatus(ctx, project, statusPatch{condition: syncReadyFalse(reasonProviderFail, err.Error())})
		return ctrl.Result{}, err
	}

	rows := scm.BuildMirrorRows(project.Namespace, project.Name, provider, project.Spec.Repo.URL, records, r.botActor())
	applied, err := r.Store.ApplySnapshot(ctx, project.Namespace, project.Name, rows)
	if err != nil {
		logger.Error(err, "repo-sync: mirror upsert failed", "project", req.NamespacedName)
		r.patchStatus(ctx, project, statusPatch{condition: syncReadyFalse(reasonMirrorFail, err.Error())})
		return ctrl.Result{}, err
	}

	// Status is a downstream observation only (§5.1 status discipline):
	// the mirror pass succeeded, so record liveness. Counts and timestamps
	// are observations of the inbound loop, never control input.
	webhookAt := time.Time{}
	if raw, ok := project.Annotations[TriggerAnnotation]; ok {
		if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			webhookAt = t
		}
	}
	r.patchStatus(ctx, project, statusPatch{
		condition: syncReadyTrue(applied),
		sync: &ksquadapi.ProjectSyncStatus{
			LastMirrorTime:    ptrTime(metav1.Now()),
			LastWebhookTime:   nilIfZero(webhookAt),
			MirrorRecordCount: int64(applied),
		},
	})

	// Poll fallback (AC3): the interval comes from the spec values — two
	// Projects with distinct intervals schedule distinctly.
	return ctrl.Result{RequeueAfter: time.Duration(r.pollInterval(sync)) * time.Second}, nil
}

// SetupWithManager registers the reconciler for Project events.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Providers == nil {
		r.Providers = scm.NewProviderRegistry()
	}
	if r.Store == nil {
		return fmt.Errorf("reposync.Reconciler requires a MirrorStore (scm.NewSQLMirrorStore over the coord pool)")
	}
	if r.APIReader == nil {
		// Uncached reads for Secrets: keeps the manager from starting a
		// cluster-wide Secret informer (memory + compromise blast radius).
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&ksquadapi.Project{}).
		Complete(r)
}

// resolveCredentials reads the per-Project BYO provider Secret. A missing
// Secret or empty token is a hard error: repo-sync must fail closed rather
// than fall back to any anonymous/shared credential (AC5).
func (r *Reconciler) resolveCredentials(ctx context.Context, project *ksquadapi.Project) (scm.ProviderCredentials, error) {
	auth := project.Spec.Repo.Auth
	if auth == nil || auth.CredentialSecretRef.Name == "" {
		return scm.ProviderCredentials{}, fmt.Errorf(
			"spec.repo.auth.credentialSecretRef is required when repo.sync is configured (BYO per-Project credential, AC5)")
	}
	key := types.NamespacedName{Name: auth.CredentialSecretRef.Name, Namespace: project.Namespace}
	secret := &corev1.Secret{}
	if err := r.reader().Get(ctx, key, secret); err != nil {
		return scm.ProviderCredentials{}, fmt.Errorf("resolve BYO provider secret %s: %w", key, err)
	}
	tokenKey := auth.CredentialSecretRef.Key
	if tokenKey == "" {
		tokenKey = tokenSecretKey
	}
	token := secret.Data[tokenKey]
	if len(token) == 0 {
		return scm.ProviderCredentials{}, fmt.Errorf("BYO provider secret %s has empty %q key", key, tokenKey)
	}
	return scm.ProviderCredentials{Token: string(token), TokenType: "pat"}, nil
}

// reader returns the Secret reader: the uncached API reader when wired,
// otherwise the embedded client (unit tests with a fake client).
func (r *Reconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// snapshotOptions maps the spec mirror subset onto the provider snapshot
// filter. Nil mirror = the default full set; explicit false opts a class out.
func (r *Reconciler) snapshotOptions(sync *ksquadapi.RepoSyncSpec) scm.SnapshotOptions {
	m := sync.Mirror
	if m == nil {
		return scm.SnapshotOptions{}
	}
	var types []scm.RecordType
	include := func(enabled *bool) bool { return enabled == nil || *enabled }
	if include(m.Issues) {
		types = append(types, scm.RecordTypeIssue)
	}
	if include(m.PullRequests) {
		types = append(types, scm.RecordTypePR)
	}
	if include(m.CheckRuns) {
		types = append(types, scm.RecordTypeCheckRun)
	}
	if include(m.Artifacts) {
		types = append(types, scm.RecordTypeArtifact)
	}
	return scm.SnapshotOptions{Types: types}
}

// pollInterval returns the poll cadence in seconds, from the spec values —
// zero means the 300s default, and anything below one minute is clamped
// (AC3: the interval tracks values, never a reconciler hardcode).
func (r *Reconciler) pollInterval(sync *ksquadapi.RepoSyncSpec) int32 {
	if sync.PollIntervalSeconds <= 0 {
		return DefaultPollIntervalSeconds
	}
	if sync.PollIntervalSeconds < minPollIntervalSeconds {
		return minPollIntervalSeconds
	}
	return sync.PollIntervalSeconds
}

func (r *Reconciler) botActor() string {
	if r.BotActor == "" {
		return scm.DefaultBotActor
	}
	return r.BotActor
}

// statusPatch is the subset of Project.status one reconcile pass writes.
type statusPatch struct {
	condition metav1.Condition
	sync      *ksquadapi.ProjectSyncStatus
}

// patchStatus applies the status patch through the status subresource,
// preserving unrelated conditions. Failures are logged, not returned:
// status is observation, and a failed observation write must not fail the
// mirror pass it describes.
//
// The copy comes FIRST: meta.SetStatusCondition mutates the condition it
// finds through the slice's backing array, so mutating project's own
// conditions before DeepCopy would leave the DeepEqual guard comparing the
// mutated original against a copy of itself — every write after the first
// would be silently suppressed. A MergeFrom patch (not Update) also keeps
// a concurrent status writer's unrelated fields from being clobbered.
func (r *Reconciler) patchStatus(ctx context.Context, project *ksquadapi.Project, patch statusPatch) {
	logger := log.FromContext(ctx)

	next := project.DeepCopy()
	patch.condition.LastTransitionTime = lastTransition(next.Status.Conditions, patch.condition.Type, patch.condition.Status)
	meta.SetStatusCondition(&next.Status.Conditions, patch.condition)
	if patch.sync != nil {
		if next.Status.Sync == nil {
			next.Status.Sync = &ksquadapi.ProjectSyncStatus{}
		}
		if patch.sync.LastMirrorTime != nil {
			next.Status.Sync.LastMirrorTime = patch.sync.LastMirrorTime
		}
		if patch.sync.LastWebhookTime != nil {
			next.Status.Sync.LastWebhookTime = patch.sync.LastWebhookTime
		}
		if patch.sync.MirrorRecordCount != 0 {
			next.Status.Sync.MirrorRecordCount = patch.sync.MirrorRecordCount
		}
	}
	if apiequality.Semantic.DeepEqual(project.Status, next.Status) {
		return
	}
	if err := r.Status().Patch(ctx, next, client.MergeFrom(project)); err != nil {
		logger.Error(err, "repo-sync: project status update failed", "project", project.Name)
	}
}

func lastTransition(conditions []metav1.Condition, condType string, status metav1.ConditionStatus) metav1.Time {
	for _, c := range conditions {
		if c.Type == condType && c.Status == status {
			return c.LastTransitionTime
		}
	}
	return metav1.Now()
}

func syncReadyTrue(applied int) metav1.Condition {
	return metav1.Condition{
		Type:    ConditionSyncReady,
		Status:  metav1.ConditionTrue,
		Reason:  reasonSynced,
		Message: fmt.Sprintf("mirror pass applied %d records", applied),
	}
}

func syncReadyFalse(reason, message string) metav1.Condition {
	return metav1.Condition{
		Type:    ConditionSyncReady,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	}
}

func ptrTime(t metav1.Time) *metav1.Time { return &t }

func nilIfZero(t time.Time) *metav1.Time {
	if t.IsZero() {
		return nil
	}
	return ptrTime(metav1.NewTime(t))
}
