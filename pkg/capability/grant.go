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

package capability

import (
	"context"
	"fmt"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// CapabilityWorkItemAuthor is the capability slug that authorizes a
// dispatched agent to author work items (create/link tickets) through the
// internal authoring MCP server (ADR-0024a). It is the only capability slug
// defined today; the grant store keeps the list open (Team config) so
// widening never requires a rebuild (ADR-0024 §3).
const CapabilityWorkItemAuthor = "work_item.author"

// CapabilityCoordinatorPropose is the capability slug that marks a dispatched
// coordinator agent as operating in PROPOSE mode (Role.Spec.CoordinatorMode ==
// "propose", ISI-5282). It is a DERIVED grant — never a Team.Spec.Grants entry —
// added alongside work_item.author for a coordinator Role whose CoordinatorMode
// is propose (see ResolveGrant). It rides the SAME server-authenticated
// capability channel work_item.author does (the run-token claim S3 / the
// X-Agent-Capabilities header), so the authoring MCP edge learns a coordinator
// is propose-gated from control-plane config alone — never a tool argument or a
// client-settable field. When present, the authoring tools (work_item_create /
// work_item_assign) raise inert discussion Proposals the human confirms, instead
// of executing the coord write directly (auto mode, the default, is unchanged).
const CapabilityCoordinatorPropose = "coordinator.propose"

// CapabilityDiscussion is the capability slug baked into the per-run HS256
// token for source=discussion thread-runs. It authorizes the run's token to
// call discussion_search + discussion_post through the built-in
// ksquad-memory-discussion MCPServer (ADR-0024c D4, ISI-5138).
const CapabilityDiscussion = "discussion"

// GrantSet is the resolved capability set for a Run's decomposing agent(s),
// read from the owning Team's grant store (ADR-0024a S4). The zero value is
// the empty set — deny-by-default: an unresolved or grant-absent Run yields
// a GrantSet that grants nothing. It feeds S2's authoring-MCP gate and S3's
// run-token capability claim so both derive authority from one read.
type GrantSet struct {
	caps map[string]struct{}
}

// Has reports whether capability is granted. Safe on the zero value.
func (g GrantSet) Has(capability string) bool {
	if g.caps == nil {
		return false
	}
	_, ok := g.caps[capability]
	return ok
}

// Empty reports whether the set grants nothing.
func (g GrantSet) Empty() bool { return len(g.caps) == 0 }

// List returns the granted capability slugs, sorted — stable bytes for
// manifests, run-token claims and logs.
func (g GrantSet) List() []string {
	if len(g.caps) == 0 {
		return nil
	}
	out := make([]string, 0, len(g.caps))
	for c := range g.caps {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// ResolveGrant resolves the capability grant for a Run's decomposing
// agent(s) from the owning Team's grant store (ADR-0024a S4). It is the ONE
// read path both S2 (authoring-MCP gate, ISI-4868) and S3 (run-token claim,
// ISI-4869) call, so the gate proved and the token minted agree by
// construction.
//
// Resolution walks agent → role → team grant:
//  1. read the owning Team (run.spec.teamRef);
//  2. for each dispatched agent (run.spec.agents) resolve its role
//     (agent.spec.roleRef.name) and read that Role CR;
//  3. UNION the Team's grants keyed by any role a dispatched agent holds
//     (precedence across multiple roles/agents is a union);
//  4. ADD work_item.author by DEFAULT for any dispatched agent whose Role is a
//     COORDINATOR (Role.Spec.Coordinator=true) — the orchestration MVP
//     (ISI-5223): a coordinator (PM) claiming a ticket must be able to
//     decompose it into sub-tickets and delegate them via the ADR-0024
//     authoring lane, out of the box, without an explicit Team.Spec.Grants
//     entry. This is a DELIBERATE relaxation of ADR-0024's deny-by-default
//     posture for coordinator roles ONLY; Team.Spec.Grants remains the config
//     seam to widen authoring to other (non-coordinator) roles.
//
// Deny-by-default and fail-closed: a missing Team, a Team with no grants, a
// non-coordinator agent with no matching Team grant, and a grant naming a role
// that no dispatched agent holds all resolve to the empty set — never
// error-open. Only a genuine read error (not NotFound) is returned, so the
// reconciler requeues rather than dispatching against a half-read grant.
func ResolveGrant(ctx context.Context, reader client.Reader, run *api.Run) (GrantSet, error) {
	teamNS := run.Spec.TeamRef.Namespace
	if teamNS == "" {
		teamNS = run.Namespace
	}
	var team api.Team
	teamFound := true
	if err := reader.Get(ctx, client.ObjectKey{Namespace: teamNS, Name: run.Spec.TeamRef.Name}, &team); err != nil {
		if !isNotFound(err) {
			return GrantSet{}, fmt.Errorf("read team %s/%s for capability grant (fail-closed): %w", teamNS, run.Spec.TeamRef.Name, err)
		}
		teamFound = false
	}

	// role name → granted capabilities (last write wins on duplicate roles;
	// the CRD listMapKey=role forbids duplicates at admission). A missing Team
	// yields no explicit grants, but the coordinator DEFAULT below still applies
	// (it derives from the Role CR, not the Team's grant store).
	byRole := make(map[string][]string, len(team.Spec.Grants))
	if teamFound {
		for _, g := range team.Spec.Grants {
			if g.Role == "" {
				continue
			}
			byRole[g.Role] = g.Capabilities
		}
	}

	caps := map[string]struct{}{}
	for _, agentRef := range run.Spec.Agents {
		agentNS := agentRef.Namespace
		if agentNS == "" {
			agentNS = run.Namespace
		}
		var agent api.Agent
		if err := reader.Get(ctx, client.ObjectKey{Namespace: agentNS, Name: agentRef.Name}, &agent); err != nil {
			if isNotFound(err) {
				continue
			}
			return GrantSet{}, fmt.Errorf("read agent %s/%s for capability grant (fail-closed): %w", agentNS, agentRef.Name, err)
		}
		role := agent.Spec.RoleRef.Name
		if role == "" {
			continue
		}
		for _, c := range byRole[role] {
			if c == "" {
				continue
			}
			caps[c] = struct{}{}
		}
		// ISI-5223: coordinator roles are author-capable by default.
		// ISI-5282: a coordinator Role whose CoordinatorMode is "propose" ALSO
		// gets the derived coordinator.propose grant, so the authoring edge raises
		// proposals instead of executing directly.
		coordinator, mode, err := coordinatorRole(ctx, reader, &agent)
		if err != nil {
			return GrantSet{}, err
		}
		if coordinator {
			caps[CapabilityWorkItemAuthor] = struct{}{}
			if mode == CoordinatorModePropose {
				caps[CapabilityCoordinatorPropose] = struct{}{}
			}
		}
	}

	if len(caps) == 0 {
		return GrantSet{}, nil
	}
	return GrantSet{caps: caps}, nil
}

// CoordinatorModePropose is the Role.Spec.CoordinatorMode value that gates
// coordinator authoring behind a human-confirmed proposal (ISI-5282). It mirrors
// the api.RoleSpec enum; kept here (not imported) so the derivation reads with the
// grant slug it drives.
const CoordinatorModePropose = "propose"

// coordinatorRole reports whether the agent's Role (agent.spec.roleRef) is a
// coordinator (Role.Spec.Coordinator=true) — the ISI-5223 default-grant signal —
// and, if so, its CoordinatorMode (ISI-5282: "" / "auto" ⇒ execute directly,
// "propose" ⇒ raise proposals). An empty roleRef or a Role deleted after
// admission is NOT a coordinator (no default grant), mirroring the model-per-role
// resolver's treatment of a dangling roleRef. Only a TRANSIENT read error (not
// NotFound) fails closed, so a lookup glitch can never silently widen OR drop the
// authoring grant.
func coordinatorRole(ctx context.Context, reader client.Reader, agent *api.Agent) (bool, string, error) {
	if agent.Spec.RoleRef.Name == "" {
		return false, "", nil
	}
	ns := agent.Spec.RoleRef.Namespace
	if ns == "" {
		ns = agent.Namespace
	}
	var role api.Role
	if err := reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: agent.Spec.RoleRef.Name}, &role); err != nil {
		if isNotFound(err) {
			return false, "", nil
		}
		return false, "", fmt.Errorf("read role %s/%s for coordinator default grant (fail-closed): %w", ns, agent.Spec.RoleRef.Name, err)
	}
	return role.Spec.Coordinator, role.Spec.CoordinatorMode, nil
}
