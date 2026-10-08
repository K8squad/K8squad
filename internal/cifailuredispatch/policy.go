package cifailuredispatch

import (
	"context"
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/controller/cifailure"
)

// ErrPolicyMissingProvenance — an enabled CI-failure policy carries no
// server-stamped EnabledBy. The apiserver stamps it on every write that sets
// Enabled=true, so an empty value on an enabled policy is a corrupted/hand-edited
// CR; the system dispatch has no authorizing-act provenance to thread, so we
// refuse rather than dispatch unprovenanced.
var ErrPolicyMissingProvenance = errors.New("cifailuredispatch: enabled CI-failure policy has no EnabledBy provenance")

// ErrPolicyMissingWatermark — an enabled CI-failure policy carries no
// server-stamped EnabledAt. Without the forward-only watermark (D4) the trigger
// cannot tell historical failures from new ones, so enabling it would
// retroactively triage the mirror backlog. The apiserver stamps EnabledAt
// alongside EnabledBy on enable; an empty value on an enabled policy is a
// corrupted/hand-edited CR and we refuse rather than retroactively triage.
var ErrPolicyMissingWatermark = errors.New("cifailuredispatch: enabled CI-failure policy has no EnabledAt watermark")

// ProjectPolicyReader implements cifailure.PolicyReader. It resolves the standing
// CiFailure policy off the Project CR and maps it, plus the coord scope ids
// (Project CR uid = coord project_id, owning Team CR uid = coord team_id), onto a
// cifailure.Policy.
type ProjectPolicyReader struct {
	reader client.Reader
}

// NewProjectPolicyReader binds the reader to the host's informer cache — the same
// reader the project/team resolvers and the coord dispatch authority use.
func NewProjectPolicyReader(reader client.Reader) *ProjectPolicyReader {
	return &ProjectPolicyReader{reader: reader}
}

// CIFailurePolicy resolves the policy for the Project (projectNamespace/
// projectName) the repo-sync reconciler just processed.
//
// Returns:
//   - (nil, nil): the Project vanished, has no CiFailure, or it is disabled — a
//     no-op pass for that Project (the dispatcher treats nil exactly like disabled).
//   - (*Policy, nil): an enabled, provenance-bearing, watermarked policy.
//   - (nil, ErrPolicyMissingProvenance/ErrPolicyMissingWatermark): enabled but
//     missing a server-stamped field (400-class misconfig; fails the pass).
//   - (nil, err): an infrastructure error reading the cluster — fails the pass so
//     the misconfiguration surfaces on the Project's SyncReady condition.
func (r *ProjectPolicyReader) CIFailurePolicy(ctx context.Context, projectNamespace, projectName string) (*cifailure.Policy, error) {
	var projects ksquadv1.ProjectList
	if err := r.reader.List(ctx, &projects, client.InNamespace(projectNamespace)); err != nil {
		return nil, fmt.Errorf("cifailuredispatch: list projects in %q: %w", projectNamespace, err)
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

	// ISI-5595 D2: the CI-failure policy lives under the uniform automation group
	// (spec.repo.automation.ciFailure), the sibling of automation.issueTriage —
	// not a bare spec.repo field. A nil automation group ⇒ nothing configured.
	var spec *ksquadv1.CiFailureSpec
	if proj.Spec.Repo.Automation != nil {
		spec = proj.Spec.Repo.Automation.CiFailure
	}
	if spec == nil || !spec.Enabled {
		return nil, nil // unconfigured / disabled ⇒ never trigger
	}
	if spec.EnabledBy == "" {
		return nil, ErrPolicyMissingProvenance
	}
	if spec.EnabledAt == nil {
		return nil, ErrPolicyMissingWatermark
	}

	teamUID, err := teamUIDForNamespace(ctx, r.reader, proj.Namespace)
	if err != nil {
		return nil, err
	}

	// Conclusions are resolved to the effective set here (the SAME default the CRD
	// marker pins), so the pure dispatcher never re-derives the default.
	return &cifailure.Policy{
		Enabled:      true,
		AgentID:      spec.AgentID,
		Conclusions:  spec.EffectiveConclusions(),
		BranchFilter: spec.BranchFilter,
		EnabledBy:    spec.EnabledBy,
		EnabledAt:    spec.EnabledAt.Time,
		ProjectID:    string(proj.UID),
		TeamID:       teamUID,
	}, nil
}

// teamUIDForNamespace resolves the uid of the Team CR owning ns (coord team_id).
// "" when no Team owns the namespace (a dev host / partially-composed squad) —
// the caller then dispatches with an unscoped team. A List error is propagated.
func teamUIDForNamespace(ctx context.Context, reader client.Reader, ns string) (string, error) {
	if ns == "" {
		return "", nil
	}
	var teams ksquadv1.TeamList
	if err := reader.List(ctx, &teams); err != nil {
		return "", fmt.Errorf("cifailuredispatch: list teams: %w", err)
	}
	for i := range teams.Items {
		if teams.Items[i].Namespace == ns {
			return string(teams.Items[i].UID), nil
		}
	}
	return "", nil
}
