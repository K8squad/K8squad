// Package cifailure is the ISI-5595 WS-C system-initiated CI-failure triage
// dispatch body. It implements the reposync.CIFailureTrigger seam: for each
// just-applied check-run mirror row the repo-sync reconciler hands over, the
// Dispatcher decides whether a failing CI run is due for triage under the
// Project's standing CiFailure policy and, if so, creates a triage work item and
// dispatches it to the configured agent under a SYSTEM identity.
//
// Governance (mirrors ISI-4750 reviewtrigger): CI-failure triage is a
// human-configured standing policy executed under a SYSTEM identity — NOT
// agent-initiated dispatch. The human authorizing act is spec.Enabled=true,
// provenanced by spec.EnabledBy; the coord dispatch is stamped
// Initiator="system:ci-failure" so it is never spoofed as a human or an agent
// dispatch. This package computes the qualification + dedup decision (all pure
// and unit-tested here); the actual work-item create + coord dispatch — the
// ISI-4711 custody-wall-sensitive write — lives entirely behind the
// ItemStore seam, which the operator composition root binds to a
// SYSTEM-identity adapter.
//
// Scope (board-approved D3/D4): it consumes the EXISTING check-run mirror
// (RecordTypeCheckRun, Conclusion) — no workflow_run mirroring is added — and is
// forward-only from the policy's EnabledAt watermark, so enabling never
// retroactively triages failures already in the mirror.
package cifailure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/K8squad/K8squad/pkg/scm"
)

// Initiator is the coord dispatch provenance every CI-failure triage dispatch is
// stamped with (pkg/coord RequestDispatchInput.Initiator). Like the review
// trigger's, it is deliberately neither "human" (the board default) nor "agent"
// (the ADR-0024 authoring lane): a triage dispatch is executed by the system on
// behalf of the human standing policy, so it carries its own third provenance.
const Initiator = "system:ci-failure"

// Policy is a resolved snapshot of a Project's CiFailureSpec plus the coord scope
// the dispatch binds to (project id + owning team id). It is a value type owned
// by this package so the pure decision logic never imports the CRD types; the
// operator adapter maps *ksquadv1.CiFailureSpec (and the Project's coord ids)
// onto it. A nil *Policy — or one with Enabled=false — disables triage entirely.
type Policy struct {
	Enabled bool
	// AgentID is the team agent the triage ticket is dispatched to.
	AgentID string
	// Conclusions is the EFFECTIVE set of qualifying check-run conclusions
	// (already defaulted to ["failure"] by the adapter via
	// CiFailureSpec.EffectiveConclusions). Matched case-insensitively.
	Conclusions []string
	// BranchFilter narrows qualifying refs by check-suite head branch. Empty ⇒
	// accept every mirrored check-run ref (the current check-run scope, D3).
	BranchFilter []string
	// EnabledBy is the server-stamped human principal that last enabled the
	// policy — the authorizing-act provenance threaded into the dispatch as the
	// Principal. Required whenever Enabled is true.
	EnabledBy string
	// EnabledAt is the server-stamped forward-only watermark (D4): only check
	// runs that completed at/after it are triaged. Required whenever Enabled.
	EnabledAt time.Time
	// ProjectID / TeamID are the coord scope the created work item and its
	// dispatch belong to.
	ProjectID string
	TeamID    string
}

// PolicyReader resolves the standing CI-failure policy for one Project. A nil
// *Policy return means "no policy configured" and is treated exactly like a
// disabled one: the reconcile pass is a no-op for that Project.
type PolicyReader interface {
	CIFailurePolicy(ctx context.Context, projectNamespace, projectName string) (*Policy, error)
}

// FailureRequest is one qualified, deduplicated CI-failure triage dispatch the
// store must realize. Every field is derived from the policy + the mirror row by
// the pure decision logic; the store never re-decides eligibility.
type FailureRequest struct {
	// DedupLabel is the idempotency key as a coord work-item label (<=64 chars):
	// a stable hash of (repo, check name, head SHA, conclusion). The store MUST
	// create-if-absent keyed on this label so a redelivered webhook or a poll
	// tick that re-runs against the same snapshot is a no-op. A re-run on the SAME
	// head SHA keeps the same label (dedup); a new failing SHA yields a new one.
	DedupLabel string
	// ProjectID / TeamID scope the created work item; AgentID is the agent the
	// dispatch binds to; Principal is the human EnabledBy provenance.
	ProjectID string
	TeamID    string
	AgentID   string
	Principal string
	// RepoURL / CheckName / HeadSHA / Conclusion describe the failing run, for
	// the work item title/body.
	RepoURL    string
	CheckName  string
	HeadSHA    string
	Conclusion string
	Title      string
}

// ItemStore is the custody-wall-sensitive seam: it owns the create-if-
// absent of the triage work item and its dispatch to the configured agent under
// the SYSTEM identity. The Dispatcher only ever asks it to Ensure a request it
// has already qualified and deduplicated; the store's own create must remain
// idempotent on FailureRequest.DedupLabel so two racing reconciles cannot
// double-create.
type ItemStore interface {
	EnsureCIFailure(ctx context.Context, req FailureRequest) (created bool, err error)
}

// Dispatcher implements reposync.CIFailureTrigger. Construct it with a
// PolicyReader and a ItemStore; both are required. Unlike the review
// trigger it needs no TeamMembership resolver — CI-failure triage has no
// author-scope question, it keys off the repo's own check runs.
type Dispatcher struct {
	Policy PolicyReader
	Store  ItemStore
}

// HandleCIFailures implements reposync.CIFailureTrigger. It is level-triggered and
// idempotent: it derives the decision purely from the just-applied rows and the
// standing policy, with dedup delegated to the store keyed on the per-(check,SHA)
// label — there is no stored diff state, so re-running it on an unchanged
// snapshot is a no-op. A failure on any row fails the whole pass so the next
// reconcile retries against the re-applied mirror.
func (d *Dispatcher) HandleCIFailures(ctx context.Context, projectNamespace, projectName string, _ scm.SourceProvider, repoURL string, rows []scm.MirrorRow) error {
	pol, err := d.Policy.CIFailurePolicy(ctx, projectNamespace, projectName)
	if err != nil {
		return fmt.Errorf("cifailure: resolve policy for %s/%s: %w", projectNamespace, projectName, err)
	}
	if pol == nil || !pol.Enabled {
		return nil // no standing policy / disabled ⇒ never trigger
	}
	if pol.AgentID == "" {
		// An enabled policy with no agent is a misconfiguration the apiserver
		// rejects on write; fail loudly rather than dispatch to "".
		return fmt.Errorf("cifailure: %s/%s: policy enabled but AgentID is empty", projectNamespace, projectName)
	}

	for _, row := range rows {
		req, ok, err := d.qualify(pol, repoURL, row)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if _, err := d.Store.EnsureCIFailure(ctx, req); err != nil {
			return fmt.Errorf("cifailure: ensure triage for %s %q@%s: %w", repoURL, req.CheckName, req.HeadSHA, err)
		}
	}
	return nil
}

// qualify applies the pure per-row decision: is this a completed check run whose
// conclusion is in the configured failure set, on an in-scope ref, past the
// forward-only watermark? If so it returns the fully-formed, deduplicated
// FailureRequest. Everything else returns ok=false.
func (d *Dispatcher) qualify(pol *Policy, repoURL string, row scm.MirrorRow) (FailureRequest, bool, error) {
	if row.Kind != scm.RecordTypeCheckRun {
		return FailureRequest{}, false, nil
	}

	var payload scm.MirrorPayload
	if len(row.Payload) > 0 {
		if err := json.Unmarshal(row.Payload, &payload); err != nil {
			return FailureRequest{}, false, fmt.Errorf("cifailure: decode check-run payload for %s %q: %w", repoURL, row.ExternalID, err)
		}
	}

	// Conclusion gate (D3): only terminal, configured-failure conclusions
	// qualify. A queued/in-progress run has an empty Conclusion and never
	// matches, so we never triage a run that has not finished.
	if payload.Conclusion == "" || !containsFold(pol.Conclusions, payload.Conclusion) {
		return FailureRequest{}, false, nil
	}

	// Forward-only watermark (D4): only runs that completed at/after EnabledAt are
	// triaged. The check-run row's UpdatedAt is GetCompletedAt (set whenever the
	// conclusion is set); fall back to CreatedAt (GetStartedAt). A run we cannot
	// time is skipped rather than retroactively triaged.
	completed := payload.UpdatedAt
	if completed.IsZero() {
		completed = payload.CreatedAt
	}
	if completed.IsZero() || completed.Before(pol.EnabledAt) {
		return FailureRequest{}, false, nil
	}

	// Branch scope. Empty filter ⇒ accept every mirrored ref (the mirror already
	// scopes check runs to the default branch + open-PR heads, D3). A non-empty
	// filter requires a known head branch that matches (fail-closed narrowing).
	if len(pol.BranchFilter) > 0 {
		if payload.HeadRef == "" || !containsFold(pol.BranchFilter, payload.HeadRef) {
			return FailureRequest{}, false, nil
		}
	}

	checkName := row.Title
	return FailureRequest{
		DedupLabel: dedupLabel(repoURL, checkName, payload.HeadSHA, payload.Conclusion),
		ProjectID:  pol.ProjectID,
		TeamID:     pol.TeamID,
		AgentID:    pol.AgentID,
		Principal:  pol.EnabledBy,
		RepoURL:    repoURL,
		CheckName:  checkName,
		HeadSHA:    payload.HeadSHA,
		Conclusion: payload.Conclusion,
		Title:      failureTitle(checkName, payload.HeadSHA),
	}, true, nil
}

// containsFold reports whether set contains s case-insensitively.
func containsFold(set []string, s string) bool {
	for _, v := range set {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// dedupLabelPrefix is the stable prefix of every CI-failure dedup label, so an
// operator (or a FindWorkItemByLabel query) can recognize triage items.
const dedupLabelPrefix = "ksquad.github.ci="

// dedupLabel builds the (<=64-char) coord work-item label the triage dispatch
// dedups on. The canonical key is repo + check name + head SHA + conclusion;
// because that overruns maxLabelLen, it is SHA-256 hashed and hex-encoded (32
// chars) behind a stable prefix. Keying on the head SHA means a re-run of the
// SAME failing commit dedups to one ticket, while a new failing SHA (or a
// different check / conclusion) yields a distinct label and thus a fresh ticket.
func dedupLabel(repoURL, checkName, headSHA, conclusion string) string {
	key := strings.Join([]string{repoURL, checkName, headSHA, conclusion}, "\x00")
	sum := sha256.Sum256([]byte(key))
	return dedupLabelPrefix + hex.EncodeToString(sum[:16])
}

// shortSHA trims a commit SHA to its first 12 chars for human-legible titles,
// leaving shorter/empty values untouched.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// failureTitle is the triage work-item title: "Analyze CI failure: <check> @ <sha>".
func failureTitle(checkName, headSHA string) string {
	name := checkName
	if name == "" {
		name = "check"
	}
	if s := shortSHA(headSHA); s != "" {
		return fmt.Sprintf("Analyze CI failure: %s @ %s", name, s)
	}
	return fmt.Sprintf("Analyze CI failure: %s", name)
}
