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

// Package reviewauto holds the SHARED reviewer-eligibility rule for the
// PR-review-automation feature (ISI-4750). It is deliberately a small, dependency-
// light pkg so BOTH the apiserver write path (E1 / ISI-4763 — validate a policy on
// write → 422) and the system-dispatch path (E4 / ISI-4766 — defensive re-check
// before dispatching the reviewer Run) enforce the D5 rule from ONE place. If the
// two ever diverge, a policy the apiserver accepted could fail (or worse, silently
// skip) at dispatch time — this package exists so they cannot.
//
// D5 rule (ISI-4750, E0 §2): a reviewer agent is eligible iff it is an agent in
// the owning Team's composition (Team.spec.agents) AND the agent's Role carries
// the code_review capability — i.e. Role.spec.activePhases contains "code_review"
// (api/v1alpha1/role_types.go). There is no separate boolean capability field.
package reviewauto

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// PhaseCodeReview is the lifecycle-phase string that marks a Role as code-review
// capable (D5). It matches the api/v1alpha1 Role.spec.activePhases enum member
// pinned by the +kubebuilder marker on RoleSpec.ActivePhases and validated by the
// Role webhook — keep it in lockstep with that enum.
const PhaseCodeReview = "code_review"

// The three D5 rejection reasons. They are sentinel errors so a caller can map
// them to a single 422 (the apiserver E1 write path) while distinguishing them
// from an infrastructure failure (a client.Reader error), which must surface as a
// 502 instead. errors.Is against these sentinels is the intended discrimination.
var (
	// ErrReviewerAgentRequired: the policy enables automation but names no
	// reviewer agent (the enabled ⇒ reviewerAgentId cross-field rule).
	ErrReviewerAgentRequired = errors.New("reviewauto: reviewer agent is required when review automation is enabled")

	// ErrReviewerNotTeamAgent: the named agent is not part of the owning Team's
	// composition (or no Team owns the namespace).
	ErrReviewerNotTeamAgent = errors.New("reviewauto: reviewer agent is not a member of the project's team")

	// ErrReviewerNoCapability: the named team agent's Role does not carry the
	// code_review capability.
	ErrReviewerNoCapability = errors.New("reviewauto: reviewer agent's role is not code_review capable")
)

// RoleCodeReviewCapable reports whether a Role carries the code_review capability
// (D5): its spec.activePhases contains "code_review". This is the pure predicate —
// no cluster access — so tests and both call paths share the exact rule.
//
// NOTE the back-compat subtlety of ActivePhases: an EMPTY activePhases means the
// role is phase-agnostic (eligible in every phase, the pre-phase-lifecycle
// default). We intentionally do NOT treat empty as code_review-capable here: D5 is
// an affirmative capability grant, and silently promoting every legacy role to a
// reviewer would let the automation dispatch a role never vetted for review. A
// role must explicitly list code_review to be eligible.
func RoleCodeReviewCapable(role *ksquadv1.Role) bool {
	if role == nil {
		return false
	}
	for _, p := range role.Spec.ActivePhases {
		if p == PhaseCodeReview {
			return true
		}
	}
	return false
}

// CheckReviewerEligibility resolves and enforces the full D5 rule for agentID
// against the Team owning teamNamespace, using reader for the CR lookups.
//
// Returns:
//   - nil                        → eligible.
//   - ErrReviewerAgentRequired   → agentID is empty.
//   - ErrReviewerNotTeamAgent    → no Team owns teamNamespace, or agentID is not
//     in the Team's composition, or the referenced Agent CR is missing.
//   - ErrReviewerNoCapability    → the agent is a team member but its Role lacks
//     code_review.
//   - any other (wrapped) error  → an infrastructure failure reading the cluster;
//     callers map this to 502, never 422 (do not errors.Is it against the
//     sentinels above).
//
// Both the E1 apiserver write handler and the E4 dispatch guard call this so the
// accept-on-write and dispatch-time checks can never disagree.
func CheckReviewerEligibility(ctx context.Context, reader client.Reader, teamNamespace, agentID string) error {
	if agentID == "" {
		return ErrReviewerAgentRequired
	}

	team, err := teamInNamespace(ctx, reader, teamNamespace)
	if err != nil {
		return fmt.Errorf("reviewauto: resolve owning team for namespace %q: %w", teamNamespace, err)
	}
	if team == nil {
		// No Team owns the namespace ⇒ agentID cannot be a team agent. This is a
		// D5 rejection (not-a-team-agent), not an infra error.
		return ErrReviewerNotTeamAgent
	}

	// The agent must be named in the Team's composition (match by ObjectRef.Name,
	// the identity Team.spec.agents carries and the reviewer dropdown selects).
	agentRef, inTeam := agentRefInTeam(team, agentID)
	if !inTeam {
		return ErrReviewerNotTeamAgent
	}

	// Resolve the Agent CR (ref namespace, else the Team's namespace).
	agentNS := agentRef.Namespace
	if agentNS == "" {
		agentNS = team.Namespace
	}
	var agent ksquadv1.Agent
	if err := reader.Get(ctx, client.ObjectKey{Namespace: agentNS, Name: agentRef.Name}, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			// Composition references an agent that no longer exists: from D5's
			// perspective this agent is not a usable team agent.
			return ErrReviewerNotTeamAgent
		}
		return fmt.Errorf("reviewauto: get agent %s/%s: %w", agentNS, agentRef.Name, err)
	}

	// Resolve the agent's Role (ref namespace, else the agent's namespace).
	roleNS := agent.Spec.RoleRef.Namespace
	if roleNS == "" {
		roleNS = agent.Namespace
	}
	var role ksquadv1.Role
	if err := reader.Get(ctx, client.ObjectKey{Namespace: roleNS, Name: agent.Spec.RoleRef.Name}, &role); err != nil {
		if apierrors.IsNotFound(err) {
			// A team agent with a dangling Role ref has no code_review capability.
			return ErrReviewerNoCapability
		}
		return fmt.Errorf("reviewauto: get role %s/%s: %w", roleNS, agent.Spec.RoleRef.Name, err)
	}

	if !RoleCodeReviewCapable(&role) {
		return ErrReviewerNoCapability
	}
	return nil
}

// ReviewerRef is one code_review-capable team agent in the identity the E2
// reviewer dropdown selects: Name is the agent name written as reviewerAgentId
// (the value CheckReviewerEligibility matches on), and ID is the Agent CR UID for
// a stable client key (mirrors the /api/squad/agents row shape).
type ReviewerRef struct {
	ID   string
	Name string
}

// EligibleReviewers returns every agent in the Team owning teamNamespace whose
// Role carries the code_review capability (D5) — the affirmative-grant roster the
// E2 reviewer dropdown pre-filters to (ISI-4779). It applies the SAME per-agent
// resolution as CheckReviewerEligibility (reviewerCapable), so the list and the
// write-time 422 cannot disagree: a name this returns passes CheckReviewerEligibility,
// and one it omits fails it.
//
// Returns (nil, nil) when no Team owns teamNamespace — an empty roster, not an
// error (existence-hiding is the caller's concern). A dangling agent/role ref is
// silently skipped (that agent is simply not eligible). Any OTHER cluster-read
// failure is returned wrapped, for the caller to map to 502 (never a partial
// roster presented as complete).
func EligibleReviewers(ctx context.Context, reader client.Reader, teamNamespace string) ([]ReviewerRef, error) {
	team, err := teamInNamespace(ctx, reader, teamNamespace)
	if err != nil {
		return nil, fmt.Errorf("reviewauto: resolve owning team for namespace %q: %w", teamNamespace, err)
	}
	if team == nil {
		return nil, nil
	}
	var out []ReviewerRef
	for _, ref := range team.Spec.Agents {
		uid, ok, err := reviewerCapable(ctx, reader, team, ref)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, ReviewerRef{ID: uid, Name: ref.Name})
		}
	}
	return out, nil
}

// reviewerCapable resolves agentRef's Agent CR and its Role within team and
// reports whether the role is code_review-capable, returning the Agent UID for a
// client key. It mirrors the agent→role resolution in CheckReviewerEligibility so
// EligibleReviewers and the write-time check apply the identical D5 rule. A
// missing Agent or Role CR ⇒ (_, false, nil): a dangling ref is not eligible,
// never an error. Only a genuine cluster-read failure returns a non-nil error.
func reviewerCapable(ctx context.Context, reader client.Reader, team *ksquadv1.Team, agentRef ksquadv1.ObjectRef) (string, bool, error) {
	agentNS := agentRef.Namespace
	if agentNS == "" {
		agentNS = team.Namespace
	}
	var agent ksquadv1.Agent
	if err := reader.Get(ctx, client.ObjectKey{Namespace: agentNS, Name: agentRef.Name}, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reviewauto: get agent %s/%s: %w", agentNS, agentRef.Name, err)
	}
	roleNS := agent.Spec.RoleRef.Namespace
	if roleNS == "" {
		roleNS = agent.Namespace
	}
	var role ksquadv1.Role
	if err := reader.Get(ctx, client.ObjectKey{Namespace: roleNS, Name: agent.Spec.RoleRef.Name}, &role); err != nil {
		if apierrors.IsNotFound(err) {
			return string(agent.UID), false, nil
		}
		return "", false, fmt.Errorf("reviewauto: get role %s/%s: %w", roleNS, agent.Spec.RoleRef.Name, err)
	}
	return string(agent.UID), RoleCodeReviewCapable(&role), nil
}

// agentRefInTeam returns the composition ref for agentID and whether it is
// present. Match is by ObjectRef.Name — the identity Team.spec.agents carries.
func agentRefInTeam(team *ksquadv1.Team, agentID string) (ksquadv1.ObjectRef, bool) {
	for _, ref := range team.Spec.Agents {
		if ref.Name == agentID {
			return ref, true
		}
	}
	return ksquadv1.ObjectRef{}, false
}

// teamInNamespace returns the Team CR whose namespace is ns, or (nil, nil) when
// none is found. Mirrors internal/apiserver.teamInNamespace so this package needs
// no dependency on the apiserver; both resolve the owning Team the same way.
func teamInNamespace(ctx context.Context, reader client.Reader, ns string) (*ksquadv1.Team, error) {
	if ns == "" {
		return nil, nil
	}
	var teams ksquadv1.TeamList
	if err := reader.List(ctx, &teams); err != nil {
		return nil, err
	}
	for i := range teams.Items {
		if teams.Items[i].Namespace == ns {
			return &teams.Items[i], nil
		}
	}
	return nil, nil
}
