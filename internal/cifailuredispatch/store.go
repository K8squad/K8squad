package cifailuredispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/K8squad/K8squad/pkg/controller/cifailure"
	"github.com/K8squad/K8squad/pkg/coord"
)

// ciItemWriter is the create-if-absent seam this store depends on — satisfied by
// *coord.WorkItemWriteStore. Narrowed to one method so the dispatch logic is
// unit-testable with a fake, no Postgres.
type ciItemWriter interface {
	EnsureCIFailureWorkItem(ctx context.Context, in coord.EnsureCIFailureWorkItemInput) (coord.EnsureCIFailureWorkItemResult, error)
}

// ciItemDispatcher is the dispatch seam — satisfied by *coord.WorkItemDispatchStore.
type ciItemDispatcher interface {
	RequestDispatch(ctx context.Context, in coord.RequestDispatchInput) (coord.WorkItemDispatchResult, error)
}

// SystemCIFailureItemStore implements cifailure.ItemStore — the custody-
// wall-sensitive create + dispatch, executed under the SYSTEM identity. It is the
// ONLY place CI-failure triage touches coord's write surface; the pure dispatcher
// hands it an already-qualified, already-deduplicated FailureRequest and this
// store realizes it idempotently.
//
// Governance (mirrors reviewdispatch.SystemReviewItemStore):
//   - the work item is authored under cifailure.Initiator ("system:ci-failure"),
//     NOT an agent — coord.EnsureCIFailureWorkItem is the plain store op, not the
//     agent-authoring lane, so no custody wall is crossed;
//   - the coord dispatch is stamped Initiator=cifailure.Initiator (system
//     provenance) and Principal=the human EnabledBy carried on the request — the
//     authorizing-act provenance — never an agent identity.
type SystemCIFailureItemStore struct {
	writer   ciItemWriter
	dispatch ciItemDispatcher
}

// NewSystemCIFailureItemStore binds the store to the coord write + dispatch ops.
// Both are required.
func NewSystemCIFailureItemStore(writer *coord.WorkItemWriteStore, dispatch *coord.WorkItemDispatchStore) (*SystemCIFailureItemStore, error) {
	if writer == nil {
		return nil, errors.New("cifailuredispatch: nil work-item write store")
	}
	if dispatch == nil {
		return nil, errors.New("cifailuredispatch: nil work-item dispatch store")
	}
	return &SystemCIFailureItemStore{writer: writer, dispatch: dispatch}, nil
}

// EnsureCIFailure creates the triage work item if none carries req.DedupLabel yet
// (create-if-absent, serialised in coord against concurrent reconciles) and
// dispatches it to req.AgentID under the SYSTEM identity. It returns created=false
// when the label already existed.
//
// Self-heal: the create (coord txn) and the dispatch (a separate coord txn) are
// not one distributed transaction. If a prior pass created the item but failed
// before dispatch, the item sits in 'backlog'; this pass finds it (created=false)
// and, because it is still in the entry lane, dispatches it. An item already
// advanced past 'backlog' was dispatched by an earlier pass — re-dispatching a
// claimed item would 409, so a benign ErrStateConflict is swallowed and the pass
// treats the triage as already handled.
func (s *SystemCIFailureItemStore) EnsureCIFailure(ctx context.Context, req cifailure.FailureRequest) (bool, error) {
	if req.Principal == "" {
		// The human EnabledBy provenance is mandatory for the dispatch; the policy
		// reader already guards this, but fail closed here too rather than author a
		// triage the dispatch can't provenance.
		return false, fmt.Errorf("cifailuredispatch: triage request for %s %q has no Principal (EnabledBy) provenance", req.RepoURL, req.CheckName)
	}

	in := coord.EnsureCIFailureWorkItemInput{
		ProjectID:  req.ProjectID,
		TeamID:     req.TeamID,
		Title:      req.Title,
		Body:       failureBody(req),
		DedupLabel: req.DedupLabel,
		Principal:  cifailure.Initiator, // SYSTEM author — never an agent
	}

	res, err := s.writer.EnsureCIFailureWorkItem(ctx, in)
	if err != nil {
		return false, fmt.Errorf("cifailuredispatch: ensure work item for %s %q@%s: %w", req.RepoURL, req.CheckName, req.HeadSHA, err)
	}

	// Dispatch only while the item is still in the entry lane: a fresh create, or
	// a prior create that never got dispatched (self-heal). Anything further along
	// was already dispatched.
	if res.Item.State == "backlog" {
		_, derr := s.dispatch.RequestDispatch(ctx, coord.RequestDispatchInput{
			WorkItemID: res.Item.ID,
			AgentID:    req.AgentID,
			TeamID:     req.TeamID,
			Principal:  req.Principal,       // human EnabledBy (authorizing provenance)
			Initiator:  cifailure.Initiator, // system dispatch provenance
		})
		if derr != nil && !errors.Is(derr, coord.ErrStateConflict) {
			return false, fmt.Errorf("cifailuredispatch: dispatch triage %s to %q: %w", res.Item.ID, req.AgentID, derr)
		}
	}
	return res.Created, nil
}

// failureBody is the work-item body for a system-dispatched CI-failure triage: a
// terse, machine-authored summary of the failing check run. It is intentionally
// plain text — no secrets, no tokens (there is no code path from this package to
// a Run env).
func failureBody(req cifailure.FailureRequest) string {
	var b strings.Builder
	b.WriteString("Automated CI-failure triage dispatched by the standing CI-failure policy (ISI-5595).\n\n")
	fmt.Fprintf(&b, "- Repository: %s\n", req.RepoURL)
	fmt.Fprintf(&b, "- Check: %s\n", req.CheckName)
	fmt.Fprintf(&b, "- Conclusion: %s\n", req.Conclusion)
	if req.HeadSHA != "" {
		fmt.Fprintf(&b, "- Head SHA: %s\n", req.HeadSHA)
	}
	b.WriteString("\nThis item was created under the system CI-failure identity, not an agent.")
	return b.String()
}
