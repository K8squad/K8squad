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

package run

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/contextasm"
	"github.com/K8squad/K8squad/pkg/controller/contextsource"
	"github.com/K8squad/K8squad/pkg/modelendpoint"
	"github.com/K8squad/K8squad/pkg/roleprompt"
	"github.com/K8squad/K8squad/pkg/teamroster"
)

// ContextAssemblers builds a per-namespace §8.5 context assembler over the
// production Sources (coord DB + Project CRD + memory service). The Run
// reconciler uses it to assemble+pin the context snapshot at Claiming →
// Running; the dispatcher uses the same seam to re-read the pinned snapshot.
// pkg/controller/contextsource.Deps implements this; tests fake it.
type ContextAssemblers interface {
	// For returns an assembler whose Sources resolve the Project CRD in
	// namespace (coord reads are namespace-agnostic, ADR-001).
	For(namespace string) *contextasm.Assembler
}

// ensureContextSnapshot assembles the §8.5 context envelope and pins its
// resolved-input snapshot on desired.ContextSnapshot (story S1, ISI-3600).
//
// It is a no-op when the side-channel is disabled (nil ContextAssemblers,
// non-regressing), when the Run is not yet being dispatched (phase is not
// Claiming/Running), or when a snapshot is already pinned (immutable for the
// Run's life — a re-drive reuses it, which is what makes resume
// deterministic). A Run with no dispatch agent is skipped (no model → no
// resolvable window); a declared-but-unreadable agent/Project fails closed.
func (r *Reconciler) ensureContextSnapshot(ctx context.Context, run *api.Run, desired *api.RunStatus) error {
	if r.ContextAssemblers == nil {
		return nil
	}
	if desired.Phase != api.RunPhaseClaiming && desired.Phase != api.RunPhaseRunning {
		return nil
	}
	if desired.ContextSnapshot != nil {
		return nil
	}
	if len(run.Spec.Agents) == 0 {
		return nil // no model to key the contextWindow off; nothing to assemble
	}

	agentRef := run.Spec.Agents[0]
	ns := agentRef.Namespace
	if ns == "" {
		ns = run.Namespace
	}
	var agent api.Agent
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: agentRef.Name}, &agent); err != nil {
		return fmt.Errorf("read Agent %s/%s for run %s/%s context assembly: %w", ns, agentRef.Name, run.Namespace, run.Name, err)
	}

	projNS := run.Spec.ProjectRef.Namespace
	if projNS == "" {
		projNS = run.Namespace
	}
	var project api.Project
	if err := r.Get(ctx, client.ObjectKey{Namespace: projNS, Name: run.Spec.ProjectRef.Name}, &project); err != nil {
		return fmt.Errorf("read Project %s/%s for run %s/%s context assembly: %w", projNS, run.Spec.ProjectRef.Name, run.Namespace, run.Name, err)
	}

	// M1.2 (ISI-4128): scoped memory recall keys on the team's Postgres uuid
	// (coord.work_item.team_id is the Team CR uid) — the Team CR name is not a
	// uuid and fails the scoped-recall query. Resolve the Team CR and pass
	// its uid; a declared-but-unreadable team fails closed like agent/project.
	teamNS := run.Spec.TeamRef.Namespace
	if teamNS == "" {
		teamNS = run.Namespace
	}
	var team api.Team
	if err := r.Get(ctx, client.ObjectKey{Namespace: teamNS, Name: run.Spec.TeamRef.Name}, &team); err != nil {
		return fmt.Errorf("read Team %s/%s for run %s/%s context assembly: %w", teamNS, run.Spec.TeamRef.Name, run.Namespace, run.Name, err)
	}
	// ISI-5223: resolve the agent's Role behavior prompt (Role.Spec.PromptRef)
	// and pass it through so the snapshot path's must-include budget check agrees
	// with the dispatcher's (which injects the same prompt as a must-include
	// element). A nil role / no prompt yields "" (unchanged); a transient read
	// error fails closed exactly like the agent/project/team reads above.
	role, err := r.resolveRole(ctx, &agent)
	if err != nil {
		return err
	}

	// ISI-5540: resolve the context window from the live effective endpoint. A
	// BYO endpoint that DECLARES its context window (modelendpoint contextWindow
	// Secret key) is authoritative — the hosted deepseek endpoint serves 128K,
	// not the catalog's conservative 64K floor. No declaration → the
	// family-catalog default (under-budget safe). Best-effort: an endpoint
	// resolution glitch falls back to the agent-model family default rather than
	// failing the snapshot (AC6 no-regression). This pins into the snapshot; a
	// resume reuses the pinned window, never re-resolving.
	window := contextsource.WindowForModel(agent.Spec.Model)
	resolver := modelendpoint.Resolver{Reader: r.Client}
	if ep, _, _, _, rerr := resolver.ResolveEffective(ctx, &agent, role); rerr == nil {
		window = contextsource.WindowFor(ep.Model, ep.ContextWindow)
	}

	rolePromptNS := agent.Namespace
	if role != nil {
		rolePromptNS = role.Namespace
	}
	rolePrompt, err := roleprompt.Resolve(ctx, r.Client, role, rolePromptNS)
	if err != nil {
		return fmt.Errorf("resolve role prompt for run %s/%s context assembly: %w", run.Namespace, run.Name, err)
	}

	// ISI-5245: resolve the team roster (assignable agent NAME ↔ role) so a
	// coordinator can name a valid assignee for work_item_assign. Fails closed on
	// a transient read exactly like the agent/project/team reads above; an empty
	// team yields "" (unchanged). Same wiring as the role prompt above, and the
	// dispatcher injects the identical fact so the snapshot and dispatch renders
	// agree.
	teamRoster, err := teamroster.Resolve(ctx, r.Client, &team, teamNS)
	if err != nil {
		return fmt.Errorf("resolve team roster for run %s/%s context assembly: %w", run.Namespace, run.Name, err)
	}

	// Resolve the Project CRD in the projectRef's namespace (honors a
	// cross-namespace projectRef), not the Run's own namespace.
	res, err := r.ContextAssemblers.For(projNS).Assemble(ctx, contextasm.AssembleRequest{
		Run:           run,
		Agent:         &agent,
		Project:       &project,
		TeamID:        string(team.UID),
		ContextWindow: window,
		RolePrompt:    rolePrompt,
		TeamRoster:    teamRoster,
	})
	if err != nil {
		return fmt.Errorf("assemble context for run %s/%s: %w", run.Namespace, run.Name, err)
	}

	snap := res.Snapshot
	now := metav1.Now()
	if r.Now != nil {
		now = r.Now()
	}
	snap.AssembledAt = &now
	desired.ContextSnapshot = snap
	return nil
}

// resolveRole resolves the Agent's Role via spec.roleRef for role-prompt
// injection (ISI-5223). An empty roleRef, or a role deleted after admission,
// contributes no role (nil) rather than failing the snapshot — a dangling
// roleRef is admission's rejection to own, and a role removed later simply
// injects no behavior prompt. Only a TRANSIENT read error fails closed, so a
// lookup glitch can never silently drop a coordinator's orchestration prompt.
// Mirrors rundrive.operatorDispatch.roleFor so both assembly paths resolve the
// same role.
func (r *Reconciler) resolveRole(ctx context.Context, agent *api.Agent) (*api.Role, error) {
	if agent.Spec.RoleRef.Name == "" {
		return nil, nil
	}
	ns := agent.Spec.RoleRef.Namespace
	if ns == "" {
		ns = agent.Namespace
	}
	var role api.Role
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: agent.Spec.RoleRef.Name}, &role); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("resolve Role %s/%s for Agent %s: %w", ns, agent.Spec.RoleRef.Name, agent.Name, err)
	}
	return &role, nil
}
