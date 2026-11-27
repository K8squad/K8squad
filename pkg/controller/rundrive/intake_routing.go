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

package rundrive

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// intake_routing.go — ISI-7959 (ISI-7946 WS-D): hierarchy + process-aware
// routing, the "stronger drive" the board asked for. The intake sweep no
// longer leaves a team-targeted work item to a blind default (Agents[0] or
// the ISI-4927 fan-out): when the team's org chart and the root manager's
// Process resolve, the item is DRIVEN — actively pushed, one Run — to the
// stage owner: the composition agent holding the Role that owns the current
// stage of the owning manager's Process.
//
// The graph is exactly what WS-A (ISI-7955) landed: Role.Spec.ReportsTo is
// the org-chart edge (an agent reaches it through Agent.spec.roleRef), and a
// manager Role's Spec.Process is the ordered stage pipeline, each
// ProcessStage naming the Role that executes it. Team admission
// (GuardTeamOrgChart) already guarantees the invariants this routing leans
// on — exactly one root (CEO) Role, an acyclic chart, every non-root Role
// reporting to exactly one manager, process stages naming Roles inside the
// team, no delegation cycles — but runtime re-derives them defensively
// rather than trusting admission blindly: a chart that does not resolve to
// exactly one root Role with a non-empty Process degrades to the legacy
// selection with a loud log, never to a wrong dispatch.
//
// The owning manager for an intake item is the team's ROOT Role: work
// enters the squad at the top of the chain and is driven downward. The
// CURRENT stage at intake is the root Process's FIRST stage — intake is the
// process ENTRY (board 'todo' → first Run); advancing stages on completion
// hand-offs is the WS-C engine's job (ISI-7946 WS-C, not yet landed), so
// until it lands every intake enters at stage one.

// stageOwnerRouting is the WS-D routing resolution for one Team.
type stageOwnerRouting struct {
	// agent is the composition agent holding the Role that owns the root
	// manager's first Process stage — the stage owner a work item is driven
	// to. "" when the routing did not resolve (reason says why; the legacy
	// selection applies).
	agent string
	// root is the team's root (CEO) Role name — the owning manager whose
	// Process governs intake.
	root string
	// stage is the Process stage the routing enters at (the root Process's
	// first stage name).
	stage string
	// reason names why the routing did not resolve ("" when it did) — the
	// honest-degraded log line's payload.
	reason string
}

// resolveStageOwnerRouting walks one Team's composition into the WS-D
// routing target. Reads are best-effort per CR — a missing Agent or Role CR
// is skipped, not fatal (admission owns those denials) — and a chart that
// cannot yield exactly one root Role with a non-empty Process resolves to
// agent="" plus a reason, so the caller falls back to the legacy selection.
// Determinism: root candidates are sorted by Role name, and the stage-owner
// agent is the FIRST composition entry holding the stage Role
// (Team.Spec.Agents order) — the same order the legacy fan-out mints in.
func (i *Intake) resolveStageOwnerRouting(ctx context.Context, team api.Team) stageOwnerRouting {
	// Agent → Role edges, walked off the composition in Team.Spec.Agents
	// order. Agent CRs resolve with the ISI-4820 candidate list (explicit
	// ref namespace → Team HOME namespace → EXEC namespace) — the same list
	// resolveAgent walks when minting the Run, so the agent intake drives to
	// is the agent the mint can actually address.
	type roleEdge struct {
		agent    string
		roleName string
	}
	var edges []roleEdge
	roles := make(map[string]*api.Role) // Role name → resolved CR (nil = unresolvable)
	for _, aref := range team.Spec.Agents {
		agent, ok := i.routingAgent(ctx, aref, team.Namespace, team.Status.Namespace)
		if !ok {
			continue // honest-degraded: admission owns missing-agent denials
		}
		roleName := agent.Spec.RoleRef.Name
		if roleName == "" {
			continue // a role-less agent can never hold a process stage
		}
		edges = append(edges, roleEdge{agent: aref.Name, roleName: roleName})
		if _, cached := roles[roleName]; !cached {
			roles[roleName] = i.routingRole(ctx, agent.Spec.RoleRef, agent.Namespace, team.Namespace, team.Status.Namespace)
		}
	}

	// The root (CEO) Role: ReportsTo unset. Admission guarantees exactly
	// one; runtime re-derives it and degrades when the live world disagrees
	// (a Role deleted after admission, a second root grafted in).
	var roots []string
	for roleName, role := range roles {
		if role == nil {
			continue
		}
		if role.Spec.ReportsTo == nil || role.Spec.ReportsTo.Name == "" {
			roots = append(roots, roleName)
		}
	}
	sort.Strings(roots) // map iteration is random; the reason line must not be
	if len(roots) != 1 {
		if len(roots) == 0 {
			return stageOwnerRouting{reason: "org chart has no root Role (every Role reports to someone, or no Role CR resolves)"}
		}
		return stageOwnerRouting{reason: fmt.Sprintf("org chart has %d root Roles (%s); need exactly one", len(roots), strings.Join(roots, ", "))}
	}
	root := roots[0]

	process := roles[root].Spec.Process
	if len(process) == 0 {
		return stageOwnerRouting{root: root, reason: fmt.Sprintf("root Role %s defines no Process", root)}
	}

	// Intake enters the process at its FIRST stage (see the file comment):
	// the current stage of a fresh work item is stage one.
	first := process[0]
	for _, e := range edges {
		if e.roleName == first.RoleRef.Name {
			return stageOwnerRouting{agent: e.agent, root: root, stage: first.Stage}
		}
	}
	return stageOwnerRouting{root: root, stage: first.Stage,
		reason: fmt.Sprintf("no composition agent holds Role %q (root Role %s, process stage %q)", first.RoleRef.Name, root, first.Stage)}
}

// routingAgent resolves one composition Agent CR, trying (in order) an
// explicit ref namespace, the Team's HOME namespace, then the EXEC
// namespace — the ISI-4820 candidate list. ok=false when it resolves
// nowhere (the caller skips it; admission owns the denial).
func (i *Intake) routingAgent(ctx context.Context, ref api.ObjectRef, homeNS, execNS string) (*api.Agent, bool) {
	for _, ns := range routingNamespaces(ref.Namespace, homeNS, execNS) {
		var agent api.Agent
		if err := i.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &agent); err == nil {
			return &agent, true
		}
	}
	return nil, false
}

// routingRole resolves the Role CR for one agent's roleRef, trying the
// ref's own namespace, then the agent's, the Team HOME, and the EXEC
// namespace. nil when it resolves nowhere (the caller skips it; admission
// owns the denial).
func (i *Intake) routingRole(ctx context.Context, roleRef api.ObjectRef, agentNS, homeNS, execNS string) *api.Role {
	for _, ns := range routingNamespaces(roleRef.Namespace, agentNS, homeNS, execNS) {
		var role api.Role
		if err := i.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: roleRef.Name}, &role); err == nil {
			return &role
		}
	}
	return nil
}

// routingNamespaces returns the CR-resolution candidate namespaces: the
// ref's own namespace first (honored as authored), then each fallback,
// deduped, empty dropped.
func routingNamespaces(refNS string, fallbacks ...string) []string {
	var out []string
	add := func(ns string) {
		if ns == "" {
			return
		}
		for _, seen := range out {
			if seen == ns {
				return
			}
		}
		out = append(out, ns)
	}
	add(refNS)
	for _, ns := range fallbacks {
		add(ns)
	}
	return out
}
