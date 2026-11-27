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

// ISI-7946 WS-A: the mandatory org chart. Team admission validates the
// reporting hierarchy over the team's composition and each manager role's
// process. Graph edges are Role.Spec.ReportsTo (the model edge) with the
// ksquad.io/reports-to label as the legacy/compat source, so label-only
// charts (examples/bmad-team before the spec migration) conform today.
package webhook

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// GuardTeamOrgChart guards the mandatory hierarchy invariant (ISI-7946 WS-A)
// at Team admission: over the Roles composed into the Team (via
// spec.agents → spec.roleRef) there must be exactly one root (the CEO role,
// reporting to nobody), the chart must be acyclic, every non-root role must
// report to exactly one manager that is itself part of the team, and every
// manager process stage must name a Role in the team with no delegation
// cycles. A fully FLAT composition (no edges at all) is migrated, not
// rejected: the default chart is synthesized with every role reporting to
// the identified root, so no team is ever hierarchy-less.
const GuardTeamOrgChart = "team/orgChart"

// teamRoleNode is the org-chart projection of one Role in the team's
// composition.
type teamRoleNode struct {
	// reportsTo is the effective manager edge: spec.reportsTo.name when the
	// model edge is set, else the legacy ksquad.io/reports-to label value.
	reportsTo string
	// coordinator marks Role.spec.coordinator (a synthesized-root candidate).
	coordinator bool
	// process is Role.spec.process (nil when the role defines none).
	process []ksquadv1alpha1.ProcessStage
}

// validateTeamOrgChart enforces GuardTeamOrgChart. Unresolvable agents and
// roles are SKIPPED on not-found — GuardTeamAgents and the Agent-side
// dangling-roleRef warning own those denials — while read errors fail
// closed like every other guard.
func (v *CrossRefValidator) validateTeamOrgChart(ctx context.Context, team *ksquadv1alpha1.Team) field.ErrorList {
	var errs field.ErrorList
	if len(team.Spec.Agents) == 0 {
		return errs // nothing composed yet — nothing to validate
	}

	nodes := map[string]teamRoleNode{}
	for i, aref := range team.Spec.Agents {
		ns := resolveNamespace(aref, team.Namespace)
		var agent ksquadv1alpha1.Agent
		if err := v.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: aref.Name}, &agent); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			errs = append(errs, invalidf(fmt.Sprintf("spec.agents[%d]", i), aref, "admission read failed (fail-closed): %v", err))
			continue
		}
		rns := resolveNamespace(agent.Spec.RoleRef, agent.Namespace)
		var role ksquadv1alpha1.Role
		if err := v.Reader.Get(ctx, client.ObjectKey{Namespace: rns, Name: agent.Spec.RoleRef.Name}, &role); err != nil {
			if apierrors.IsNotFound(err) {
				continue // Agent admission warns on a dangling roleRef (D3 soft-guard)
			}
			errs = append(errs, invalidf(fmt.Sprintf("spec.agents[%d]", i), aref, "admission read failed (fail-closed): %v", err))
			continue
		}
		if _, dup := nodes[role.Name]; dup {
			continue // several agents on one Role: one node
		}
		nodes[role.Name] = teamRoleNode{
			reportsTo:   effectiveReportsTo(role),
			coordinator: role.Spec.Coordinator,
			process:     role.Spec.Process,
		}
	}
	if len(nodes) == 0 || len(errs) > 0 {
		return errs // fail closed on read errors; an empty composition has no chart
	}

	// Flat-team migration: a composition with NO reporting edges gets the
	// synthesized default chart — every role reports to the identified root.
	if !anyEdge(nodes) {
		root, ok := synthesizedRoot(nodes)
		if !ok {
			errs = append(errs, invalidf("spec.agents", team.Spec.Agents,
				"flat team has no reports-to edges and no identifiable root (no unique coordinator, no 'ceo' Role, more than one Role); set spec.reportsTo on the non-root Roles"))
			return errs
		}
		for name, node := range nodes {
			if name != root {
				node.reportsTo = root
				nodes[name] = node
			}
		}
	}

	// Invariant 1: every manager edge resolves inside the team's composition.
	for name, node := range nodes {
		if node.reportsTo == "" {
			continue
		}
		if _, ok := nodes[node.reportsTo]; !ok {
			errs = append(errs, invalidf("spec.agents", name,
				"Role %s reports to %s, which is not part of this team's composition; add an Agent holding that Role or fix the edge", name, node.reportsTo))
		}
	}

	// Invariant 2: exactly one root (the CEO role reports to nobody).
	var roots []string
	for name, node := range nodes {
		if node.reportsTo == "" {
			roots = append(roots, name)
		}
	}
	sort.Strings(roots)
	switch {
	case len(roots) == 0:
		errs = append(errs, invalidf("spec.agents", team.Spec.Agents,
			"team hierarchy has no root: every Role reports to someone (a cycle); break the cycle or point the chain at the CEO role"))
	case len(roots) > 1:
		errs = append(errs, invalidf("spec.agents", team.Spec.Agents,
			"team hierarchy must have exactly one root (the CEO role), found %d: %s; set spec.reportsTo on the extra roots", len(roots), strings.Join(roots, ", ")))
	}

	// Invariant 3: acyclic.
	for start := range nodes {
		seen := map[string]bool{start: true}
		cur := nodes[start].reportsTo
		for cur != "" {
			if seen[cur] {
				errs = append(errs, invalidf("spec.agents", start,
					"team hierarchy is cyclic starting at Role %s; break the reportsTo cycle", start))
				break
			}
			seen[cur] = true
			cur = nodes[cur].reportsTo
		}
	}

	// Invariant 4: process stages name Roles inside the team.
	delegation := map[string][]string{}
	for name, node := range nodes {
		for _, st := range node.process {
			if _, ok := nodes[st.RoleRef.Name]; !ok {
				errs = append(errs, invalidf("spec.agents", name,
					"Role %s process stage %q names Role %s, which is not part of this team's composition; fix roleRef or add an Agent holding that Role", name, st.Stage, st.RoleRef.Name))
				continue
			}
			delegation[name] = append(delegation[name], st.RoleRef.Name)
		}
	}

	// Invariant 5: no delegation cycles (a manager process that delegates,
	// transitively or to itself, back to the delegating Role).
	if cyc := delegationCycle(delegation); cyc != "" {
		errs = append(errs, invalidf("spec.agents", cyc,
			"manager process delegation is cyclic (cycle passes through Role %s); remove the stage that delegates back", cyc))
	}

	return errs
}

// effectiveReportsTo resolves a Role's manager edge: spec.reportsTo.name
// when the model edge is set, else the legacy label value.
func effectiveReportsTo(role ksquadv1alpha1.Role) string {
	if role.Spec.ReportsTo != nil && role.Spec.ReportsTo.Name != "" {
		return role.Spec.ReportsTo.Name
	}
	if role.Labels != nil {
		return role.Labels[ksquadv1alpha1.LabelReportsTo]
	}
	return ""
}

// anyEdge reports whether any role in the composition carries a manager edge.
func anyEdge(nodes map[string]teamRoleNode) bool {
	for _, node := range nodes {
		if node.reportsTo != "" {
			return true
		}
	}
	return false
}

// synthesizedRoot picks the default-chart root for a fully flat composition:
// the unique coordinator (the team's declared driver), else the single Role
// of a one-role team, else the Role named "ceo" (the BMAD convention).
func synthesizedRoot(nodes map[string]teamRoleNode) (string, bool) {
	var coordinators []string
	for name, node := range nodes {
		if node.coordinator {
			coordinators = append(coordinators, name)
		}
	}
	switch {
	case len(coordinators) == 1:
		return coordinators[0], true
	case len(coordinators) > 1:
		return "", false // ambiguous: multiple declared drivers, no single root
	case len(nodes) == 1:
		for name := range nodes {
			return name, true
		}
	}
	if _, ok := nodes["ceo"]; ok {
		return "ceo", true
	}
	return "", false
}

// delegationCycle returns one Role on a delegation cycle, or "" when the
// process-delegation graph is acyclic. Self-loops count: a process stage
// executed by the defining Role is the degenerate cycle.
func delegationCycle(delegation map[string][]string) string {
	const (
		white = iota
		gray
		black
	)
	color := make(map[string]int, len(delegation))
	var cycleAt string
	var visit func(string) bool
	visit = func(n string) bool {
		color[n] = gray
		for _, m := range delegation[n] {
			switch color[m] {
			case gray:
				cycleAt = m
				return true
			case white:
				if visit(m) {
					return true
				}
			}
		}
		color[n] = black
		return false
	}
	for n := range delegation {
		if color[n] == white && visit(n) {
			return cycleAt
		}
	}
	return ""
}
