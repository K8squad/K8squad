package reviewdispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/K8squad/K8squad/pkg/controller/reviewtrigger"
	"github.com/K8squad/K8squad/pkg/coord"
)

// reviewItemWriter is the create-if-absent seam this store depends on — satisfied
// by *coord.WorkItemWriteStore. Narrowed to one method so the authz-critical
// dispatch logic is unit-testable with a fake, no Postgres.
type reviewItemWriter interface {
	EnsureReviewWorkItem(ctx context.Context, in coord.EnsureReviewWorkItemInput) (coord.EnsureReviewWorkItemResult, error)
}

// reviewItemDispatcher is the dispatch seam — satisfied by *coord.WorkItemDispatchStore.
type reviewItemDispatcher interface {
	RequestDispatch(ctx context.Context, in coord.RequestDispatchInput) (coord.WorkItemDispatchResult, error)
}

// SystemReviewItemStore implements reviewtrigger.ReviewItemStore — the custody-
// wall-sensitive create + dispatch, executed under the SYSTEM identity. It is the
// ONLY place the review automation touches coord's write surface; the pure
// dispatcher hands it a already-qualified, already-deduplicated ReviewRequest and
// this store realizes it idempotently.
//
// Governance (D1, ISI-4711):
//   - the work item is authored under reviewtrigger.Initiator ("system:review-
//     automation"), NOT an agent — coord.EnsureReviewWorkItem is the plain store
//     op, not the agent-authoring lane (workitemauthor.go), so no custody wall is
//     crossed;
//   - the coord dispatch is stamped Initiator=reviewtrigger.Initiator (system
//     provenance) and Principal=the human EnabledBy carried on the request — the
//     D1 authorizing-act provenance — never an agent identity.
type SystemReviewItemStore struct {
	writer   reviewItemWriter
	dispatch reviewItemDispatcher
}

// NewSystemReviewItemStore binds the store to the coord write + dispatch ops. Both
// are required.
func NewSystemReviewItemStore(writer *coord.WorkItemWriteStore, dispatch *coord.WorkItemDispatchStore) (*SystemReviewItemStore, error) {
	if writer == nil {
		return nil, errors.New("reviewdispatch: nil work-item write store")
	}
	if dispatch == nil {
		return nil, errors.New("reviewdispatch: nil work-item dispatch store")
	}
	return &SystemReviewItemStore{writer: writer, dispatch: dispatch}, nil
}

// EnsureReview creates the PR-review work item if none carries req.DedupLabel yet
// (create-if-absent, serialised in coord against concurrent reconciles) and
// dispatches it to req.ReviewerAgentID under the SYSTEM identity. It returns
// created=false when the label already existed.
//
// Self-heal: the create (coord txn) and the dispatch (a separate coord txn) are
// not one distributed transaction. If a prior pass created the item but failed
// before dispatch, the item sits in 'backlog'; this pass finds it (created=false)
// and, because it is still in the entry lane, dispatches it. An item already
// advanced past 'backlog' was dispatched by an earlier pass — re-dispatching a
// claimed item would 409, so a benign ErrStateConflict is swallowed and the pass
// treats the review as already handled.
func (s *SystemReviewItemStore) EnsureReview(ctx context.Context, req reviewtrigger.ReviewRequest) (bool, error) {
	if req.Principal == "" {
		// The human EnabledBy provenance is mandatory for the D1 dispatch; the
		// policy reader already guards this, but fail closed here too rather than
		// author a review the dispatch can't provenance.
		return false, fmt.Errorf("reviewdispatch: review request for %s#%s has no Principal (EnabledBy) provenance", req.RepoURL, req.PRNumber)
	}

	res, err := s.writer.EnsureReviewWorkItem(ctx, coord.EnsureReviewWorkItemInput{
		ProjectID:  req.ProjectID,
		TeamID:     req.TeamID,
		Title:      req.Title,
		Body:       reviewBody(req),
		DedupLabel: req.DedupLabel,
		Principal:  reviewtrigger.Initiator, // SYSTEM author — never an agent
	})
	if err != nil {
		return false, fmt.Errorf("reviewdispatch: ensure work item for %s#%s: %w", req.RepoURL, req.PRNumber, err)
	}

	// Dispatch only while the item is still in the entry lane: a fresh create, or
	// a prior create that never got dispatched (self-heal). Anything further along
	// was already dispatched.
	if res.Item.State == "backlog" {
		_, derr := s.dispatch.RequestDispatch(ctx, coord.RequestDispatchInput{
			WorkItemID: res.Item.ID,
			AgentID:    req.ReviewerAgentID,
			TeamID:     req.TeamID,
			Principal:  req.Principal,           // human EnabledBy (D1 provenance)
			Initiator:  reviewtrigger.Initiator, // system dispatch provenance
		})
		if derr != nil && !errors.Is(derr, coord.ErrStateConflict) {
			return false, fmt.Errorf("reviewdispatch: dispatch review %s to %q: %w", res.Item.ID, req.ReviewerAgentID, derr)
		}
	}
	return res.Created, nil
}

// reviewBody is the work-item body for a system-dispatched PR review: a terse,
// machine-authored summary linking the PR under review and pinning the head SHA
// the review is bound to. It is intentionally plain text — no secrets, no tokens
// (there is no code path from this package to a Run env).
func reviewBody(req reviewtrigger.ReviewRequest) string {
	var b strings.Builder
	b.WriteString("Automated code review dispatched by the standing review policy (ISI-4750).\n\n")
	fmt.Fprintf(&b, "- Repository: %s\n", req.RepoURL)
	fmt.Fprintf(&b, "- Pull request: #%s\n", req.PRNumber)
	if req.HeadSHA != "" {
		fmt.Fprintf(&b, "- Head SHA: %s\n", req.HeadSHA)
	}
	b.WriteString("\nThis item was created under the system review-automation identity, not an agent.")
	return b.String()
}
