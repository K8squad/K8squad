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

// Package teamroster resolves a Team's composition (Team.Spec.Agents) into the
// "your team" roster fact the context assembler injects into a coordinator's
// run context (ISI-5245, orchestration MVP, parent ISI-5220).
//
// The coordinator prompt (pkg/roleprompt.DefaultCoordinatorPrompt) tells a
// decomposing PM to "assign each sub-ticket to the right role's agent" and that
// "the assignee must be an agent on this item's team" — but nothing ever
// surfaced WHICH agents are on the team, nor the role each fills. work_item_assign
// / assignee_agent_id REQUIRE a valid agent NAME on the item's team
// (Team.Spec.Agents); with no roster in context the PM had no names to pass, so
// every assign attempt was rejected as "Invalid agent names" (zero sub-tickets
// dispatched in the ISI-5235 e2e). This package closes that gap by rendering the
// assignable agent NAME ↔ role mapping the assembler injects as an authoritative,
// must-include element — the exact counterpart to pkg/roleprompt (ISI-5223) on
// the assign half of the authoring lane.
//
// The role each agent fills is its Agent.Spec.RoleRef.Name (the same role
// vocabulary the coordinator prompt speaks: "architect", "implementer",
// "reviewer"). Reading each Agent CR to surface its role is best-effort: a
// dangling ref (agent named on the team but its CR absent) still lists the NAME
// — the name is what an assign needs — just without a resolved role. Only a
// genuine, non-NotFound read error fails closed, so a lookup glitch can never
// silently ship a coordinator a partial roster that would send it back to
// guessing names.
package teamroster

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// Resolve builds the roster fact for team: for each ref in team.Spec.Agents,
// its assignable NAME and the role it fills (Agent.Spec.RoleRef.Name, resolved
// against defaultNS or the ref's own namespace). Returns "" when the team names
// no agents (nothing to assign to — unchanged behavior). It is fail-closed on a
// genuine read error (not NotFound): the caller requeues rather than dispatching
// a run whose team could not be resolved. A NotFound Agent CR is NOT an error —
// the name is still listed (it is the assign target), only its role is omitted.
func Resolve(ctx context.Context, reader client.Reader, team *api.Team, defaultNS string) (string, error) {
	if team == nil || len(team.Spec.Agents) == 0 {
		return "", nil
	}

	lines := make([]string, 0, len(team.Spec.Agents))
	for _, ref := range team.Spec.Agents {
		if ref.Name == "" {
			continue
		}
		ns := ref.Namespace
		if ns == "" {
			ns = defaultNS
		}
		var agent api.Agent
		role := ""
		if err := reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &agent); err != nil {
			if !apierrors.IsNotFound(err) {
				return "", fmt.Errorf("teamroster: read Agent %s/%s for team %s: %w", ns, ref.Name, team.Name, err)
			}
			// Dangling ref: still list the NAME (it is the assign target); role omitted.
		} else {
			role = agent.Spec.RoleRef.Name
		}
		if role != "" {
			lines = append(lines, fmt.Sprintf("- %s — role: %s", ref.Name, role))
		} else {
			lines = append(lines, fmt.Sprintf("- %s", ref.Name))
		}
	}
	if len(lines) == 0 {
		return "", nil
	}

	var b strings.Builder
	b.WriteString("## Your team (assignable agents)\n\n")
	b.WriteString("These are the agents on this item's team. When you assign a sub-ticket ")
	b.WriteString("(via `work_item_assign`, or `assignee_agent_id` on `work_item_create`), ")
	b.WriteString("the assignee MUST be one of these agent NAMES, exactly as written below — ")
	b.WriteString("a role name (\"architect\", \"reviewer\") is NOT a valid assignee and is rejected ")
	b.WriteString("as an invalid agent name. Match the work to the role, then pass that agent's NAME:\n\n")
	b.WriteString(strings.Join(lines, "\n"))
	return b.String(), nil
}
