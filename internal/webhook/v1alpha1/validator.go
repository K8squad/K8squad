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
	"errors"
	"fmt"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/credinject"
	"github.com/K8squad/K8squad/pkg/modelendpoint"
	"github.com/K8squad/K8squad/pkg/sandbox"
	"github.com/K8squad/K8squad/pkg/toolchain"
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
	// #nosec G101 -- guard path key naming the field it guards, not a credential
	GuardAgentSecret = "agent/credentialSecretRef"
	// GuardAgentCredentialClass validates spec.credentialClass against the
	// credential injection contract's taxonomy (story 5.4, pkg/credinject):
	// an unknown class must fail at ADMISSION, never reach the injection seam
	// where an unmapped class would strand a Run with no runtime-native
	// credential env.
	// #nosec G101 -- guard path key naming the field it guards, not a credential
	GuardAgentCredentialClass = "agent/credentialClass"
	// GuardAgentModelEndpoint guards the BYO model-endpoint Secret
	// (spec.modelEndpointRef, §10.3 seam / stories 5.7+5.11): it must exist
	// AND carry the 7.5 shape (endpointURL key, parseable http(s) URL) — a
	// mis-shaped endpoint must fail at ADMISSION, never silently
	// mid-Run (5.7 AC).
	GuardAgentModelEndpoint = "agent/modelEndpointRef"
	// GuardAgentFallbackModelEndpoint guards the fallback's own endpoint
	// Secret (spec.fallbackModel.modelEndpointRef, story 5.11) with the
	// same existence + shape discipline.
	GuardAgentFallbackModelEndpoint = "agent/fallbackModel.modelEndpointRef"
	GuardRunTeam                    = "run/teamRef"
	GuardRunProject                 = "run/projectRef"
	GuardRunAgents                  = "run/agents"
	// GuardSkillMCPServers guards Skill.spec.mcpToolRefs (story A2 /
	// ADR-042): every ref must resolve to an existing MCPServer. The old
	// schema-only world admitted dangling refs silently because no target
	// CRD existed; the capability plane fails closed instead.
	GuardSkillMCPServers = "skill/mcpToolRefs"
	// GuardToolchainCatalog guards Toolchain admission (Epic B / ISI-3286,
	// plan §2.2b trust boundary): team-namespace Toolchains may only
	// narrow an existing cluster-catalog entry, wildcards are rejected,
	// and cluster-scope RBAC needs the explicit platform opt-in.
	GuardToolchainCatalog = "toolchain/catalog"
	// GuardRunToolchains guards Run admission's toolchain resolution
	// (story B2, plan §2.2): every name@version ref the Run's skills
	// require must resolve in the team namespace override or the cluster
	// catalog, and a Run's skills must pin one version per toolchain.
	// Unknown name/version and version conflicts fail closed — the same
	// resolver Run assembly uses, so what admission proved is what
	// dispatch assumes.
	GuardRunToolchains = "run/toolchains"
	// GuardRunTrustedDev gates WHO may set the sandbox trusted-dev escape
	// annotation: platform operators only. Without this guard the
	// annotation is a plain metadata write any Run author can set,
	// which would hand the shared-kernel escape to untrusted users.
	GuardRunTrustedDev = "run/trusted-dev-annotation"
)

// controlPlaneNamespace is the ksquad control-plane namespace whose service
// accounts count as platform operators for the trusted-dev escape. It
// mirrors pkg/controller/team.SystemNamespace locally (sandbox.go keeps the
// same class of local mirror to avoid dragging controller deps in).
const controlPlaneNamespace = "ksquad-system"

// trustedDevSetterServiceAccounts is the explicit allowlist of identities
// that may set the trusted-dev escape annotation (F5). The pre-F5 prefix
// check ("any system:serviceaccount:ksquad-system:*") treated EVERY
// workload in the control-plane namespace — any current or future one — as
// a privileged setter; a single compromised or over-permissioned
// control-plane pod would have been enough to hand out the shared-kernel
// escape. Adding a setter means adding a name here, deliberately, in review.
var trustedDevSetterServiceAccounts = map[string]bool{
	"system:serviceaccount:" + controlPlaneNamespace + ":operator": true,
}

// CrossRefValidator enforces the cross-object existence invariants the CRD
// schemas cannot express: CEL in structural schemas evaluates against
// `self` only, so a cross-object rule written as CEL would be a silent
// no-op that admits the dangling ref (story 1.3 design note). Every check
// therefore lives here, in a ValidatingAdmissionWebhook.
type CrossRefValidator struct {
	// Reader fetches referenced objects at admission time.
	Reader client.Reader

	// Toolchains carries the platform-side toolchain configuration
	// (cluster catalog namespace, cluster-scope opt-in). The zero value
	// defaults to the standard control-plane namespace with cluster scope
	// off; production wiring injects the deployment env (Helm
	// tools.rbac.clusterScopeEnabled).
	Toolchains toolchain.PlatformConfig

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
	if v.on(GuardAgentCredentialClass) {
		if err := credinject.ValidateClass(credinject.CredentialClass(agent.Spec.CredentialClass)); err != nil {
			errs = append(errs, invalidf("spec.credentialClass", agent.Spec.CredentialClass, "%v", err))
		}
	}
	if v.on(GuardAgentModelEndpoint) && agent.Spec.ModelEndpointRef != nil {
		if err := v.validateEndpointRef(ctx, agent.Namespace, agent.Spec.ModelEndpointRef, "spec.modelEndpointRef"); err != nil {
			errs = append(errs, err)
		}
	}
	if v.on(GuardAgentFallbackModelEndpoint) && agent.Spec.FallbackModel != nil && agent.Spec.FallbackModel.ModelEndpointRef != nil {
		if err := v.validateEndpointRef(ctx, agent.Namespace, agent.Spec.FallbackModel.ModelEndpointRef, "spec.fallbackModel.modelEndpointRef"); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// validateEndpointRef runs the §10.3 model-endpoint resolution
// (pkg/modelendpoint, story 5.7 + 7.5 shape) against one endpoint Secret
// ref and converts its fail-closed verdict into an admission denial. An
// endpoint that would strand a Run mid-flight (dangling Secret, missing
// endpointURL key, malformed URL) is rejected HERE — the same resolver the
// dispatch path uses, so what admission proved is what dispatch assumes.
func (v *CrossRefValidator) validateEndpointRef(ctx context.Context, namespace string, ref *ksquadv1alpha1.SecretRef, path string) *field.Error {
	r := modelendpoint.Resolver{Reader: v.Reader}
	_, err := r.ResolveRef(ctx, namespace, ref, "")
	var unresolved *modelendpoint.ErrUnresolved
	if errors.As(err, &unresolved) {
		return invalidf(path, ref.Name, "%s; fix the endpoint Secret (endpointURL + optional apiToken, story 7.5 shape) and retry", unresolved.Reason)
	}
	if err != nil {
		return invalidf(path, ref.Name, "admission read failed (fail-closed): %v", err)
	}
	return nil
}

// ValidateSkill rejects a Skill whose spec.mcpToolRefs target a missing
// MCPServer (story A2, ADR-042 webhook contract, fail-closed). Refs resolve
// in the ref's explicit namespace or the Skill's own namespace. The denial
// message names both endpoints of the dangling ref so the fix is actionable:
// create the MCPServer first or drop the ref.
func (v *CrossRefValidator) ValidateSkill(ctx context.Context, skill *ksquadv1alpha1.Skill) field.ErrorList {
	var errs field.ErrorList
	if !v.on(GuardSkillMCPServers) {
		return errs
	}
	for i, ref := range skill.Spec.McpToolRefs {
		ns := resolveNamespace(ref, skill.Namespace)
		ok, err := refExists(ctx, v.Reader, &ksquadv1alpha1.MCPServer{}, ns, ref.Name)
		switch {
		case err != nil:
			errs = append(errs, invalidf(fmt.Sprintf("spec.mcpToolRefs[%d]", i), ref,
				"admission read failed (fail-closed): %v; retry or check apiserver health", err))
			continue
		case !ok:
			errs = append(errs, invalidf(fmt.Sprintf("spec.mcpToolRefs[%d]", i), ref,
				"skill %s/%s references missing MCPServer %s/%s; create the MCPServer first or drop the ref",
				skill.Namespace, skill.Name, ns, ref.Name))
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
	errs = append(errs, v.validateRunToolchains(ctx, run)...)
	return errs
}

// validateRunToolchains resolves the Run's full toolchain demand — its
// agents' skills' spec.requires.toolchains — against the catalog through
// the SAME resolver Run assembly uses (story B2, plan §2.2 fail-closed):
// unknown names, unknown versions, version conflicts and narrow-only
// boundary violations reject the Run with actionable messages naming the
// demanding skill.
func (v *CrossRefValidator) validateRunToolchains(ctx context.Context, run *ksquadv1alpha1.Run) field.ErrorList {
	if !v.on(GuardRunToolchains) {
		return nil
	}
	resolver := &toolchain.Resolver{Reader: v.Reader, Platform: v.Toolchains}
	reqs, err := resolver.RefsForRun(ctx, run)
	if err != nil {
		// Read failures fail closed — same posture as every other guard.
		return field.ErrorList{invalidf("spec.agents", run.Spec.Agents, "toolchain resolution read failed (fail-closed): %v", err)}
	}
	if len(reqs.Refs) == 0 {
		return nil
	}
	if _, err := resolver.ResolveRefs(ctx, run.Namespace, reqs.Refs, toolchain.DetailsFor(run)); err != nil {
		path := "spec.agents"
		var observed any = reqs.Refs
		if ref := firstRefByError(err, reqs); ref != "" {
			if skill := reqs.Sources[ref]; skill != "" {
				path = "spec.agents.skills.requires.toolchains"
				observed = ref + " (skill " + skill + ")"
			} else {
				observed = ref
			}
		}
		return field.ErrorList{invalidf(path, observed, "%v; enable the default catalog (tools.defaultCatalog.enabled), define the Toolchain, or align the skills' version pins", err)}
	}
	return nil
}

// firstRefByError extracts the ref a resolver error points at, so the
// denial's observed value names the exact demanding requirement.
func firstRefByError(err error, reqs *toolchain.RunRequirements) string {
	switch e := err.(type) {
	case *toolchain.UnknownError:
		return e.Ref
	case *toolchain.VersionError:
		return e.Ref
	case *toolchain.ConflictError:
		return e.Name + toolchain.RefSeparator + e.Have
	case *toolchain.TrustError:
		for _, ref := range reqs.Refs {
			if reqs.Sources[ref] != "" {
				return ref
			}
		}
	}
	return ""
}

// invalidf is field.Invalid with a printf-shaped detail (fix) message, so
// every denial renders as path + observed value + fix.
func invalidf(path string, observed any, format string, args ...any) *field.Error {
	return field.Invalid(field.NewPath(path), observed, fmt.Sprintf(format, args...))
}

// ValidateRunTrustedDev gates the ksquad.io/trusted-dev escape (story 4.2
// §2: "explicit, audited, non-default"). Setting it must be a privileged,
// deliberate act: only platform operators (service accounts in the
// control-plane namespace or system:masters) may set or change it. On
// update, an unchanged carry-over from an already-annotated Run is not a
// new act and passes — the gate stops ESCALATION, not ordinary edits.
// Absent request user info (non-admission callers, tests) fails closed:
// anonymous is unprivileged.
func (v *CrossRefValidator) ValidateRunTrustedDev(run, old *ksquadv1alpha1.Run, userInfo authenticationv1.UserInfo) field.ErrorList {
	if !v.on(GuardRunTrustedDev) {
		return nil
	}
	value, set := run.Annotations[sandbox.TrustedDevAnnotation]
	if !set {
		return nil
	}
	if old != nil && old.Annotations[sandbox.TrustedDevAnnotation] == value {
		return nil
	}
	if isPrivilegedRequester(userInfo) {
		return nil
	}
	var errs field.ErrorList
	errs = append(errs, invalidf(
		"metadata.annotations["+sandbox.TrustedDevAnnotation+"]", value,
		"the trusted-dev escape admits the shared-kernel runtime and may only be set by platform operators (service accounts in %s, or system:masters); ask an operator to set it on your behalf", controlPlaneNamespace))
	return errs
}

// isPrivilegedRequester reports whether the admission requester may set
// the trusted-dev escape: a service account on the explicit
// trustedDevSetterServiceAccounts allowlist (F5) or system:masters.
// Anything else in the control-plane namespace is NOT privileged — the
// namespace is not the grant.
func isPrivilegedRequester(userInfo authenticationv1.UserInfo) bool {
	if trustedDevSetterServiceAccounts[userInfo.Username] {
		return true
	}
	for _, group := range userInfo.Groups {
		if group == "system:masters" {
			return true
		}
	}
	return false
}
