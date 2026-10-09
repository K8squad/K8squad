// Package issuetrigger is the ISI-5595 WS-B system-initiated issue-triage
// dispatch body. It is the generalization of pkg/controller/reviewtrigger to the
// GitHub Issues section: for each just-applied issue mirror row the repo-sync
// reconciler hands over, the Dispatcher decides whether triage is due under the
// Project's standing IssueTriage policy and, if so, mints an internal ticket and
// dispatches it to the configured triage agent under a SYSTEM identity.
//
// Governance (D1, ISI-4711): issue triage is a human-configured standing policy
// executed under a SYSTEM identity — NOT agent-initiated dispatch. The human
// authorizing act is spec.Enabled=true, provenanced by spec.EnabledBy; the coord
// dispatch is stamped Initiator="system:issue-triage" so it is never spoofed as a
// human or an agent dispatch. This package computes the qualification + dedup
// decision (all pure and unit-tested here); the actual work-item create + coord
// dispatch — the ISI-4711 custody-wall-sensitive write — lives entirely behind
// the TriageItemStore seam, which the operator composition root binds to a
// SYSTEM-identity adapter.
//
// Dedup / convergence with the manual bridge: the dedup key is the SAME join
// label the manual issue→ticket bridge uses — ksquad.github.issue=owner/repo#N
// (internal/apiserver/githubissuedispatch.go). EnsureTriage is create-if-absent
// on that label, so an auto-triaged issue and a manually-assigned one converge on
// ONE ticket and are never double-dispatched; a redelivered webhook or a poll
// tick that re-runs against the same snapshot is a no-op.
package issuetrigger

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/K8squad/K8squad/pkg/scm"
)

// Initiator is the coord dispatch provenance every issue-triage dispatch is
// stamped with (pkg/coord RequestDispatchInput.Initiator). Like
// reviewtrigger.Initiator it is deliberately neither "human" nor "agent": a
// triage dispatch is executed by the system on behalf of the human standing
// policy, so it carries its own third provenance value.
const Initiator = "system:issue-triage"

// AnchorLabelPrefix is the plaintext join label shared with the manual
// issue→ticket bridge (ISI-4757: ksquad.github.issue=owner/repo#N). It is BOTH
// the dedup/idempotency key for the triage create AND the join key the console
// local-agent badge reads back — so auto triage and manual assignment share one
// ticket per issue.
const AnchorLabelPrefix = "ksquad.github.issue="

// Policy is a resolved snapshot of a Project's IssueTriageSpec plus the coord
// scope the dispatch binds to (project id + owning team id). It is a value type
// owned by this package so the pure decision logic never imports the CRD types;
// the operator adapter maps *ksquadv1.IssueTriageSpec (and the Project's coord
// ids) onto it. A nil *Policy — or one with Enabled=false — disables triage for
// that Project entirely.
type Policy struct {
	Enabled bool
	// TriageAgentID is the agent the triage work item is dispatched to. Required
	// whenever Enabled is true.
	TriageAgentID string
	// LabelFilter restricts triage to issues carrying at least one of these
	// labels (case-insensitive). Empty ⇒ no label restriction.
	LabelFilter []string
	// OnlyUnassigned, when true, skips issues that already carry a GitHub
	// assignee. Resolved by the adapter from IssueTriageSpec.EffectiveOnlyUnassigned.
	OnlyUnassigned bool
	// EnabledBy is the server-stamped human principal that last enabled the policy
	// — the D1 authorizing-act provenance threaded into the dispatch as the
	// Principal. Required whenever Enabled is true.
	EnabledBy string
	// EnabledAt is the server-stamped instant the policy was last enabled — the D4
	// forward-only watermark. Only issues created/updated at/after it qualify.
	// Required (non-zero) whenever Enabled is true.
	EnabledAt time.Time
	// ProjectID / TeamID are the coord scope the created work item and its
	// dispatch belong to.
	ProjectID string
	TeamID    string
}

// PolicyReader resolves the standing triage policy for one Project. A nil *Policy
// return means "no policy configured" and is treated exactly like a disabled one:
// the reconcile pass is a no-op for that Project.
type PolicyReader interface {
	TriagePolicy(ctx context.Context, projectNamespace, projectName string) (*Policy, error)
}

// TriageRequest is one qualified, deduplicated issue-triage dispatch the store
// must realize. Every field is derived from the policy + the mirror row by the
// pure decision logic; the store never re-decides eligibility.
type TriageRequest struct {
	// DedupLabel is the idempotency key as a coord work-item label:
	// ksquad.github.issue=owner/repo#N — the SAME label the manual bridge uses, so
	// the store's create-if-absent converges auto + manual onto one ticket. The
	// store MUST create-if-absent keyed on this label.
	DedupLabel string
	// ProjectID / TeamID scope the created work item; TriageAgentID is the agent
	// the dispatch binds to; Principal is the human EnabledBy provenance.
	ProjectID     string
	TeamID        string
	TriageAgentID string
	Principal     string
	// IssueRef / IssueURL describe the issue under triage, for the work item
	// title/body.
	IssueRef string
	IssueURL string
	Title    string
}

// TriageItemStore is the custody-wall-sensitive seam: it owns the create-if-
// absent of the triage work item and its dispatch to the triage agent under the
// SYSTEM identity. The Dispatcher only ever asks it to Ensure a request it has
// already qualified and deduplicated; the store's own create must remain
// idempotent on TriageRequest.DedupLabel so two racing reconciles cannot
// double-create.
type TriageItemStore interface {
	// EnsureTriage creates the triage work item (if none carries req.DedupLabel
	// yet) and dispatches it to req.TriageAgentID under Initiator. It returns
	// created=false when the label already existed, so a caller can distinguish a
	// fresh dispatch from an idempotent no-op. It MUST stamp the coord dispatch
	// Initiator = issuetrigger.Initiator and the Principal = req.Principal (the
	// human EnabledBy), never an agent identity.
	EnsureTriage(ctx context.Context, req TriageRequest) (created bool, err error)
}

// Dispatcher implements reposync.IssueTriageTrigger. Construct it with a
// PolicyReader and a TriageItemStore; both are required.
type Dispatcher struct {
	Policy PolicyReader
	Store  TriageItemStore
}

// TriageChanges implements reposync.IssueTriageTrigger. It is level-triggered and
// idempotent: it derives the decision purely from the just-applied rows and the
// standing policy, with dedup delegated to the store keyed on the per-issue
// label — there is no stored diff state, so re-running it on an unchanged
// snapshot is a no-op. A failure on any row fails the whole pass so the next
// reconcile retries against the re-applied mirror.
func (d *Dispatcher) TriageChanges(ctx context.Context, projectNamespace, projectName string, _ scm.SourceProvider, repoURL string, rows []scm.MirrorRow) error {
	pol, err := d.Policy.TriagePolicy(ctx, projectNamespace, projectName)
	if err != nil {
		return fmt.Errorf("issuetrigger: resolve policy for %s/%s: %w", projectNamespace, projectName, err)
	}
	if pol == nil || !pol.Enabled {
		return nil // no standing policy / disabled ⇒ never trigger (D1)
	}
	if pol.TriageAgentID == "" {
		// An enabled policy with no agent is a misconfiguration the apiserver
		// rejects on write; fail loudly rather than dispatch to "".
		return fmt.Errorf("issuetrigger: %s/%s: policy enabled but TriageAgentID is empty", projectNamespace, projectName)
	}

	for _, row := range rows {
		req, ok, err := qualify(pol, repoURL, row)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if _, err := d.Store.EnsureTriage(ctx, req); err != nil {
			return fmt.Errorf("issuetrigger: ensure triage for %s: %w", req.IssueRef, err)
		}
	}
	return nil
}

// qualify applies the pure per-row decision: is this an open issue the configured
// filters select, created/updated at/after the forward-only watermark? If so it
// returns the fully-formed, deduplicated TriageRequest. Non-issue rows, non-open
// issues, label/assignee-filtered issues, and backlog (pre-enabledAt) issues
// return ok=false.
func qualify(pol *Policy, repoURL string, row scm.MirrorRow) (TriageRequest, bool, error) {
	if row.Kind != scm.RecordTypeIssue {
		return TriageRequest{}, false, nil
	}
	// Only open issues are triageable — a closed issue has nothing to pick up.
	if !strings.EqualFold(row.State, "open") {
		return TriageRequest{}, false, nil
	}

	payload, err := decodePayload(row)
	if err != nil {
		return TriageRequest{}, false, fmt.Errorf("issuetrigger: decode issue payload for %s: %w", row.ExternalID, err)
	}

	// D4 forward-only: only issues whose GitHub created_at OR updated_at is
	// at/after the enablement watermark qualify, so first-enable never floods the
	// squad with the entire open-issue backlog. An issue with unknown (zero)
	// timestamps fails safe (does NOT qualify) rather than be treated as fresh.
	if !atOrAfter(payload.CreatedAt, pol.EnabledAt) && !atOrAfter(payload.UpdatedAt, pol.EnabledAt) {
		return TriageRequest{}, false, nil
	}

	// onlyUnassigned (default): skip issues a human already picked up upstream.
	if pol.OnlyUnassigned && len(payload.Assignees) > 0 {
		return TriageRequest{}, false, nil
	}

	// Label filter: triage only issues carrying at least one configured label.
	if len(pol.LabelFilter) > 0 && !matchesAnyLabel(payload.Labels, pol.LabelFilter) {
		return TriageRequest{}, false, nil
	}

	issueRef := deriveIssueRef(payload.URL, repoURL, row.ExternalID, payload.Number)
	if issueRef == "" {
		// No owner/repo#N could be derived — we refuse to mint a ticket with a
		// label we can't tie to a real issue (keeps the join honest).
		return TriageRequest{}, false, nil
	}

	return TriageRequest{
		DedupLabel:    AnchorLabelPrefix + issueRef,
		ProjectID:     pol.ProjectID,
		TeamID:        pol.TeamID,
		TriageAgentID: pol.TriageAgentID,
		Principal:     pol.EnabledBy,
		IssueRef:      issueRef,
		IssueURL:      payload.URL,
		Title:         triageTitle(issueRef, row.Title),
	}, true, nil
}

func decodePayload(row scm.MirrorRow) (scm.MirrorPayload, error) {
	var payload scm.MirrorPayload
	if len(row.Payload) == 0 {
		return payload, nil
	}
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		return scm.MirrorPayload{}, err
	}
	return payload, nil
}

// atOrAfter reports whether t is non-zero and not before the watermark. A zero t
// (unknown timestamp) is never at/after a real watermark — the fail-safe stance
// for the forward-only guard.
func atOrAfter(t, watermark time.Time) bool {
	if t.IsZero() {
		return false
	}
	return !t.Before(watermark)
}

// matchesAnyLabel reports whether issueLabels contains at least one of filter
// (case-insensitive). filter is assumed non-empty.
func matchesAnyLabel(issueLabels, filter []string) bool {
	if len(issueLabels) == 0 {
		return false
	}
	want := make(map[string]struct{}, len(filter))
	for _, f := range filter {
		want[strings.ToLower(strings.TrimSpace(f))] = struct{}{}
	}
	for _, l := range issueLabels {
		if _, ok := want[strings.ToLower(strings.TrimSpace(l))]; ok {
			return true
		}
	}
	return false
}

// ghIssueURLRe pulls owner/repo and the issue number out of a canonical GitHub
// issue URL (https://github.com/owner/repo/issues/123) — the SAME pattern the
// manual bridge (githubissuedispatch.go) uses, so the derived ref/casing matches
// and the two converge on one ticket.
var ghIssueURLRe = regexp.MustCompile(`github\.com/([^/]+/[^/]+)/issues/(\d+)`)

// deriveIssueRef builds the compact owner/repo#N ref for the label. It prefers
// the mirrored issue URL (canonical casing from GitHub, matching the manual
// bridge exactly); if that is absent/unparseable it falls back to the Project's
// repo URL slug plus the issue number. Returns "" when neither yields a usable
// owner/repo#N.
func deriveIssueRef(issueURL, repoURL, externalID string, payloadNumber int) string {
	if m := ghIssueURLRe.FindStringSubmatch(issueURL); m != nil {
		return m[1] + "#" + m[2]
	}
	slug := repoSlug(repoURL)
	num := issueNumber(externalID, payloadNumber)
	if slug == "" || num == "" {
		return ""
	}
	return slug + "#" + num
}

// repoSlug extracts owner/repo from a repo URL, tolerating a trailing slash and a
// .git suffix. Casing is preserved (NOT lowercased) so the fallback label matches
// the manual bridge, which derives from the canonical GitHub URL. Returns "" when
// two path segments cannot be found.
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
	return owner + "/" + repo
}

// issueNumber resolves the issue number as a string, preferring the indexed
// ExternalID and falling back to the payload Number.
func issueNumber(externalID string, payloadNumber int) string {
	if n := strings.TrimSpace(externalID); n != "" {
		return n
	}
	if payloadNumber > 0 {
		return fmt.Sprintf("%d", payloadNumber)
	}
	return ""
}

func triageTitle(issueRef, issueTitle string) string {
	if strings.TrimSpace(issueTitle) == "" {
		return "Triage GitHub issue " + issueRef
	}
	return fmt.Sprintf("Triage %s: %s", issueRef, issueTitle)
}
