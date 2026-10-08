package issuedispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/K8squad/K8squad/pkg/controller/issuetrigger"
	"github.com/K8squad/K8squad/pkg/coord"
)

// triageItemWriter is the create-if-absent seam this store depends on — satisfied
// by *coord.WorkItemWriteStore. Narrowed to one method so the authz-critical
// dispatch logic is unit-testable with a fake, no Postgres.
type triageItemWriter interface {
	EnsureReviewWorkItem(ctx context.Context, in coord.EnsureReviewWorkItemInput) (coord.EnsureReviewWorkItemResult, error)
}

// triageItemDispatcher is the dispatch seam — satisfied by *coord.WorkItemDispatchStore.
type triageItemDispatcher interface {
	RequestDispatch(ctx context.Context, in coord.RequestDispatchInput) (coord.WorkItemDispatchResult, error)
}

// SystemTriageItemStore implements issuetrigger.TriageItemStore — the custody-
// wall-sensitive create + dispatch, executed under the SYSTEM identity. It is the
// ONLY place issue-triage automation touches coord's write surface; the pure
// dispatcher hands it an already-qualified, already-deduplicated TriageRequest
// and this store realizes it idempotently.
//
// Governance (D1, ISI-4711), identical posture to reviewdispatch.SystemReviewItemStore:
//   - the work item is authored under issuetrigger.Initiator ("system:issue-
//     triage"), NOT an agent — coord.EnsureReviewWorkItem is the plain store op,
//     not the agent-authoring lane, so no custody wall is crossed;
//   - the coord dispatch is stamped Initiator=issuetrigger.Initiator (system
//     provenance) and Principal=the human EnabledBy carried on the request — the
//     D1 authorizing-act provenance — never an agent identity.
//
// Dedup convergence with the manual bridge: because the dedup label IS the shared
// ksquad.github.issue=owner/repo#N join key, EnsureReviewWorkItem find-or-creates
// the SAME ticket a human manual-assign would, and the dispatch-only-while-backlog
// guard means an already-handled issue (manual OR a prior auto pass) is a no-op.
type SystemTriageItemStore struct {
	writer   triageItemWriter
	dispatch triageItemDispatcher
}

// NewSystemTriageItemStore binds the store to the coord write + dispatch ops.
// Both are required.
func NewSystemTriageItemStore(writer *coord.WorkItemWriteStore, dispatch *coord.WorkItemDispatchStore) (*SystemTriageItemStore, error) {
	if writer == nil {
		return nil, errors.New("issuedispatch: nil work-item write store")
	}
	if dispatch == nil {
		return nil, errors.New("issuedispatch: nil work-item dispatch store")
	}
	return &SystemTriageItemStore{writer: writer, dispatch: dispatch}, nil
}

// EnsureTriage creates the triage work item if none carries req.DedupLabel yet
// (create-if-absent, serialised in coord against concurrent reconciles) and
// dispatches it to req.TriageAgentID under the SYSTEM identity. It returns
// created=false when the label already existed.
//
// Self-heal: the create and the dispatch are separate coord txns. If a prior pass
// created the item but failed before dispatch, the item sits in 'backlog'; this
// pass finds it (created=false) and, because it is still in the entry lane,
// dispatches it. An item already advanced past 'backlog' was dispatched earlier
// (by a prior auto pass OR the manual bridge) — re-dispatching a claimed item
// would 409, so a benign ErrStateConflict is swallowed and the pass treats the
// issue as already handled.
func (s *SystemTriageItemStore) EnsureTriage(ctx context.Context, req issuetrigger.TriageRequest) (bool, error) {
	if req.Principal == "" {
		// The human EnabledBy provenance is mandatory for the D1 dispatch; the
		// policy reader already guards this, but fail closed here too rather than
		// author a triage the dispatch can't provenance.
		return false, fmt.Errorf("issuedispatch: triage request for %s has no Principal (EnabledBy) provenance", req.IssueRef)
	}

	in := coord.EnsureReviewWorkItemInput{
		ProjectID:  req.ProjectID,
		TeamID:     req.TeamID,
		Title:      req.Title,
		Body:       triageBody(req),
		DedupLabel: req.DedupLabel,
		Principal:  issuetrigger.Initiator, // SYSTEM author — never an agent
	}

	res, err := s.writer.EnsureReviewWorkItem(ctx, in)
	if err != nil {
		return false, fmt.Errorf("issuedispatch: ensure work item for %s: %w", req.IssueRef, err)
	}

	// Dispatch only while the item is still in the entry lane: a fresh create, or
	// a prior create that never got dispatched (self-heal). Anything further along
	// was already dispatched.
	if res.Item.State == "backlog" {
		_, derr := s.dispatch.RequestDispatch(ctx, coord.RequestDispatchInput{
			WorkItemID: res.Item.ID,
			AgentID:    req.TriageAgentID,
			TeamID:     req.TeamID,
			Principal:  req.Principal,          // human EnabledBy (D1 provenance)
			Initiator:  issuetrigger.Initiator, // system dispatch provenance
		})
		if derr != nil && !errors.Is(derr, coord.ErrStateConflict) {
			return false, fmt.Errorf("issuedispatch: dispatch triage %s to %q: %w", res.Item.ID, req.TriageAgentID, derr)
		}
	}
	return res.Created, nil
}

// triageBody is the work-item body for a system-dispatched issue triage: a terse,
// machine-authored summary linking the issue under triage. The full upstream
// issue body + comment thread reaches the agent at run time through the mirror
// (keyed on the shared label), so the body stays plain — no secrets, no tokens
// (there is no code path from this package to a Run env).
func triageBody(req issuetrigger.TriageRequest) string {
	var b strings.Builder
	b.WriteString("Automated triage dispatched by the standing issue-triage policy (ISI-5595).\n\n")
	fmt.Fprintf(&b, "- GitHub issue: %s\n", req.IssueRef)
	if req.IssueURL != "" {
		fmt.Fprintf(&b, "- URL: %s\n", req.IssueURL)
	}
	b.WriteString("\nThis item was created under the system issue-triage identity, not an agent. " +
		"At the end of the run the agent posts its findings back to the ticket and the linked GitHub issue.")
	return b.String()
}
