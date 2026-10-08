// Package issuedispatch binds the pure pkg/controller/issuetrigger decision core
// to its concrete operator authorities (ISI-5595 WS-B): the standing IssueTriage
// policy read off the Project CR, and the custody-wall-sensitive work-item create
// + dispatch executed under the SYSTEM identity. It is the issue-triage sibling
// of internal/reviewdispatch and follows the same governance posture.
package issuedispatch

import (
	"context"
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/controller/issuetrigger"
)

// ErrPolicyMissingProvenance — an enabled triage policy carries no server-stamped
// EnabledBy. The apiserver stamps it on every write that sets Enabled=true, so an
// empty value on an enabled policy is a corrupted/hand-edited CR; the D1 dispatch
// has no authorizing-act provenance to thread, so we refuse rather than dispatch
// unprovenanced.
var ErrPolicyMissingProvenance = errors.New("issuedispatch: enabled triage policy has no EnabledBy provenance")

// ErrPolicyMissingEnabledAt — an enabled triage policy carries no server-stamped
// EnabledAt watermark. Without it the D4 forward-only guard cannot be enforced
// (every historical open issue would qualify and flood the squad on first
// enable), so we refuse rather than dispatch the whole backlog. The apiserver
// stamps it alongside EnabledBy on enable.
var ErrPolicyMissingEnabledAt = errors.New("issuedispatch: enabled triage policy has no EnabledAt watermark")

// ProjectPolicyReader implements issuetrigger.PolicyReader. It resolves the
// standing IssueTriage policy off the Project CR and maps it, plus the coord
// scope ids (Project CR uid = coord project_id, owning Team CR uid = coord
// team_id), onto an issuetrigger.Policy.
type ProjectPolicyReader struct {
	reader client.Reader
}

// NewProjectPolicyReader binds the reader to the host's informer cache.
func NewProjectPolicyReader(reader client.Reader) *ProjectPolicyReader {
	return &ProjectPolicyReader{reader: reader}
}

// TriagePolicy resolves the policy for the Project the repo-sync reconciler just
// processed.
//
// Returns:
//   - (nil, nil): the Project vanished, has no issue-triage automation, or it is
//     disabled — a no-op pass for that Project.
//   - (*Policy, nil): an enabled, provenance-bearing, watermark-bearing policy.
//   - (nil, ErrPolicyMissingProvenance / ErrPolicyMissingEnabledAt): enabled but
//     missing server-stamped provenance/watermark (misconfig; fails the pass).
//   - (nil, err): an infrastructure error reading the cluster (fails the pass so
//     it surfaces on the Project's SyncReady condition).
func (r *ProjectPolicyReader) TriagePolicy(ctx context.Context, projectNamespace, projectName string) (*issuetrigger.Policy, error) {
	var projects ksquadv1.ProjectList
	if err := r.reader.List(ctx, &projects, client.InNamespace(projectNamespace)); err != nil {
		return nil, fmt.Errorf("issuedispatch: list projects in %q: %w", projectNamespace, err)
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

	auto := proj.Spec.Repo.Automation
	if auto == nil || auto.IssueTriage == nil {
		return nil, nil // unconfigured ⇒ never trigger (D1)
	}
	spec := auto.IssueTriage
	if !spec.Enabled {
		return nil, nil // disabled ⇒ never trigger (D1)
	}
	if spec.EnabledBy == "" {
		return nil, ErrPolicyMissingProvenance
	}
	if spec.EnabledAt == nil || spec.EnabledAt.IsZero() {
		return nil, ErrPolicyMissingEnabledAt
	}

	teamUID, err := teamUIDForNamespace(ctx, r.reader, proj.Namespace)
	if err != nil {
		return nil, err
	}

	return &issuetrigger.Policy{
		Enabled:        true,
		TriageAgentID:  spec.TriageAgentID,
		LabelFilter:    spec.LabelFilter,
		OnlyUnassigned: spec.EffectiveOnlyUnassigned(),
		EnabledBy:      spec.EnabledBy,
		EnabledAt:      spec.EnabledAt.Time,
		ProjectID:      string(proj.UID),
		TeamID:         teamUID,
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
		return "", fmt.Errorf("issuedispatch: list teams: %w", err)
	}
	for i := range teams.Items {
		if teams.Items[i].Namespace == ns {
			return string(teams.Items[i].UID), nil
		}
	}
	return "", nil
}
