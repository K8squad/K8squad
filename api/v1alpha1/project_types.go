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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProjectSpec defines the desired state of Project (arch §5.1, §5.4, §8.5,
// story 1.2 AC5).
//
// A Project couples an upstream source repository (mirrored, not made the
// source of truth — §5.4) with a workspace and a context budget.
type ProjectSpec struct {
	// Repo is the upstream source repository mirrored by KSquad (§5.4:
	// GitHub is the v1 provider behind the pkg/scm seam; the fenced
	// coordination record stays authoritative).
	// +kubebuilder:validation:Required
	Repo RepoSpec `json:"repo"`

	// WorkspacePVC sizes and classes the Project workspace PVC (§9.4 —
	// Runs work in their own git-worktree on this volume).
	// +optional
	WorkspacePVC *PVCSpec `json:"workspacePVC,omitempty"`

	// EgressPolicyRef references the egress policy applied to this
	// project's Runs (§12.2 — default-deny NetworkPolicy + allowlist).
	// +optional
	EgressPolicyRef *ObjectRef `json:"egressPolicyRef,omitempty"`

	// Goals are project-level goals injected into every Run's context
	// envelope (§8.5). A goal change is a new Project revision; the next Run
	// assembles against it while in-flight Runs keep their snapshot.
	// +optional
	Goals []string `json:"goals,omitempty"`

	// Conventions is free-form project convention guidance (coding style,
	// commit/PR discipline, review norms) injected into every Run's context
	// envelope as a project-metadata class (§8.5, ISI-5280 WS-E). Like Goals,
	// it is DECLARATIVE, NON-SECRET descriptor text — not a credential surface.
	// The context assembler treats it as authoritative-tier best-effort, so a
	// large value is trimmed under budget rather than dropping the task itself.
	// +optional
	Conventions string `json:"conventions,omitempty"`

	// ArchDocRefs are references to the project's architecture / design
	// documents (URLs or repo-relative paths) injected into every Run's
	// context envelope (§8.5, ISI-5280 WS-E). These are CITATIONS the agent
	// can cite/open, not mirrored bodies — NON-SECRET pointers only.
	// +optional
	ArchDocRefs []string `json:"archDocRefs,omitempty"`

	// ContextBudget is the project-level default per-tier token allocation
	// (§8.5) — raise it once for projects with large architecture docs and
	// every agent on the project inherits it. Per-Agent overrides via
	// Agent.spec.contextBudgetOverride; per-Run dynamic trim in the shim.
	// +optional
	ContextBudget *ContextBudget `json:"contextBudget,omitempty"`

	// OwnedBy is the owner principal ref (story 1.6, ISI-2522): the
	// authoritative ownership signal for resource-scoped permission checks
	// (Epic 15.3) — not a display field. Mutable: ownership may be
	// transferred after creation. Defaults to the created-by principal at
	// admission (internal/webhook AttributionWebhook) and is indexed for
	// RBAC scope queries (internal/index).
	// +optional
	OwnedBy PrincipalRef `json:"ownedBy,omitempty"`
}

var _ OwnedByHolder = &Project{}

// GetOwnedBy returns the spec.ownedBy owner principal (story 1.6).
func (p *Project) GetOwnedBy() PrincipalRef { return p.Spec.OwnedBy }

// SetOwnedBy sets the spec.ownedBy owner principal (story 1.6).
func (p *Project) SetOwnedBy(principal PrincipalRef) { p.Spec.OwnedBy = principal }

// RepoSpec is the upstream source repository of a Project, plus its sync
// configuration (arch §5.4).
type RepoSpec struct {
	// URL of the upstream repository, e.g.
	// "https://github.com/acme/widget".
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	URL string `json:"url"`

	// Ref is the default ref to track (branch or tag); empty means the
	// provider default branch.
	// +optional
	Ref string `json:"ref,omitempty"`

	// Auth carries the per-Project BYO credential for the provider.
	// +optional
	Auth *RepoAuth `json:"auth,omitempty"`

	// Sync configures the repo-sync reconciler (§5.4). Nil disables sync.
	// +optional
	Sync *RepoSyncSpec `json:"sync,omitempty"`

	// ReviewAutomation configures human-enabled, system-executed PR-review
	// automation for this Project (ISI-4750, PRD v1 D1-D6). A human enabling this
	// policy is the authorizing act (D1): the reviewer Run is later dispatched
	// under a SYSTEM identity, NOT agent-initiated - this config introduces NO
	// agent-authored work item and does not cross the ISI-4711 custody wall.
	// Nil (the default) means automation is off. Writing this config is INERT
	// until the E3 change-detection and E4 dispatch epics land - it stores policy
	// only and triggers nothing yet.
	// +optional
	ReviewAutomation *ReviewAutomationSpec `json:"reviewAutomation,omitempty"`

	// Automation groups the newer, human-enabled / system-executed per-section
	// automation policies for this Project (ISI-5595). Each sub-policy is opt-in,
	// independently gated, and executed under its own SYSTEM identity — never
	// agent-initiated, exactly like ReviewAutomation (ISI-4711 custody wall).
	// reviewAutomation stays on spec.repo directly for backward-compat (ISI-4750);
	// the new sections (issueTriage, and later ciFailure) live here so all three
	// read/write uniformly (ISI-5595 D2). Nil (the default) means no new-section
	// automation is configured.
	// +optional
	Automation *RepoAutomationSpec `json:"automation,omitempty"`
}

// RepoAutomationSpec groups the ISI-5595 per-section automation policies that are
// NOT the legacy PR-review one (that stays at spec.repo.reviewAutomation for
// backward-compat, D2). Each field is an independently-gated, opt-in standing
// policy executed under a SYSTEM identity. Nil sub-policies are off.
type RepoAutomationSpec struct {
	// IssueTriage configures auto-triage of newly-opened GitHub issues (ISI-5595
	// WS-B): mint an internal ticket and dispatch a configured triage agent for
	// each new open issue the mirror captures. Nil / disabled = today's behaviour
	// exactly (issue→ticket is MANUAL only via the console Issues board bridge).
	// +optional
	IssueTriage *IssueTriageSpec `json:"issueTriage,omitempty"`

	// CiFailure configures human-enabled, system-executed CI-failure triage for
	// this Project (ISI-5595 WS-C, board-approved D3/D4). A human enabling this
	// policy is the authorizing act: when a mirrored CI check run finishes with a
	// configured conclusion (default: failure) on an in-scope ref, the operator
	// mints a triage ticket and dispatches it to the configured agent under a
	// SYSTEM identity (system:ci-failure) — NOT agent-initiated, so it introduces
	// no agent-authored work item and does not cross the ISI-4711 custody wall. It
	// reuses the EXISTING check-run mirror (Conclusion); no workflow_run mirroring
	// is added (D3). Nil (the default) means CI-failure triage is off.
	// +optional
	CiFailure *CiFailureSpec `json:"ciFailure,omitempty"`
}

// IssueTriageSpec is the GitHub-issue auto-triage policy (ISI-5595 WS-B). It is
// the generalization of ReviewAutomationSpec to the Issues section: a human
// enabling it (Enabled=true) is the D1 authorizing act, provenanced by the
// server-stamped EnabledBy; the triage Run is later dispatched under the SYSTEM
// identity system:issue-triage, NOT agent-initiated, so it crosses no custody
// wall (ISI-4711). The auto path reuses the EXACT mechanics of the manual
// issue→ticket bridge (internal/apiserver/githubissuedispatch.go): find-or-create
// a work item keyed on the shared join label ksquad.github.issue=owner/repo#N,
// then dispatch — so an auto-triaged and a manually-assigned issue converge on
// ONE ticket and are never double-handled.
type IssueTriageSpec struct {
	// Enabled turns the standing policy on. When false (or the whole struct is
	// nil) no issue is ever auto-triaged. Enabling is the human authorizing act
	// for the D1 system-dispatch path.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// TriageAgentID is the team agent that triages new issues. It MUST be an agent
	// in the owning Team's composition (the coord dispatch enforces the
	// agent-∈-Team check on write). Required whenever Enabled is true.
	// +optional
	TriageAgentID string `json:"triageAgentId,omitempty"`

	// LabelFilter optionally restricts triage to issues carrying AT LEAST ONE of
	// these GitHub labels (case-insensitive match against the mirrored issue
	// labels). Empty (the default) triages every qualifying open issue regardless
	// of labels.
	// +optional
	LabelFilter []string `json:"labelFilter,omitempty"`

	// OnlyUnassigned, when true (the default), triages only issues that have NO
	// GitHub assignee — so an issue a human has already picked up upstream is left
	// alone. Set false to triage every qualifying open issue. A nil value means
	// the default (true); use EffectiveOnlyUnassigned to resolve it.
	// +optional
	OnlyUnassigned *bool `json:"onlyUnassigned,omitempty"`

	// EnabledBy is the SERVER-STAMPED principal that last set Enabled=true — the
	// D1 authorizing-act provenance threaded into the system dispatch. Written by
	// the apiserver from the authenticated caller on any write that sets
	// Enabled=true, and CLEARED when Enabled is set false. NEVER trusted from the
	// request body. Read-only from the caller's perspective.
	// +optional
	EnabledBy string `json:"enabledBy,omitempty"`

	// EnabledAt is the SERVER-STAMPED timestamp Enabled was last set true — the D4
	// forward-only watermark. The trigger auto-triages ONLY issues created or
	// updated at/after this instant, so enabling on a repo with a large open-issue
	// backlog does NOT flood the squad with the entire history. Written/cleared by
	// the apiserver alongside EnabledBy; NEVER trusted from the request body.
	// +optional
	EnabledAt *metav1.Time `json:"enabledAt,omitempty"`
}

// CiFailureSpec is the Actions CI-failure triage policy (ISI-5595 WS-C, plan r1,
// board-approved D3/D4). D3: the trigger consumes the EXISTING check-run mirror
// (RecordTypeCheckRun, Conclusion) — no workflow_run mirroring is added. D4: the
// trigger is forward-only from EnabledAt, so enabling it never retroactively
// triages the backlog of failures already in the mirror.
type CiFailureSpec struct {
	// Enabled turns the standing policy on. When false (or the whole struct is
	// nil) no CI-failure triage is ever triggered. Enabling is the human
	// authorizing act for the system-dispatch path.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// AgentID is the team agent the triage ticket is dispatched to. It MUST be an
	// agent in the owning Team's composition. Required whenever Enabled is true.
	// +optional
	AgentID string `json:"agentId,omitempty"`

	// BranchFilter narrows which refs a failing check run triages, matched against
	// the check suite's head branch (glob-free exact match, lower-cased). Empty
	// (the default) accepts every mirrored check-run ref, which is already scoped
	// by the mirror to the default branch plus open-PR heads — i.e. the current
	// check-run scope (D3). A failing run whose head branch is unknown is accepted
	// only when BranchFilter is empty; a non-empty filter requires a known,
	// matching branch (fail-closed narrowing).
	// +optional
	BranchFilter []string `json:"branchFilter,omitempty"`

	// Conclusions selects which check-run conclusions qualify as a failure.
	// Empty (the default) means ["failure"]; "timed_out" and "cancelled" may be
	// added to also triage those terminal non-success conclusions (D3).
	// +optional
	// +kubebuilder:validation:items:Enum=failure;timed_out;cancelled
	Conclusions []string `json:"conclusions,omitempty"`

	// EnabledBy is the SERVER-STAMPED principal that last set Enabled=true — the
	// authorizing-act provenance threaded into the system dispatch as the
	// Principal. It is written by the apiserver from the authenticated caller on
	// any write that sets Enabled=true, and CLEARED when Enabled is set false. It
	// is NEVER trusted from the request body. Required whenever Enabled is true.
	// +optional
	EnabledBy string `json:"enabledBy,omitempty"`

	// EnabledAt is the SERVER-STAMPED time Enabled was last set true — the
	// forward-only watermark (D4): only check runs that COMPLETED at/after this
	// instant are triaged, so enabling never retroactively triages historical
	// failures already sitting in the mirror. Written alongside EnabledBy and
	// CLEARED when Enabled is set false; NEVER trusted from the request body.
	// Required whenever Enabled is true.
	// +optional
	EnabledAt *metav1.Time `json:"enabledAt,omitempty"`
}

// EffectiveOnlyUnassigned resolves the OnlyUnassigned toggle, applying the
// default (true) when unset (nil). Callers get the default from one place rather
// than re-deciding nil-handling.
func (s *IssueTriageSpec) EffectiveOnlyUnassigned() bool {
	if s == nil || s.OnlyUnassigned == nil {
		return true
	}
	return *s.OnlyUnassigned
}

// CI-failure conclusion vocabulary (ISI-5595 WS-C, D3). Kept as Go constants so
// the operator trigger defaults and matches against the SAME values the CRD enum
// markers pin.
const (
	// CiFailureConclusionFailure is the default qualifying conclusion.
	CiFailureConclusionFailure = "failure"
	// CiFailureConclusionTimedOut optionally also triages timed-out check runs.
	CiFailureConclusionTimedOut = "timed_out"
	// CiFailureConclusionCancelled optionally also triages cancelled check runs.
	CiFailureConclusionCancelled = "cancelled"
)

// EffectiveConclusions resolves the qualifying check-run conclusions, applying
// the ["failure"] default when unset (D3: configured per Project, never hardcoded
// in the trigger).
func (s *CiFailureSpec) EffectiveConclusions() []string {
	if s == nil || len(s.Conclusions) == 0 {
		return []string{CiFailureConclusionFailure}
	}
	return s.Conclusions
}

// ReviewAutomationSpec is the PR-review-automation policy (ISI-4750 D6, E0 §2).
// Persisting it is inert until the E3/E4 epics land: it is authoritative config
// the change-detection and system-dispatch paths will later READ, not behaviour
// this story wires.
type ReviewAutomationSpec struct {
	// Enabled turns the standing policy on. When false (or the whole struct is
	// nil) no review is ever triggered. Enabling is the human authorizing act
	// for the D1 system-dispatch path.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// ReviewerAgentID is the team agent that performs the review. It MUST be an
	// agent in the owning Team's composition whose Role carries the code_review
	// capability (Role.spec.activePhases contains "code_review", D5); the
	// apiserver validates eligibility on write via the shared ReviewerEligibility
	// resolver. Required whenever Enabled is true.
	// +optional
	ReviewerAgentID string `json:"reviewerAgentId,omitempty"`

	// Scope selects which PRs are reviewed (D3). "team_authored" (default) only
	// reviews PRs whose actor is a team agent; "all" reviews every PR.
	// +optional
	// +kubebuilder:validation:Enum=team_authored;all
	// +kubebuilder:default=team_authored
	Scope string `json:"scope,omitempty"`

	// Trigger selects when a review fires (D4). "on_open" reviews a newly
	// mirrored PR row once; "on_new_commits" (default) also re-reviews when the
	// mirrored head SHA changes (requires the E3 mirror head-SHA enrichment).
	// +optional
	// +kubebuilder:validation:Enum=on_open;on_new_commits
	// +kubebuilder:default=on_new_commits
	Trigger string `json:"trigger,omitempty"`

	// EnabledBy is the SERVER-STAMPED principal that last set Enabled=true - the
	// D1 authorizing-act provenance threaded into the E4 system dispatch (E0 §5).
	// It is written by the apiserver from the authenticated caller on any write
	// that sets Enabled=true, and CLEARED when Enabled is set false. It is NEVER
	// trusted from the request body: the write handler overwrites/strips any
	// client-supplied value. Read-only from the caller's perspective.
	// +optional
	EnabledBy string `json:"enabledBy,omitempty"`
}

// Review-automation enum values (ISI-4750 D3/D4, E0 §2). Kept as Go constants so
// the apiserver handler and the shared ReviewerEligibility resolver default and
// validate against the SAME vocabulary the CRD enum markers pin.
const (
	// ReviewScopeTeamAuthored reviews only PRs authored by a team agent (default).
	ReviewScopeTeamAuthored = "team_authored"
	// ReviewScopeAll reviews every PR.
	ReviewScopeAll = "all"

	// ReviewTriggerOnOpen reviews a newly mirrored PR row once.
	ReviewTriggerOnOpen = "on_open"
	// ReviewTriggerOnNewCommits also re-reviews when the head SHA changes (default).
	ReviewTriggerOnNewCommits = "on_new_commits"
)

// EffectiveScope resolves the configured review scope, applying the team_authored
// default when unset (D3: the scope is configured per Project, never hardcoded).
func (s *ReviewAutomationSpec) EffectiveScope() string {
	if s == nil || s.Scope == "" {
		return ReviewScopeTeamAuthored
	}
	return s.Scope
}

// EffectiveTrigger resolves the configured review trigger, applying the
// on_new_commits default when unset (D4).
func (s *ReviewAutomationSpec) EffectiveTrigger() string {
	if s == nil || s.Trigger == "" {
		return ReviewTriggerOnNewCommits
	}
	return s.Trigger
}

// RepoAuth is the provider credential discipline for a Project repo
// (arch §5.4, D8, NFR-SEC8): a per-Project/per-user BYO Secret ref, scoped
// to mirror-read (+ status-write only when reflectOutbound) — never a shared
// master token, and never logged, echoed or exposed to an agent Run.
type RepoAuth struct {
	// CredentialSecretRef is the BYO provider credential Secret.
	// +kubebuilder:validation:Required
	CredentialSecretRef SecretRef `json:"credentialSecretRef"`
}

// RepoSyncSpec configures the repo-sync reconciler for a Project
// (arch §5.4, ADR-018).
type RepoSyncSpec struct {
	// Provider is the source-control provider. GitHub is v1; GitLab and
	// Gitea drop in behind the same pkg/scm interface (§5.4, ADR-018).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=github;gitlab;gitea
	Provider string `json:"provider"`

	// WebhookSecretRef references the per-Project HMAC Secret. The HMAC
	// signature is verified before any webhook payload is parsed (§5.4,
	// FR-H4, NFR-SEC8); webhooks are only a fast path on top of the
	// level-triggered reconcile.
	// +optional
	WebhookSecretRef *SecretRef `json:"webhookSecretRef,omitempty"`

	// Mirror selects what the inbound reconciler mirrors into the scm
	// schema. Nil means the default set: issues, pull requests, check runs
	// and release/build artifacts (§5.4).
	// +optional
	Mirror *RepoMirrorSpec `json:"mirror,omitempty"`

	// IssueSync configures the story-11.2 GitHub-issues ⇄ work-items sync
	// for this Project. Nil keeps the default: inbound-only sync for any
	// issue links that exist (§5.4, FR-H1).
	// +optional
	IssueSync *RepoIssueSyncSpec `json:"issueSync,omitempty"`

	// PollIntervalSeconds is the periodic poll-fallback cadence (§5.4,
	// story 11.1 AC3): the level-triggered reconcile re-runs at this
	// interval so a lost webhook is never permanent drift. It comes from
	// the spec/chart values, never a reconciler hardcode; zero means the
	// default of 300s.
	// +optional
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:default=300
	PollIntervalSeconds int32 `json:"pollIntervalSeconds,omitempty"`

	// ReflectOutbound opts in to posting KSquad Run status/comments back to
	// the provider (§5.4): off by default, requires a status-write-scoped
	// token, every write origin-marked for echo suppression.
	// +optional
	ReflectOutbound bool `json:"reflectOutbound,omitempty"`
}

// RepoMirrorSpec selects the inbound mirror subset (arch §5.4). Nil fields
// default to mirroring that object class.
type RepoMirrorSpec struct {
	// Issues mirrors provider issues (FR-H1 issue⇄work-item mapping).
	// +optional
	Issues *bool `json:"issues,omitempty"`

	// PullRequests mirrors PRs incl. review state (FR-H2).
	// +optional
	PullRequests *bool `json:"pullRequests,omitempty"`

	// CheckRuns mirrors CI check runs (FR-H2).
	// +optional
	CheckRuns *bool `json:"checkRuns,omitempty"`

	// Artifacts mirrors release/build artifacts by URI + sha (FR-H2).
	// +optional
	Artifacts *bool `json:"artifacts,omitempty"`

	// Releases mirrors published GitHub releases (tag/name/url/published-at,
	// ISI-3956 S5a). Unlike the other toggles this is OPT-IN: a nil or false
	// value does NOT fetch releases (they cost an extra API class per sync
	// tick and the GitHub-status tab is whole without them). Set true to
	// include RecordTypeRelease in the mirror.
	// +optional
	Releases *bool `json:"releases,omitempty"`

	// Branches mirrors the repo's branch refs (name/head SHA/default flag,
	// ISI-4026). OPT-IN exactly like Releases: a nil or false value does NOT
	// fetch branches (the bounded branch-list call costs an extra API class
	// per sync tick). Set true to include RecordTypeBranch in the mirror.
	// +optional
	Branches *bool `json:"branches,omitempty"`
}

// RepoIssueSyncSpec configures the issue⇄work-item sync loop (story 11.2,
// FR-H1): given issue links in scm.issue_link, the repo-sync reconciler's
// level-triggered pass drives status/labels across the seam per this
// direction, with last-writer-wins conflicts audited (§6.5).
type RepoIssueSyncSpec struct {
	// Direction governs which way changes flow for this Project's issue
	// links: inbound mirrors provider issue status/labels into the linked
	// work item only; bidirectional also reflects KSquad work-item status
	// changes back to the provider issue through the SourceProvider seam
	// (origin-marked for echo suppression, §5.4).
	// +optional
	// +kubebuilder:validation:Enum=inbound;bidirectional
	// +kubebuilder:default=inbound
	Direction string `json:"direction,omitempty"`
}

// EffectiveIssueSyncDirection resolves the configured issue-sync direction,
// applying the inbound default when unset (story 11.2 AC1: the direction is
// configured per Project, never hardcoded in the loop).
func (s *RepoSyncSpec) EffectiveIssueSyncDirection() string {
	if s == nil || s.IssueSync == nil || s.IssueSync.Direction == "" {
		return "inbound"
	}
	return s.IssueSync.Direction
}

// PVCSpec sizes and classes a workspace PVC (arch §5.1 — "workspacePVC
// (size/class)").
type PVCSpec struct {
	// Size of the PVC, e.g. "50Gi".
	// +kubebuilder:validation:Required
	Size resource.Quantity `json:"size"`

	// Class is the storageClass name. Empty does NOT mean the cluster
	// default (story 9.2: relying on the cluster default is
	// misconfiguration) — the operator resolves it from its Helm-provided
	// workspace storage class and fails closed when that is unset too:
	// no PVC is created silently bound to an unsuitable class.
	// +optional
	Class string `json:"class,omitempty"`

	// AccessModes of the PVC (§9.4). Default [ReadWriteOnce] — the
	// serialize-via-lease + worktree-per-Run regime; ReadWriteMany is the
	// opt-in for storage classes that support true parallelism.
	// +optional
	// +kubebuilder:validation:MinItems=1
	AccessModes []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`
}

// defaultWorkspaceAccessModes is the §9.4 default when spec.accessModes is
// unset: RWO — writers are serialized by the per-Project write-lease
// (story 4.4) and each Run works in its own git worktree.
var defaultWorkspaceAccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}

// EffectiveAccessModes resolves the workspace PVC access modes, applying the
// §9.4 default ([ReadWriteOnce]) when spec.accessModes is unset. Callers (the
// PVC reconciler of story 4.4, the workspace defaulting webhook) get the
// default from exactly one place instead of re-hardcoding RWO. A fresh slice
// is returned so callers never mutate the shared default.
func (s *PVCSpec) EffectiveAccessModes() []corev1.PersistentVolumeAccessMode {
	src := s.AccessModes
	if len(src) == 0 {
		src = defaultWorkspaceAccessModes
	}
	out := make([]corev1.PersistentVolumeAccessMode, len(src))
	copy(out, src)
	return out
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=proj,categories=ksquad
// +kubebuilder:subresource:status
// +kubebuilder:webhook:path=/mutate-ksquad-io-v1alpha1-project,mutating=true,failurePolicy=fail,sideEffects=None,groups=ksquad.io,resources=projects,verbs=create;update,versions=v1alpha1,name=mproject-attribution.ksquad.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-ksquad-io-v1alpha1-project,mutating=false,failurePolicy=fail,sideEffects=None,groups=ksquad.io,resources=projects,verbs=create;update,versions=v1alpha1,name=vproject-attribution.ksquad.io,admissionReviewVersions=v1

// Project is the Schema for the projects API — a repo + workspace
// (arch §5.1). It is namespaced by default.
type Project struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProjectSpec   `json:"spec,omitempty"`
	Status ProjectStatus `json:"status,omitempty"`
}

// ProjectStatus is the observed state of a Project. The repo-sync
// reconciler (story 11.1) is its only writer: status reports mirror
// liveness — never desired state, and never coordination custody (§6).
type ProjectStatus struct {
	// Conditions summarize the repo-sync loop's latest observations
	// (story 11.1): SyncReady when the provider seam + BYO credential
	// resolved and the last level-triggered mirror pass applied.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Sync reports repo-sync mirror progress (§5.4, story 11.1). Nil
	// until the first successful mirror pass.
	// +optional
	Sync *ProjectSyncStatus `json:"sync,omitempty"`
}

// ProjectSyncStatus is the repo-sync slice of Project.status (§5.4).
// Every field is a read-only observation of the inbound mirror loop —
// the fenced coordination record (§6) stays authoritative and nothing
// here is control input.
type ProjectSyncStatus struct {
	// LastMirrorTime is when the level-triggered reconcile last applied
	// a provider snapshot to the scm mirror (webhook-triggered or poll).
	// +optional
	LastMirrorTime *metav1.Time `json:"lastMirrorTime,omitempty"`

	// LastWebhookTime is when a good-signature webhook last triggered a
	// reconcile fast path (§5.4). Unaffected by poll ticks.
	// +optional
	LastWebhookTime *metav1.Time `json:"lastWebhookTime,omitempty"`

	// MirrorRecordCount is the number of records the last snapshot
	// applied (post echo-suppression) — a liveness signal, not a
	// coordination fact.
	// +optional
	MirrorRecordCount int64 `json:"mirrorRecordCount,omitempty"`
}

// +kubebuilder:object:root=true

// ProjectList contains a list of Project.
type ProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Project `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Project{}, &ProjectList{})
}
