// Package reviewtrigger is the ISI-4750 E4 system-initiated PR-review dispatch
// body. It implements the reposync.ReviewTrigger seam (introduced by E3): for
// each just-applied PR mirror row the repo-sync reconciler hands over, the
// Dispatcher decides whether a review is due under the Project's standing
// ReviewAutomation policy (E1 config) and, if so, creates a PR-review work item
// and dispatches it to the configured reviewer agent under a SYSTEM identity.
//
// D1 governance (ISI-4750, confirmation 567be9e1): review dispatch is a
// human-configured standing policy executed under a SYSTEM identity — NOT
// agent-initiated dispatch. The human authorizing act is spec.Enabled=true,
// provenanced by spec.EnabledBy; the coord dispatch is stamped
// Initiator="system:review-automation" so it is never spoofed as a human or an
// agent dispatch. This package computes the qualification + dedup decision (all
// pure and unit-tested here); the actual work-item create + coord dispatch —
// the ISI-4711 custody-wall-sensitive write — lives entirely behind the
// ReviewItemStore seam, which the operator composition root binds to a
// SYSTEM-identity adapter.
package reviewtrigger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/K8squad/K8squad/pkg/scm"
)

// Initiator is the coord dispatch provenance every review dispatch is stamped
// with (pkg/coord RequestDispatchInput.Initiator). It is deliberately neither
// "human" (the board default) nor "agent" (the ADR-0024 authoring lane): a
// review dispatch is executed by the system on behalf of the human standing
// policy, so it carries its own third provenance value.
const Initiator = "system:review-automation"

// Policy is a resolved snapshot of a Project's ReviewAutomationSpec plus the
// coord scope the dispatch binds to (project id + owning team id). It is a
// value type owned by this package so the pure decision logic never imports the
// CRD types; the operator adapter maps *ksquadv1.ReviewAutomationSpec (and the
// Project's coord ids) onto it. A nil *Policy — or one with Enabled=false —
// disables review for that Project entirely.
type Policy struct {
	Enabled         bool
	ReviewerAgentID string
	// Scope is scm review scope: scm review "team_authored" (default) reviews
	// only PRs whose actor is a team agent; "all" reviews every PR. Empty ⇒
	// team_authored, matching ReviewAutomationSpec.EffectiveScope.
	Scope string
	// Trigger is "on_open" (review a PR once) or "on_new_commits" (default:
	// also re-review when the head SHA changes). Empty ⇒ on_new_commits,
	// matching ReviewAutomationSpec.EffectiveTrigger.
	Trigger string
	// EnabledBy is the server-stamped human principal that last enabled the
	// policy — the D1 authorizing-act provenance threaded into the dispatch as
	// the Principal. Required whenever Enabled is true.
	EnabledBy string
	// ProjectID / TeamID are the coord scope the created work item and its
	// dispatch belong to.
	ProjectID string
	TeamID    string
}

// Review scope / trigger vocabulary, kept identical to the ksquadv1 constants so
// the adapter can pass the CRD value through unchanged.
const (
	scopeTeamAuthored = "team_authored"
	scopeAll          = "all"

	triggerOnOpen       = "on_open"
	triggerOnNewCommits = "on_new_commits"
)

func (p *Policy) effectiveScope() string {
	if p == nil || p.Scope == "" {
		return scopeTeamAuthored
	}
	return p.Scope
}

func (p *Policy) effectiveTrigger() string {
	if p == nil || p.Trigger == "" {
		return triggerOnNewCommits
	}
	return p.Trigger
}

// PolicyReader resolves the standing review policy for one Project. A nil
// *Policy return means "no policy configured" and is treated exactly like a
// disabled one: the reconcile pass is a no-op for that Project.
type PolicyReader interface {
	ReviewPolicy(ctx context.Context, projectNamespace, projectName string) (*Policy, error)
}

// TeamMembership answers the scope=team_authored question: is the PR's mirror
// actor an agent in the owning Team's composition? Only consulted when the
// resolved scope is team_authored.
type TeamMembership interface {
	IsTeamAgent(ctx context.Context, teamID, actor string) (bool, error)
}

// ReviewRequest is one qualified, deduplicated PR-review dispatch the store must
// realize. Every field is derived from the policy + the mirror row by the pure
// decision logic; the store never re-decides eligibility.
type ReviewRequest struct {
	// DedupLabel is the idempotency key as a coord work-item label (<=64 chars,
	// see maxLabelLen): a stable hash of (repo, PR number[, head SHA]). The
	// store MUST create-if-absent keyed on this label so a redelivered webhook
	// or a poll tick that re-runs against the same snapshot is a no-op.
	DedupLabel string
	// ProjectID / TeamID scope the created work item; ReviewerAgentID is the
	// agent the dispatch binds to; Principal is the human EnabledBy provenance.
	ProjectID       string
	TeamID          string
	ReviewerAgentID string
	Principal       string
	// RepoURL / PRNumber / HeadSHA describe the PR under review, for the work
	// item title/body and the review payload.
	RepoURL  string
	PRNumber string
	HeadSHA  string
	Title    string
}

// ReviewItemStore is the custody-wall-sensitive seam: it owns the create-if-
// absent of the PR-review work item and its dispatch to the reviewer agent
// under the SYSTEM identity. The Dispatcher only ever asks it to Ensure a
// request it has already qualified and deduplicated; the store's own create
// must remain idempotent on ReviewRequest.DedupLabel so two racing reconciles
// cannot double-create.
type ReviewItemStore interface {
	// EnsureReview creates the PR-review work item (if none carries
	// req.DedupLabel yet) and dispatches it to req.ReviewerAgentID under
	// Initiator. It returns created=false when the label already existed, so a
	// caller can distinguish a fresh dispatch from an idempotent no-op. It MUST
	// stamp the coord dispatch Initiator = reviewtrigger.Initiator and the
	// Principal = req.Principal (the human EnabledBy), never an agent identity.
	EnsureReview(ctx context.Context, req ReviewRequest) (created bool, err error)
}

// Dispatcher implements reposync.ReviewTrigger. Construct it with a PolicyReader,
// a TeamMembership resolver, and a ReviewItemStore; all three are required.
type Dispatcher struct {
	Policy  PolicyReader
	Members TeamMembership
	Store   ReviewItemStore
}

// ReviewChanges implements reposync.ReviewTrigger. It is level-triggered and
// idempotent: it derives the decision purely from the just-applied rows and the
// standing policy, with dedup delegated to the store keyed on the per-PR label —
// there is no stored diff state, so re-running it on an unchanged snapshot is a
// no-op. A failure on any row fails the whole pass so the next reconcile retries
// against the re-applied mirror.
func (d *Dispatcher) ReviewChanges(ctx context.Context, projectNamespace, projectName string, _ scm.SourceProvider, repoURL string, rows []scm.MirrorRow) error {
	pol, err := d.Policy.ReviewPolicy(ctx, projectNamespace, projectName)
	if err != nil {
		return fmt.Errorf("reviewtrigger: resolve policy for %s/%s: %w", projectNamespace, projectName, err)
	}
	if pol == nil || !pol.Enabled {
		return nil // no standing policy / disabled ⇒ never trigger (D1)
	}
	if pol.ReviewerAgentID == "" {
		// An enabled policy with no reviewer is a misconfiguration the
		// apiserver rejects on write; fail loudly rather than dispatch to "".
		return fmt.Errorf("reviewtrigger: %s/%s: policy enabled but ReviewerAgentID is empty", projectNamespace, projectName)
	}

	for _, row := range rows {
		req, ok, err := d.qualify(ctx, pol, repoURL, row)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if _, err := d.Store.EnsureReview(ctx, req); err != nil {
			return fmt.Errorf("reviewtrigger: ensure review for %s#%s: %w", repoURL, req.PRNumber, err)
		}
	}
	return nil
}

// qualify applies the pure per-row decision: is this an open PR that the
// configured scope selects? If so it returns the fully-formed, deduplicated
// ReviewRequest. Non-PR rows, non-open PRs, and out-of-scope PRs return ok=false.
func (d *Dispatcher) qualify(ctx context.Context, pol *Policy, repoURL string, row scm.MirrorRow) (ReviewRequest, bool, error) {
	if row.Kind != scm.RecordTypePR {
		return ReviewRequest{}, false, nil
	}
	// Only open PRs are reviewable — a closed/merged PR has nothing to gate.
	if !strings.EqualFold(row.State, "open") {
		return ReviewRequest{}, false, nil
	}

	// Scope filter (D3). team_authored (default) reviews only PRs authored by a
	// team agent; all reviews every PR.
	if pol.effectiveScope() != scopeAll {
		if row.Actor == "" {
			return ReviewRequest{}, false, nil // unknown author can't be a team agent
		}
		member, err := d.Members.IsTeamAgent(ctx, pol.TeamID, row.Actor)
		if err != nil {
			return ReviewRequest{}, false, fmt.Errorf("reviewtrigger: team membership check for %q: %w", row.Actor, err)
		}
		if !member {
			return ReviewRequest{}, false, nil
		}
	}

	var payload scm.MirrorPayload
	if len(row.Payload) > 0 {
		if err := json.Unmarshal(row.Payload, &payload); err != nil {
			return ReviewRequest{}, false, fmt.Errorf("reviewtrigger: decode PR payload for %s#%s: %w", repoURL, row.ExternalID, err)
		}
	}
	// on_new_commits (default) requires the E3 head-SHA enrichment: without a
	// head SHA there is no change-detection key, so skip rather than review the
	// same head repeatedly under a SHA-less label.
	if pol.effectiveTrigger() == triggerOnNewCommits && payload.HeadSHA == "" {
		return ReviewRequest{}, false, nil
	}

	return ReviewRequest{
		DedupLabel:      dedupLabel(pol.effectiveTrigger(), repoURL, row.ExternalID, payload.HeadSHA),
		ProjectID:       pol.ProjectID,
		TeamID:          pol.TeamID,
		ReviewerAgentID: pol.ReviewerAgentID,
		Principal:       pol.EnabledBy,
		RepoURL:         repoURL,
		PRNumber:        row.ExternalID,
		HeadSHA:         payload.HeadSHA,
		Title:           reviewTitle(repoURL, row.ExternalID, row.Title),
	}, true, nil
}

// dedupLabelPrefix is the stable prefix of every review-automation dedup label,
// so an operator (or a FindWorkItemByLabel query) can recognize review items.
const dedupLabelPrefix = "ksquad.review="

// dedupLabel builds the (<=64-char) coord work-item label the review dispatch
// dedups on. The canonical key is repo#number[@headSHA]; because a 40-char SHA
// plus a repo URL overruns maxLabelLen, the key is SHA-256 hashed and hex-
// encoded (32 chars) behind a stable prefix. For trigger=on_open the head SHA is
// excluded so a PR is reviewed exactly once regardless of new commits; for
// on_new_commits it is included so each new head yields a distinct label and
// thus a fresh review.
func dedupLabel(trigger, repoURL, prNumber, headSHA string) string {
	key := repoURL + "#" + prNumber
	if trigger != triggerOnOpen {
		key += "@" + headSHA
	}
	sum := sha256.Sum256([]byte(key))
	return dedupLabelPrefix + hex.EncodeToString(sum[:16])
}

// AnchorLabelPrefix is the plaintext, human-legible label that joins a PR-review
// work item back to the pull request it reviews. Unlike the opaque dedup label
// (dedupLabelPrefix), it is a stable, greppable key the console read model
// (ISI-4767 E5) looks up to surface "reviewed" on the PR card. It mirrors the
// ISI-4757 GitHub-issue bridge anchor form (ksquad.github.issue=<owner>/<repo>#N).
const AnchorLabelPrefix = "ksquad.github.pr="

// PRAnchorLabel builds the plaintext PR-review anchor label for (repoURL, PR
// number): ksquad.github.pr=<owner>/<repo>#N. owner/repo is lower-cased so the
// producer (the review dispatch store) and the read-model consumer (apiserver
// githubstatus) always agree regardless of how GitHub cases the slug — the
// single shared normalization is the join invariant. It returns "" when
// owner/repo cannot be extracted from repoURL (or prNumber is empty): an
// unrecognized URL yields no anchor rather than a malformed one.
func PRAnchorLabel(repoURL, prNumber string) string {
	slug := repoSlug(repoURL)
	if slug == "" || prNumber == "" {
		return ""
	}
	return AnchorLabelPrefix + slug + "#" + prNumber
}

// repoSlug extracts the lower-cased <owner>/<repo> from a repo URL, tolerating a
// trailing slash and a .git suffix. Returns "" when two path segments cannot be
// found.
func repoSlug(repoURL string) string {
	s := strings.TrimSuffix(strings.TrimSpace(repoURL), "/")
	s = strings.TrimSuffix(s, ".git")
	if s == "" {
		return ""
	}
	parts := strings.Split(s, "/")
	if len(parts) < 2 {
		return ""
	}
	owner, repo := parts[len(parts)-2], parts[len(parts)-1]
	if owner == "" || repo == "" {
		return ""
	}
	return strings.ToLower(owner + "/" + repo)
}

func reviewTitle(repoURL, prNumber, prTitle string) string {
	repo := repoURL
	if i := strings.LastIndex(strings.TrimSuffix(repoURL, "/"), "/"); i >= 0 {
		if j := strings.LastIndex(repoURL[:i], "/"); j >= 0 {
			repo = repoURL[j+1:]
		}
	}
	repo = strings.TrimSuffix(repo, ".git")
	if prTitle == "" {
		return fmt.Sprintf("Review %s#%s", repo, prNumber)
	}
	return fmt.Sprintf("Review %s#%s: %s", repo, prNumber, prTitle)
}
