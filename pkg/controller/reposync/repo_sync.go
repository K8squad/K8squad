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

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	ksquadapi "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/issuesync"
	"github.com/K8squad/K8squad/pkg/scm"
	"github.com/K8squad/K8squad/pkg/telemetry"
	"github.com/K8squad/K8squad/pkg/telemetry/scmmetrics"
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
	reasonIssueSync    = "IssueSyncError"
	reasonReviewFail   = "ReviewTriggerError"
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

// ReviewTrigger is the OPTIONAL PR-review-automation dispatch seam (ISI-4750
// E3/E4). It is introduced here (E3) and implemented by E4 (ISI-4766); a nil
// trigger disables review automation entirely, exactly like a nil IssueSync
// disables the link pass. When wired, the SAME reconcile pass that upsert the
// mirror and ran the issue link pass hands the JUST-APPLIED snapshot rows to
// the trigger — same triggers (webhook + poll), same level-triggered
// discipline. E3 deliberately owns only the seam and the invocation site: the
// trigger BODY (which rows qualify, the work-item label dedup on (PR number,
// head SHA), the create-if-absent and the dispatch) lives entirely in E4, so
// this reconciler never touches the ISI-4711 human-only custody wall.
type ReviewTrigger interface {
	// ReviewChanges inspects one Project's just-applied mirror rows and
	// dispatches review for new/changed qualifying PRs. It MUST be
	// level-triggered and idempotent: re-running it on an unchanged snapshot
	// (a redelivered webhook, a poll tick) is a no-op, because dedup is keyed
	// on the (PR number, head SHA) pair now carried on the PR rows' payload —
	// there is no stored diff state. A failure fails the reconcile so the
	// next level-triggered pass retries against the re-applied mirror.
	ReviewChanges(ctx context.Context, projectNamespace, projectName string, provider scm.SourceProvider, repoURL string, rows []scm.MirrorRow) error
}

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

	// IssueSync is the story-11.2 issue⇄work-item engine. Nil disables the
	// link pass (no links configured / unit tests); when wired, the SAME
	// reconcile pass that upserts the mirror also drives status/labels
	// across the seam for every scm.issue_link of this Project, per the
	// configured direction (spec.repo.sync.issueSync.direction).
	IssueSync *issuesync.Syncer

	// ReviewTrigger is the OPTIONAL PR-review-automation dispatch seam
	// (ISI-4750 E3 introduces it, E4 wires the implementation). Nil disables
	// review automation; when wired, the same reconcile pass hands the
	// just-applied PR rows to it after the link pass. See ReviewTrigger.
	ReviewTrigger ReviewTrigger

	// BotActor is the echo-suppression identity (default scm.DefaultBotActor):
	// provider records authored by this actor are OUR reflected writes and are
	// dropped on the way in (AC6).
	BotActor string

	// Metrics records the scm sync surface (ISI-4395 GH-2/GH-3): the
	// scm.sync span, the sync/duration/panic counters and the mirror-age /
	// rate-limit gauges. Nil is safe — every method is a no-op on a nil
	// receiver — so unit tests and stripped binaries need not wire it.
	Metrics *scmmetrics.Metrics
}

// +kubebuilder:rbac:groups=ksquad.io,resources=projects,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=ksquad.io,resources=projects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

// Reconcile runs one level-triggered mirror pass for the requested Project
// and schedules the poll fallback. Missing Projects are not errors (deleted
// mid-queue); provider/store failures set SyncReady=False and return the
// error so controller-runtime requeues with backoff.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	logger := log.FromContext(ctx)

	project := &ksquadapi.Project{}
	if err := r.Get(ctx, req.NamespacedName, project); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	sync := project.Spec.Repo.Sync
	if sync == nil {
		// Repo-sync not configured: nothing to mirror, no poll to schedule.
		// A Project without sync is not an error state (§5.4). No scm.sync span
		// is opened for an unconfigured Project — it is not a sync at all.
		return ctrl.Result{}, nil
	}

	// GH-2 trace join: a good-signature webhook (cmd/scm-webhook) Inject-ed its
	// W3C trace context onto the Project annotations before bumping the trigger.
	// Extracting it here re-parents the scm.sync span onto the webhook's trace,
	// so the webhook→reconcile hop reads as ONE distributed trace. A poll-driven
	// pass has no inbound context and simply roots a fresh trace.
	ctx = telemetry.Extract(ctx, project.Annotations)
	trigger := classifyTrigger(project)
	projectKey := project.Namespace + "/" + project.Name

	ctx, span := telemetry.Tracer().Start(ctx, "scm.sync", trace.WithAttributes(
		attribute.String("ksquad.scm.provider", sync.Provider),
		attribute.String("ksquad.scm.trigger", trigger),
		attribute.String("ksquad.project.name", project.Name),
		attribute.String("ksquad.project.namespace", project.Namespace),
	))
	start := time.Now()
	reason := reasonSynced
	success := false
	// One defer owns the span close AND the sync/panic metric (GH-2/GH-3).
	// A panic in the Providers/Store/Snapshot derefs (repo_sync.go nil-guard
	// history, ISI-4113/4117) is recorded on the span + counted on
	// ksquad_scm_sync_panics_total and then RE-RAISED: the crash stays a crash,
	// it just stops being invisible. controller-runtime's own panic recovery
	// and the SetupWithManager nil-Client guard still backstop the r.Get above,
	// which runs before this span exists.
	defer func() {
		if rec := recover(); rec != nil {
			span.RecordError(fmt.Errorf("scm.sync panic: %v", rec))
			span.SetStatus(codes.Error, "panic")
			r.Metrics.RecordPanic(ctx, sync.Provider)
			span.End()
			panic(rec)
		}
		if retErr != nil {
			span.RecordError(retErr)
			span.SetStatus(codes.Error, retErr.Error())
		}
		span.SetAttributes(attribute.String("ksquad.scm.reason", reason))
		r.Metrics.RecordSync(ctx, scmmetrics.SyncOutcome{
			Provider: sync.Provider,
			Trigger:  trigger,
			Reason:   reason,
			Project:  projectKey,
			Duration: time.Since(start),
			Success:  success,
		})
		span.End()
	}()

	// Resolve the BYO credential per Project (AC5): the token comes from the
	// referenced Secret, is handed to the provider factory, and leaves no
	// other trace — no field on this struct, no log line, no Run path.
	creds, err := r.resolveCredentials(ctx, project)
	if err != nil {
		logger.Error(err, "repo-sync: BYO credential not resolvable", "project", req.NamespacedName)
		reason = reasonNoCredential
		// The error text is surfaced on status/span, but the CredentialMissing
		// path must never let a BYO token leak into it — resolveCredentials
		// only ever returns "secret not resolvable"/"empty key" shapes, never
		// the secret bytes (PII hygiene, NFR-SEC8).
		r.patchStatus(ctx, project, statusPatch{condition: syncReadyFalse(reasonNoCredential, err.Error())})
		// Error only: controller-runtime ignores RequeueAfter alongside a
		// non-nil error (it requeues with backoff instead).
		return ctrl.Result{}, err
	}

	provider, err := r.Providers.Provider(ctx, sync.Provider, creds)
	if err != nil {
		logger.Error(err, "repo-sync: provider not resolvable", "project", req.NamespacedName)
		reason = reasonProviderFail
		r.patchStatus(ctx, project, statusPatch{condition: syncReadyFalse(reasonProviderFail, err.Error())})
		return ctrl.Result{}, err
	}

	// ── the level-triggered pass: provider snapshot → mirror upsert (AC2) ──
	records, err := provider.Snapshot(ctx, project.Spec.Repo.URL, r.snapshotOptions(sync))
	// Feed the provider rate-limit headroom gauge from whatever the pass saw,
	// success or failure (GH-3): a snapshot that just tripped the limit reports
	// 0-ish headroom, which is exactly the signal the dashboard needs.
	r.observeRateLimit(provider)
	if err != nil {
		logger.Error(err, "repo-sync: provider snapshot failed", "project", req.NamespacedName)
		reason = reasonProviderFail
		// A provider rate limit gets a respectful scheduled retry at the
		// provider's own Retry-After — not exponential backoff fighting it.
		var rateLimited *scm.RateLimitedError
		if errors.As(err, &rateLimited) {
			delay := rateLimited.RetryAfter
			if delay < time.Second {
				delay = time.Second
			}
			// The condition message must be byte-stable across passes
			// within one rate-limit window: every status write re-fires
			// the Project watch, so a per-pass-fresh countdown (raw
			// fractional seconds) rewrote the condition on every
			// reconcile and controller-runtime hot-looped (ISI-4120,
			// several reconciles/sec per Project). The message carries a
			// minute-bucketed countdown; the requeue below keeps the
			// FULL Retry-After precision.
			r.patchStatus(ctx, project, statusPatch{
				condition: syncReadyFalse(reasonProviderFail, rateLimitMessage(sync.Provider, delay)),
			})
			return ctrl.Result{RequeueAfter: delay}, nil
		}
		r.patchStatus(ctx, project, statusPatch{condition: syncReadyFalse(reasonProviderFail, err.Error())})
		return ctrl.Result{}, err
	}

	rows := scm.BuildMirrorRows(project.Namespace, project.Name, provider, project.Spec.Repo.URL, records, r.botActor())
	applied, err := r.Store.ApplySnapshot(ctx, project.Namespace, project.Name, rows)
	if err != nil {
		logger.Error(err, "repo-sync: mirror upsert failed", "project", req.NamespacedName)
		reason = reasonMirrorFail
		r.patchStatus(ctx, project, statusPatch{condition: syncReadyFalse(reasonMirrorFail, err.Error())})
		return ctrl.Result{}, err
	}

	// Anchor the (Project, repo) pair in scm.repo (0008 schema contract):
	// one row per mirrored repo carrying the pass's freshness. Outside the
	// snapshot tx — see SQLMirrorStore.UpsertRepo — and a failure here
	// surfaces as a failed reconcile so the next pass re-anchors.
	if err := r.Store.UpsertRepo(ctx, project.Namespace, project.Name, sync.Provider, project.Spec.Repo.URL, time.Now()); err != nil {
		logger.Error(err, "repo-sync: scm.repo anchor upsert failed", "project", req.NamespacedName)
		reason = reasonMirrorFail
		r.patchStatus(ctx, project, statusPatch{condition: syncReadyFalse(reasonMirrorFail, err.Error())})
		return ctrl.Result{}, err
	}

	// ── the story-11.2 link pass: mirror rows → linked work items ──
	// Same pass, same triggers (webhook + poll), same level-triggered
	// discipline: the engine diffs the JUST-APPLIED snapshot against the
	// link bookkeeping and applies LWW with audited conflicts. A failure
	// here fails the reconcile (the mirror itself already applied — the
	// next pass re-applies it idempotently and retries the link pass).
	if r.IssueSync != nil {
		if _, err := r.IssueSync.SyncProject(ctx, project.Namespace, project.Name,
			provider, project.Spec.Repo.URL, sync.EffectiveIssueSyncDirection(), rows); err != nil {
			logger.Error(err, "repo-sync: issue link pass failed", "project", req.NamespacedName)
			reason = reasonIssueSync
			r.patchStatus(ctx, project, statusPatch{condition: syncReadyFalse(reasonIssueSync, err.Error())})
			return ctrl.Result{}, err
		}
	}

	// ── the ISI-4750 review-automation trigger: mirror PR rows → review dispatch ──
	// Runs AFTER the link pass on the SAME just-applied rows and the SAME
	// level-triggered discipline. E3 owns only this nil-guarded invocation; the
	// trigger body (qualify → dedup by (PR number, head SHA) label →
	// create-if-absent → dispatch) is E4 (ISI-4766), kept behind the seam so
	// this reconciler stays clear of the human-only custody wall (ISI-4711).
	// A failure fails the reconcile — the mirror already applied, so the next
	// level-triggered pass re-applies it and retries the trigger idempotently.
	if r.ReviewTrigger != nil {
		if err := r.ReviewTrigger.ReviewChanges(ctx, project.Namespace, project.Name,
			provider, project.Spec.Repo.URL, rows); err != nil {
			logger.Error(err, "repo-sync: review trigger failed", "project", req.NamespacedName)
			reason = reasonReviewFail
			r.patchStatus(ctx, project, statusPatch{condition: syncReadyFalse(reasonReviewFail, err.Error())})
			return ctrl.Result{}, err
		}
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

	// The pass applied the mirror: mark success so the deferred metric stamps
	// the mirror-age gauge for this Project and records reason=Synced.
	success = true
	span.SetAttributes(attribute.Int("ksquad.scm.mirror.record_count", applied))

	// Poll fallback (AC3): the interval comes from the spec values — two
	// Projects with distinct intervals schedule distinctly.
	return ctrl.Result{RequeueAfter: time.Duration(r.pollInterval(sync)) * time.Second}, nil
}

// classifyTrigger labels a reconcile pass as webhook- or poll-driven for the
// scm.sync span and the ksquad_scm_sync_total{trigger} metric. Heuristic (the
// reconcile itself is level-triggered and identical either way): a scm-sync
// trigger annotation whose timestamp is NEWER than the last recorded successful
// mirror means an external trigger arrived since the last pass — a webhook
// delivery OR a manual "Sync now" (both bump the same annotation; the apiserver
// side tags the manual path distinctly, GH-4). Otherwise it is the scheduled
// poll requeue (or a spec change). Both values are bounded — no cardinality
// risk.
func classifyTrigger(project *ksquadapi.Project) string {
	raw, ok := project.Annotations[TriggerAnnotation]
	if !ok || raw == "" {
		return scmmetrics.TriggerPoll
	}
	triggeredAt, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return scmmetrics.TriggerPoll
	}
	if sync := project.Status.Sync; sync != nil && sync.LastMirrorTime != nil {
		if !triggeredAt.After(sync.LastMirrorTime.Time) {
			return scmmetrics.TriggerPoll
		}
	}
	return scmmetrics.TriggerWebhook
}

// observeRateLimit feeds the provider rate-limit headroom gauge when the
// provider implements the optional scm.RateLimitReporter seam (GH-3). Providers
// that cannot report headroom are simply skipped.
func (r *Reconciler) observeRateLimit(provider scm.SourceProvider) {
	if r.Metrics == nil {
		return
	}
	reporter, ok := provider.(scm.RateLimitReporter)
	if !ok {
		return
	}
	if remaining, seen := reporter.LastRateRemaining(); seen {
		r.Metrics.ObserveRateLimit(provider.Name(), remaining)
	}
}

// SetupWithManager registers the reconciler for Project events.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Providers == nil {
		r.Providers = scm.NewProviderRegistry()
	}
	if r.Store == nil {
		return fmt.Errorf("reposync.Reconciler requires a MirrorStore (scm.NewSQLMirrorStore over the coord pool)")
	}
	if r.Client == nil {
		// The composition root (cmd/operator) must hand over the manager's
		// client, but Reconcile's very first act is r.Get — a nil embedded
		// client panics there on EVERY reconcile (observed as a live panic
		// loop on k8squad-test, ISI-4113 diagnosis). Default it here so the
		// zero-Client constructor can never ship that crash again.
		r.Client = mgr.GetClient()
	}
	if r.APIReader == nil {
		// Uncached reads for Secrets: keeps the manager from starting a
		// cluster-wide Secret informer (memory + compromise blast radius).
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("project").
		For(&ksquadapi.Project{}).
		// Our own status writes re-fire the Project watch: without a
		// filter, every status patch (the per-pass SyncReady message)
		// became another immediate reconcile — the ISI-4120 hot loop.
		// Reconciles are driven by spec changes (generation), the webhook
		// trigger annotation, and the scheduled RequeueAfter.
		WithEventFilter(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
		)).
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
	// Releases are OPT-IN (ISI-3956 S5a): nil/false does NOT include them, so
	// the extra API class is spent only when explicitly requested — the
	// inverse of the include() default the other kinds use.
	if m.Releases != nil && *m.Releases {
		types = append(types, scm.RecordTypeRelease)
	}
	// Branches are OPT-IN exactly like releases (ISI-4026): the bounded
	// branch-list call is an extra API class per sync tick, spent only when
	// explicitly requested.
	if m.Branches != nil && *m.Branches {
		types = append(types, scm.RecordTypeBranch)
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

// rateLimitMessage renders the SyncReady=False message for a rate-limited
// snapshot pass. The countdown is deliberately QUANTIZED to whole minutes:
// our own status writes re-fire the Project watch, so a message that
// changes every pass (raw fractional Retry-After) means a new status patch
// and another immediate reconcile — the ISI-4120 hot loop. A minute bucket
// keeps the message byte-stable within one rate-limit window while still
// telling the operator roughly how long the deferral runs. Only the
// MESSAGE is coarse; the requeue keeps full Retry-After precision.
func rateLimitMessage(provider string, delay time.Duration) string {
	if delay < time.Minute {
		return fmt.Sprintf("%s rate limited, snapshot retry deferred <1m", provider)
	}
	return fmt.Sprintf("%s rate limited, snapshot retry deferred ~%s", provider, delay.Round(time.Minute))
}

func ptrTime(t metav1.Time) *metav1.Time { return &t }

func nilIfZero(t time.Time) *metav1.Time {
	if t.IsZero() {
		return nil
	}
	return ptrTime(metav1.NewTime(t))
}
