package reviewdispatch

import (
	"context"
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/controller/reviewtrigger"
	"github.com/K8squad/K8squad/pkg/reviewauto"
)

// ErrPolicyMissingProvenance — an enabled review policy carries no server-stamped
// EnabledBy. The apiserver stamps it on every write that sets Enabled=true, so an
// empty value on an enabled policy is a corrupted/hand-edited CR; the D1 dispatch
// has no authorizing-act provenance to thread, so we refuse rather than dispatch
// unprovenanced.
var ErrPolicyMissingProvenance = errors.New("reviewdispatch: enabled review policy has no EnabledBy provenance")

// ProjectPolicyReader implements reviewtrigger.PolicyReader. It resolves the
// standing ReviewAutomation policy off the Project CR (the E1 config surface) and
// maps it, plus the coord scope ids (Project CR uid = coord project_id, owning
// Team CR uid = coord team_id), onto a reviewtrigger.Policy. It also performs the
// D5 defensive reviewer-eligibility re-check via the SHARED pkg/reviewauto rule,
// so a policy the apiserver accepted on write but that has since drifted (reviewer
// removed from the team, or its Role lost code_review) fails loudly at dispatch
// time instead of dispatching to an ineligible reviewer.
type ProjectPolicyReader struct {
	reader client.Reader
}

// NewProjectPolicyReader binds the reader to the host's informer cache — the same
// reader the project/team resolvers and the coord dispatch authority use.
func NewProjectPolicyReader(reader client.Reader) *ProjectPolicyReader {
	return &ProjectPolicyReader{reader: reader}
}

// ReviewPolicy resolves the policy for the Project (projectNamespace/projectName)
// the repo-sync reconciler just processed.
//
// Returns:
//   - (nil, nil): the Project vanished, has no ReviewAutomation, or it is
//     disabled — a no-op pass for that Project (the dispatcher treats nil exactly
//     like disabled).
//   - (*Policy, nil): an enabled, provenance-bearing, reviewer-eligible policy.
//   - (nil, ErrPolicyMissingProvenance): enabled but no EnabledBy (400-class
//     misconfig; fails the pass).
//   - (nil, err): a D5 eligibility rejection (reviewauto sentinel) or an
//     infrastructure error reading the cluster — both fail the pass so the
//     misconfiguration is surfaced on the Project's SyncReady condition, not
//     silently swallowed.
func (r *ProjectPolicyReader) ReviewPolicy(ctx context.Context, projectNamespace, projectName string) (*reviewtrigger.Policy, error) {
	var projects ksquadv1.ProjectList
	if err := r.reader.List(ctx, &projects, client.InNamespace(projectNamespace)); err != nil {
		return nil, fmt.Errorf("reviewdispatch: list projects in %q: %w", projectNamespace, err)
	}
	var proj *ksquadv1.Project
	for i := range projects.Items {
		if projects.Items[i].Name == projectName {
			proj = &projects.Items[i]
			break
		}
	}
	if proj == nil {
		return nil, nil // project vanished between reconcile and policy read ⇒ no-op
	}

	spec := proj.Spec.Repo.ReviewAutomation
	if spec == nil || !spec.Enabled {
		return nil, nil // unconfigured / disabled ⇒ never trigger (D1)
	}
	if spec.EnabledBy == "" {
		return nil, ErrPolicyMissingProvenance
	}

	// D5 defensive re-check via the SHARED rule (pkg/reviewauto): the reviewer
	// must still be a code_review-capable agent in the owning Team. A sentinel
	// rejection (not-a-team-agent / no-capability / required) or an infra error
	// both surface as a failed pass — we never dispatch to an ineligible reviewer.
	if err := reviewauto.CheckReviewerEligibility(ctx, r.reader, proj.Namespace, spec.ReviewerAgentID); err != nil {
		return nil, fmt.Errorf("reviewdispatch: reviewer %q ineligible for %s/%s: %w",
			spec.ReviewerAgentID, projectNamespace, projectName, err)
	}

	teamUID, err := teamUIDForNamespace(ctx, r.reader, proj.Namespace)
	if err != nil {
		return nil, err
	}

	// Scope/Trigger are passed through raw: reviewtrigger.Policy owns the
	// defaulting (empty ⇒ team_authored / on_new_commits), the SAME defaults the
	// CRD markers and ReviewAutomationSpec.Effective* pin, so there is one source.
	return &reviewtrigger.Policy{
		Enabled:         true,
		ReviewerAgentID: spec.ReviewerAgentID,
		Scope:           spec.Scope,
		Trigger:         spec.Trigger,
		EnabledBy:       spec.EnabledBy,
		ProjectID:       string(proj.UID),
		TeamID:          teamUID,
	}, nil
}

// teamUIDForNamespace resolves the uid of the Team CR owning ns (coord team_id).
// "" when no Team owns the namespace (a dev host / partially-composed squad) —
// the caller then dispatches with an unscoped team, exactly as the board's
// project-ref resolver degrades. A List error is propagated.
func teamUIDForNamespace(ctx context.Context, reader client.Reader, ns string) (string, error) {
	if ns == "" {
		return "", nil
	}
	var teams ksquadv1.TeamList
	if err := reader.List(ctx, &teams); err != nil {
		return "", fmt.Errorf("reviewdispatch: list teams: %w", err)
	}
	for i := range teams.Items {
		if teams.Items[i].Namespace == ns {
			return string(teams.Items[i].UID), nil
		}
	}
	return "", nil
}
