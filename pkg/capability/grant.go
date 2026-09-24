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
//     (agent.spec.roleRef.name);
//  3. UNION the Team's grants keyed by any role a dispatched agent holds
//     (precedence across multiple roles/agents is a union).
//
// Deny-by-default and fail-closed: a missing Team, a Team with no grants, an
// agent with no matching grant, and a grant naming a role that no dispatched
// agent holds all resolve to the empty set — never error-open. Only a
// genuine read error (not NotFound) is returned, so the reconciler requeues
// rather than dispatching against a half-read grant.
func ResolveGrant(ctx context.Context, reader client.Reader, run *api.Run) (GrantSet, error) {
	teamNS := run.Spec.TeamRef.Namespace
	if teamNS == "" {
		teamNS = run.Namespace
	}
	var team api.Team
	if err := reader.Get(ctx, client.ObjectKey{Namespace: teamNS, Name: run.Spec.TeamRef.Name}, &team); err != nil {
		if isNotFound(err) {
			return GrantSet{}, nil
		}
		return GrantSet{}, fmt.Errorf("read team %s/%s for capability grant (fail-closed): %w", teamNS, run.Spec.TeamRef.Name, err)
	}
	if len(team.Spec.Grants) == 0 {
		return GrantSet{}, nil
	}

	// role name → granted capabilities (last write wins on duplicate roles;
	// the CRD listMapKey=role forbids duplicates at admission).
	byRole := make(map[string][]string, len(team.Spec.Grants))
	for _, g := range team.Spec.Grants {
		if g.Role == "" {
			continue
		}
		byRole[g.Role] = g.Capabilities
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
	}

	if len(caps) == 0 {
		return GrantSet{}, nil
	}
	return GrantSet{caps: caps}, nil
}
