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

package webhook

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// Guard ids for the cross-object existence checks. Each id names one
// load-bearing admission guard; the falsification suite disables them one
// at a time and asserts the matching invalid case flips reject→admit,
// proving no guard is dead code.
const (
	GuardTeamProjects = "team/projects"
	GuardTeamAgents   = "team/agents"
	GuardAgentRuntime = "agent/runtimeRef"
	GuardAgentRole    = "agent/roleRef"
	GuardAgentSkills  = "agent/skillRefs"
	GuardAgentSecret  = "agent/credentialSecretRef"
	GuardRunTeam      = "run/teamRef"
	GuardRunProject   = "run/projectRef"
	GuardRunAgents    = "run/agents"
)

// CrossRefValidator enforces the cross-object existence invariants the CRD
// schemas cannot express: CEL in structural schemas evaluates against
// `self` only, so a cross-object rule written as CEL would be a silent
// no-op that admits the dangling ref (story 1.3 design note). Every check
// therefore lives here, in a ValidatingAdmissionWebhook.
type CrossRefValidator struct {
	// Reader fetches referenced objects at admission time.
	Reader client.Reader

	// DisabledGuards turns individual guards off. It exists for the
	// falsification suite (delete-a-guard tests) and MUST stay empty in
	// production wiring.
	DisabledGuards map[string]bool
}

func (v *CrossRefValidator) on(guard string) bool {
	return !v.DisabledGuards[guard]
}

// resolveNamespace implements the ObjectRef contract: an empty Namespace
// means the referring object's own namespace.
func resolveNamespace(ref ksquadv1alpha1.ObjectRef, ownNamespace string) string {
	if ref.Namespace != "" {
		return ref.Namespace
	}
	return ownNamespace
}

// refExists reports whether the referenced namespaced object exists.
func refExists[T client.Object](ctx context.Context, r client.Reader, obj T, namespace, name string) (bool, error) {
	obj.SetNamespace(namespace)
	obj.SetName(name)
	err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj)
	if err == nil {
		return true, nil
	}
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	// Transient API errors fail closed: admission is denied rather than
	// letting a dangling ref through on a read error.
	return false, err
}

// ValidateTeam rejects a Team whose spec references a missing Project or a
// missing Agent. Each denial carries the field path, the observed ref and
// the fix (story 1.3 AC: "Team referencing missing Project").
func (v *CrossRefValidator) ValidateTeam(ctx context.Context, team *ksquadv1alpha1.Team) field.ErrorList {
	var errs field.ErrorList
	if v.on(GuardTeamProjects) {
		for i, ref := range team.Spec.Projects {
			ns := resolveNamespace(ref, team.Namespace)
			ok, err := refExists(ctx, v.Reader, &ksquadv1alpha1.Project{}, ns, ref.Name)
			switch {
			case err != nil:
				errs = append(errs, invalidf(fmt.Sprintf("spec.projects[%d]", i), ref, "admission read failed (fail-closed): %v; retry or check apiserver health", err))
				continue
			case !ok:
				errs = append(errs, invalidf(fmt.Sprintf("spec.projects[%d]", i), ref, "referenced Project %s/%s does not exist; create it first or fix the ref", ns, ref.Name))
			}
		}
	}
	if v.on(GuardTeamAgents) {
		for i, ref := range team.Spec.Agents {
			ns := resolveNamespace(ref, team.Namespace)
			ok, err := refExists(ctx, v.Reader, &ksquadv1alpha1.Agent{}, ns, ref.Name)
			switch {
			case err != nil:
				errs = append(errs, invalidf(fmt.Sprintf("spec.agents[%d]", i), ref, "admission read failed (fail-closed): %v", err))
				continue
			case !ok:
				errs = append(errs, invalidf(fmt.Sprintf("spec.agents[%d]", i), ref, "referenced Agent %s/%s does not exist; create it first or fix the ref", ns, ref.Name))
			}
		}
	}
	return errs
}

// ValidateAgent rejects an Agent whose runtimeRef, roleRef, skillRefs or
// credentialSecretRef dangle. This is the webhook half of the ticket's
// "Agent.runtime" example: the runtime TYPE discipline is CEL on the
// AgentRuntime CRD (FR-D3); the runtime REF existence is checked here.
func (v *CrossRefValidator) ValidateAgent(ctx context.Context, agent *ksquadv1alpha1.Agent) field.ErrorList {
	var errs field.ErrorList
	if v.on(GuardAgentRuntime) {
		ref := agent.Spec.RuntimeRef
		ns := resolveNamespace(ref, agent.Namespace)
		ok, err := refExists(ctx, v.Reader, &ksquadv1alpha1.AgentRuntime{}, ns, ref.Name)
		switch {
		case err != nil:
			errs = append(errs, invalidf("spec.runtimeRef", ref, "admission read failed (fail-closed): %v", err))
		case !ok:
			errs = append(errs, invalidf("spec.runtimeRef", ref, "referenced AgentRuntime %s/%s does not exist; register the runtime first (FR-D3: non-conformant types need experimental=true)", ns, ref.Name))
		}
	}
	if v.on(GuardAgentRole) {
		ref := agent.Spec.RoleRef
		ns := resolveNamespace(ref, agent.Namespace)
		ok, err := refExists(ctx, v.Reader, &ksquadv1alpha1.Role{}, ns, ref.Name)
		switch {
		case err != nil:
			errs = append(errs, invalidf("spec.roleRef", ref, "admission read failed (fail-closed): %v", err))
		case !ok:
			errs = append(errs, invalidf("spec.roleRef", ref, "referenced Role %s/%s does not exist; create it first or fix the ref", ns, ref.Name))
		}
	}
	if v.on(GuardAgentSkills) {
		for i, ref := range agent.Spec.SkillRefs {
			ns := resolveNamespace(ref, agent.Namespace)
			ok, err := refExists(ctx, v.Reader, &ksquadv1alpha1.Skill{}, ns, ref.Name)
			switch {
			case err != nil:
				errs = append(errs, invalidf(fmt.Sprintf("spec.skillRefs[%d]", i), ref, "admission read failed (fail-closed): %v", err))
				continue
			case !ok:
				errs = append(errs, invalidf(fmt.Sprintf("spec.skillRefs[%d]", i), ref, "referenced Skill %s/%s does not exist; grant it via an existing Skill or drop the ref", ns, ref.Name))
			}
		}
	}
	if v.on(GuardAgentSecret) {
		ref := agent.Spec.CredentialSecretRef
		ok, err := refExists(ctx, v.Reader, &corev1.Secret{}, agent.Namespace, ref.Name)
		switch {
		case err != nil:
			errs = append(errs, invalidf("spec.credentialSecretRef", ref, "admission read failed (fail-closed): %v", err))
		case !ok:
			errs = append(errs, invalidf("spec.credentialSecretRef", ref, "referenced Secret %s/%s does not exist; create the BYO credential Secret (arch §11) first", agent.Namespace, ref.Name))
		}
	}
	return errs
}

// ValidateRun rejects a Run whose teamRef, projectRef or agent selectors
// dangle (story 1.3 AC: "Run referencing missing Team/Project").
func (v *CrossRefValidator) ValidateRun(ctx context.Context, run *ksquadv1alpha1.Run) field.ErrorList {
	var errs field.ErrorList
	if v.on(GuardRunTeam) {
		ref := run.Spec.TeamRef
		ns := resolveNamespace(ref, run.Namespace)
		ok, err := refExists(ctx, v.Reader, &ksquadv1alpha1.Team{}, ns, ref.Name)
		switch {
		case err != nil:
			errs = append(errs, invalidf("spec.teamRef", ref, "admission read failed (fail-closed): %v", err))
		case !ok:
			errs = append(errs, invalidf("spec.teamRef", ref, "referenced Team %s/%s does not exist; a Run must execute under an existing squad's tenancy (§12.1)", ns, ref.Name))
		}
	}
	if v.on(GuardRunProject) {
		ref := run.Spec.ProjectRef
		ns := resolveNamespace(ref, run.Namespace)
		ok, err := refExists(ctx, v.Reader, &ksquadv1alpha1.Project{}, ns, ref.Name)
		switch {
		case err != nil:
			errs = append(errs, invalidf("spec.projectRef", ref, "admission read failed (fail-closed): %v", err))
		case !ok:
			errs = append(errs, invalidf("spec.projectRef", ref, "referenced Project %s/%s does not exist; create the Project (repo + workspace PVC) first", ns, ref.Name))
		}
	}
	if v.on(GuardRunAgents) {
		for i, ref := range run.Spec.Agents {
			ns := resolveNamespace(ref, run.Namespace)
			ok, err := refExists(ctx, v.Reader, &ksquadv1alpha1.Agent{}, ns, ref.Name)
			switch {
			case err != nil:
				errs = append(errs, invalidf(fmt.Sprintf("spec.agents[%d]", i), ref, "admission read failed (fail-closed): %v", err))
				continue
			case !ok:
				errs = append(errs, invalidf(fmt.Sprintf("spec.agents[%d]", i), ref, "referenced Agent %s/%s does not exist; fix the selector or create the Agent", ns, ref.Name))
			}
		}
	}
	return errs
}

// invalidf is field.Invalid with a printf-shaped detail (fix) message, so
// every denial renders as path + observed value + fix.
func invalidf(path string, observed any, format string, args ...any) *field.Error {
	return field.Invalid(field.NewPath(path), observed, fmt.Sprintf(format, args...))
}
