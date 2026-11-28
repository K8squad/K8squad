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

// upward.go — controller-side manager resolution for the WS-B upward
// hand-off spine (ISI-7956, plan rev 3). pkg/coord is DB-only, so the WS-A
// org-chart edge (Role label ksquad.io/reports-to, mirrored from
// spec.reportsTo by the mrole-orgchart admission webhook) is resolved HERE,
// over the controller's client, and handed into the coord layer as a plain
// manager agent name.
//
// Resolution is ONE level up, fail-closed to "" (no push): any read gap, a
// team without a reconciled namespace, an unknown agent, a root role, or a
// manager not seated in the team's agent list resolves NO manager rather
// than a wrong one — the spine never fabricates a recipient. Chain recursion
// (multi-level walks) is WS-C's concern, not this spine's.
package rundrive

import (
	"context"
	"database/sql"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/orgops"
)

// UpwardManagerResolver resolves the manager agent name for the finishing
// agent of one work item ("" = none). runID is advisory provenance; the
// resolution keys on the work item's team and the checkout's assignee.
type UpwardManagerResolver func(ctx context.Context, workItemID, runID string) string

// WithUpwardManagerResolver opts this runner into the WS-B upward hand-off
// spine: every per-Run ProdEffects it builds carries the resolved manager, so
// Terminal pushes the completion hand-off. Nil leaves the spine off. Returns
// r for chaining.
func (r *ProdRunner) WithUpwardManagerResolver(f UpwardManagerResolver) *ProdRunner {
	r.upwardResolver = f
	return r
}

// WithUpwardManagerResolver opts these claims into the WS-B spine: a terminal
// re-entry settle that observes 'succeeded' pushes the same hand-off the
// machine's Terminal effect writes. Nil leaves the spine off. Returns c for
// chaining.
func (c *ProdClaims) WithUpwardManagerResolver(f UpwardManagerResolver) *ProdClaims {
	c.upwardResolver = f
	return c
}

// NewUpwardManagerResolver binds a resolver over the coord DB (work item →
// team + finishing agent) and the controller client (Team/Agent/Role CRs).
// The DB read runs OUTSIDE the caller's terminal transaction: the resolved
// manager is advisory input to the push, and the coord layer re-reads the
// authoritative assignee inside its own transaction for the marker row.
func NewUpwardManagerResolver(db *sql.DB, c client.Client) UpwardManagerResolver {
	return func(ctx context.Context, workItemID, runID string) string {
		if db == nil || c == nil || workItemID == "" {
			return ""
		}
		if ctx == nil {
			ctx = context.Background()
		}

		// (1) The item's team (Team CR uid, the intake linkage) and the
		// checkout's assignee — the finishing agent (stamped at acquire,
		// retained through release).
		var teamID, agent sql.NullString
		const facts = `
			SELECT wi.team_id::text,
			       COALESCE((SELECT cl.assignee_agent FROM coord.claim cl
			                  WHERE cl.work_item_id = wi.id), '')
			  FROM coord.work_item wi
			 WHERE wi.id = $1::uuid`
		if err := db.QueryRowContext(ctx, facts, workItemID).Scan(&teamID, &agent); err != nil {
			return ""
		}
		if !teamID.Valid || !agent.Valid || agent.String == "" {
			return ""
		}

		// (2) The Team CR by uid; agents live in its reconciled squad
		// namespace (the same Status.Namespace the intake sweep pins).
		var teams api.TeamList
		if err := c.List(ctx, &teams); err != nil {
			return ""
		}
		var team *api.Team
		for i := range teams.Items {
			if string(teams.Items[i].UID) == teamID.String {
				team = &teams.Items[i]
				break
			}
		}
		if team == nil || team.Status.Namespace == "" {
			return ""
		}
		ns := team.Status.Namespace

		// (3) The finishing agent's RoleRef (the runScopesFor listing
		// pattern: role namespace defaults to the agent's own).
		var agents api.AgentList
		if err := c.List(ctx, &agents, client.InNamespace(ns)); err != nil {
			return ""
		}
		finishingRole, roleNS := "", ns
		for i := range agents.Items {
			if agents.Items[i].Name == agent.String {
				finishingRole = agents.Items[i].Spec.RoleRef.Name
				if agents.Items[i].Spec.RoleRef.Namespace != "" {
					roleNS = agents.Items[i].Spec.RoleRef.Namespace
				}
				break
			}
		}
		if finishingRole == "" {
			return ""
		}

		// (4) ONE level up the reports-to edge — the label the WS-A
		// mrole-orgchart webhook mirrors from spec.reportsTo (the
		// deriveRunScopes read).
		var roles api.RoleList
		if err := c.List(ctx, &roles, client.InNamespace(roleNS)); err != nil {
			return ""
		}
		managerRole := ""
		for i := range roles.Items {
			if roles.Items[i].Name == finishingRole {
				managerRole = roles.Items[i].Labels[orgops.LabelReportsTo]
				break
			}
		}
		if managerRole == "" {
			return "" // root/CEO: the WS-E human termination path, not this spine
		}

		// (5) The manager AGENT: an agent in the team namespace assuming the
		// manager role, seated in the Team composition (intake dispatches a
		// requested_agent only against team.Spec.Agents). Deterministic pick
		// (name-sorted first) when several agents share the role.
		seated := make(map[string]struct{}, len(team.Spec.Agents))
		for _, ref := range team.Spec.Agents {
			seated[ref.Name] = struct{}{}
		}
		var candidates []string
		for i := range agents.Items {
			a := &agents.Items[i]
			if a.Spec.RoleRef.Name != managerRole {
				continue
			}
			if _, ok := seated[a.Name]; !ok {
				continue
			}
			candidates = append(candidates, a.Name)
		}
		if len(candidates) == 0 {
			return ""
		}
		sort.Strings(candidates)
		return candidates[0]
	}
}
