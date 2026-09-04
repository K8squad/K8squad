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

// Package capability implements Run assembly (plan §2.3, ADR-044): the
// requirement-union algorithm that folds a Run's agents' skills into one
// capability envelope — toolchains to stage, MCP servers to wire, sidecars
// to gate — and renders it onto the sandbox pod and Run.status.
//
// Everything here is fail-closed by construction: an envelope that cannot
// be computed exactly is an error, never a silent partial grant.
package capability

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// Requirements is a Run's unioned capability demand (ADR-044 steps 1–2):
// the toolchains, MCP servers and sidecars its skills require, deduped
// with first-seen order and per-item provenance for actionable errors.
type Requirements struct {
	// ToolchainRefs are the unique name@version strings the Run's skills
	// require (resolution and conflict handling live in pkg/toolchain).
	ToolchainRefs []string

	// ToolchainSources maps each toolchain ref to the first skill
	// (ns/name) that required it — denial messages name the demander.
	ToolchainSources map[string]string

	// MCPRefs are the unique MCP server refs the Run's skills grant.
	MCPRefs []api.ObjectRef

	// MCPSources maps each MCP ref key ("ns/name") to the first skill
	// that referenced it.
	MCPSources map[string]string

	// Sidecars is the union of Skill.spec.requires.sidecars (deduped,
	// first-seen order) — capability-gated service sidecars (§5.3.3).
	Sidecars []string
}

func (r *Requirements) mcpKey(ref api.ObjectRef, defaultNS string) string {
	ns := ref.Namespace
	if ns == "" {
		ns = defaultNS
	}
	return ns + "/" + ref.Name
}

// Collect walks Run → Agents (spec.skillRefs ∪ Role.spec.defaultSkills) →
// Skills and unions the capability demand (ADR-044 step 1). Missing
// agents/skills/roles are NOT errors here: the story 1.3 cross-ref guards
// already reject dangling refs at admission, and assembly only needs the
// demand of what resolves (same posture as toolchain.RefsForRun). Read
// failures are errors (fail-closed).
func Collect(ctx context.Context, reader client.Reader, run *api.Run) (*Requirements, error) {
	reqs := &Requirements{
		ToolchainSources: map[string]string{},
		MCPSources:       map[string]string{},
	}
	seenSkills := map[string]bool{}

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
			return nil, fmt.Errorf("read agent %s/%s for capability assembly (fail-closed): %w", agentNS, agentRef.Name, err)
		}

		// The agent's Role contributes its defaultSkills to the union
		// (ADR-044 step 1: Agent.spec.skillRefs ∪ Role.spec.defaultSkills).
		skillRefs := append([]api.ObjectRef(nil), agent.Spec.SkillRefs...)
		if roleRef := agent.Spec.RoleRef; roleRef.Name != "" {
			roleNS := roleRef.Namespace
			if roleNS == "" {
				roleNS = agent.Namespace
			}
			var role api.Role
			if err := reader.Get(ctx, client.ObjectKey{Namespace: roleNS, Name: roleRef.Name}, &role); err != nil {
				if !isNotFound(err) {
					return nil, fmt.Errorf("read role %s/%s for capability assembly (fail-closed): %w", roleNS, roleRef.Name, err)
				}
			} else {
				skillRefs = append(skillRefs, role.Spec.DefaultSkills...)
			}
		}

		for _, skillRef := range skillRefs {
			skillNS := skillRef.Namespace
			if skillNS == "" {
				skillNS = agent.Namespace
			}
			skillKey := skillNS + "/" + skillRef.Name
			if seenSkills[skillKey] {
				continue
			}
			var skill api.Skill
			if err := reader.Get(ctx, client.ObjectKey{Namespace: skillNS, Name: skillRef.Name}, &skill); err != nil {
				if isNotFound(err) {
					continue
				}
				return nil, fmt.Errorf("read skill %s for capability assembly (fail-closed): %w", skillKey, err)
			}
			seenSkills[skillKey] = true

			for _, ref := range skill.Spec.Requires.Toolchains {
				if _, seen := reqs.ToolchainSources[ref]; !seen {
					reqs.ToolchainRefs = append(reqs.ToolchainRefs, ref)
					reqs.ToolchainSources[ref] = skillKey
				}
			}
			for _, ref := range skill.Spec.McpToolRefs {
				key := reqs.mcpKey(ref, skill.Namespace)
				if _, seen := reqs.MCPSources[key]; !seen {
					reqs.MCPRefs = append(reqs.MCPRefs, api.ObjectRef{
						Namespace: ref.Namespace,
						Name:      ref.Name,
					})
					reqs.MCPSources[key] = skillKey
				}
			}
			for _, sidecar := range skill.Spec.Requires.Sidecars {
				if !containsString(reqs.Sidecars, sidecar) {
					reqs.Sidecars = append(reqs.Sidecars, sidecar)
				}
			}
		}
	}
	return reqs, nil
}

<<<<<<< Updated upstream
=======
// GrantedSkill is copied from api/v1alpha1 to avoid circular imports.
// Records a skill's identity, source type, and permissions for capability 
// assembly (ADR-0004 Phase 1 keystone). Permissions are copied verbatim from 
// Skill.spec.permissions ONLY (never from body content — AC4/AC7 D8).
type GrantedSkill = api.GrantedSkill

>>>>>>> Stashed changes
func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
