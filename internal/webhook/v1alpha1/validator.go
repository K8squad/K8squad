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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/credinject"
	"github.com/K8squad/K8squad/pkg/modelendpoint"
	"github.com/K8squad/K8squad/pkg/sandbox"
	"github.com/K8squad/K8squad/pkg/toolchain"
	"github.com/K8squad/K8squad/pkg/toolcred"
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
	// GuardAgentToolCredentials validates spec.toolCredentials (ISI-3565,
	// pkg/toolcred): each aux credential must name a KNOWN purpose and a
	// non-empty Secret — an unknown purpose or dangling Secret must fail at
	// ADMISSION, never reach the AssemblePod injection seam where it would
	// strand a Run whose gh/git authenticates as nobody (symmetric to
	// GuardAgentCredentialClass + GuardAgentSecret for the model credential).
	// #nosec G101 -- guard path key naming the field it guards, not a credential
	GuardAgentToolCredentials = "agent/toolCredentials"
	GuardRunTeam              = "run/teamRef"
	GuardRunProject           = "run/projectRef"
	GuardRunAgents            = "run/agents"
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
	// GuardRunToolCredentials guards Run.spec.toolCredentials (ISI-3565):
	// the projected aux credentials must (a) pass the same enum/existence/
	// no-duplicate checks as the Agent path, AND (b) be DERIVED from the
	// Run's own Agents — every Run aux credential must be declared by one of
	// the Run's spec.agents with a matching (purpose, Secret). Without (b) a
	// Run author could mount ANY same-namespace Secret as GH_TOKEN, bypassing
	// Agent admission (Copilot review, PR#230). Aux credentials on a Run that
	// names no Agents are un-derivable and rejected fail-closed.
	// #nosec G101 -- guard path key naming the field it guards, not a credential
	GuardRunToolCredentials = "run/toolCredentials"
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
		// Metadata-only existence check: the guard only needs to know the BYO
		// credential Secret exists, never its bytes. A full typed Secret Get
		// here would deserialize token plaintext into the control plane,
		// contradicting the no-plaintext boundary (symmetric with the
		// tool-credential check above; the reader is uncached — ISI-3565).
		ok, err := v.secretMetadataExists(ctx, agent.Namespace, ref.Name)
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
	if v.on(GuardAgentToolCredentials) {
		errs = append(errs, v.validateToolCredentials(ctx, agent.Namespace, agent.Spec.ToolCredentials, "spec.toolCredentials")...)
	}
	return errs
}

// validateToolCredentials is the shared admission check for an aux-credential
// list (ISI-3565), used by BOTH Agent and Run so what one admits the other
// admits identically. Each entry must (a) name a KNOWN purpose
// (pkg/toolcred), (b) carry a non-empty Secret that resolves, and (c) not
// duplicate another entry's purpose — a duplicate purpose is admitted-but-
// undispatchable, because toolcred.Inject would emit GH_TOKEN/GITHUB_TOKEN
// twice and AssemblePod's collision guard would then reject the pod. All
// three fail CLOSED at admission rather than at the injection seam.
func (v *CrossRefValidator) validateToolCredentials(ctx context.Context, namespace string, creds []ksquadv1alpha1.ToolCredential, pathPrefix string) field.ErrorList {
	var errs field.ErrorList
	seen := map[string]int{}
	for i, tc := range creds {
		path := fmt.Sprintf("%s[%d]", pathPrefix, i)
		// Enum-validate the purpose against the injection contract's
		// taxonomy: an unknown purpose must fail here, not at the AssemblePod
		// seam where it would strand a Run.
		if err := toolcred.ValidatePurpose(toolcred.Purpose(tc.Purpose)); err != nil {
			errs = append(errs, invalidf(path+".purpose", tc.Purpose, "%v", err))
		} else if prev, dup := seen[tc.Purpose]; dup {
			// Known purpose repeated: reject the second occurrence. Injecting
			// the same purpose twice collides on GH_TOKEN/GITHUB_TOKEN and
			// makes the admitted object undispatchable.
			errs = append(errs, invalidf(path+".purpose", tc.Purpose, "duplicate tool-credential purpose %q (already declared at %s[%d]); declare each purpose at most once", tc.Purpose, pathPrefix, prev))
		} else {
			seen[tc.Purpose] = i
		}
		// Require a Secret name (fail closed, symmetric to
		// credinject.Inject / GuardAgentSecret) and confirm it resolves.
		if tc.SecretRef.Name == "" {
			errs = append(errs, invalidf(path+".secretRef.name", tc.SecretRef, "tool credential requires a Secret name; got empty secretRef"))
			continue
		}
		// Metadata-only existence check: this guard only needs to know the
		// Secret exists, never its bytes. Deserializing the full Secret here
		// would pull token plaintext into the control plane — contradicting
		// the by-reference/no-plaintext boundary the whole seam is built on.
		ok, err := v.secretMetadataExists(ctx, namespace, tc.SecretRef.Name)
		switch {
		case err != nil:
			errs = append(errs, invalidf(path+".secretRef", tc.SecretRef, "admission read failed (fail-closed): %v", err))
		case !ok:
			errs = append(errs, invalidf(path+".secretRef", tc.SecretRef, "referenced Secret %s/%s does not exist; create the tool-credential Secret first", namespace, tc.SecretRef.Name))
		}
	}
	return errs
}

// secretMetadataExists reports whether a Secret exists using a METADATA-ONLY
// lookup (PartialObjectMetadata): the API server returns only name/namespace,
// never the Secret's data, so the webhook proves existence without the
// control plane ever handling the token bytes (the by-reference boundary,
// NFR-SEC3). A transient read error fails closed (returned to the caller,
// which denies admission).
func (v *CrossRefValidator) secretMetadataExists(ctx context.Context, namespace, name string) (bool, error) {
	meta := &metav1.PartialObjectMetadata{}
	meta.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
	err := v.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, meta)
	if err == nil {
		return true, nil
	}
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return false, err
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
	if v.on(GuardRunToolCredentials) {
		errs = append(errs, v.validateToolCredentials(ctx, run.Namespace, run.Spec.ToolCredentials, "spec.toolCredentials")...)
		errs = append(errs, v.validateRunToolCredentialsDerived(ctx, run)...)
	}
	errs = append(errs, v.validateRunToolchains(ctx, run)...)
	return errs
}

// validateRunToolCredentialsDerived enforces the trust boundary on a Run's
// projected aux credentials (ISI-3565, Copilot review on PR#230): every
// Run.spec.toolCredentials entry must be AUTHORISED by one of the Run's
// spec.agents — an admitted Agent that declares the same (purpose, Secret).
// The Run field is meant to be a name-only PROJECTION of admitted Agents'
// aux credentials (ADR-045 D5), not a fresh grant surface; without this
// check a Run submitter could select any same-namespace Secret and have it
// injected as GH_TOKEN, bypassing Agent admission entirely. Fail-closed: an
// aux credential with no backing Agent (including a Run that names no
// Agents) is rejected.
func (v *CrossRefValidator) validateRunToolCredentialsDerived(ctx context.Context, run *ksquadv1alpha1.Run) field.ErrorList {
	var errs field.ErrorList
	if len(run.Spec.ToolCredentials) == 0 {
		return errs
	}
	// Collect the (purpose, secretName, effectiveKey) grants declared by the
	// Run's Agents. The identity must include the EFFECTIVE Secret key, not
	// just the name: toolcred.Inject mounts SecretRef.Key (defaulting to
	// "token"), so a Run that reuses an authorized Secret name but selects a
	// DIFFERENT key would otherwise slip through and read material the Agent
	// never authorized.
	//
	// Only Agents in the RUN's namespace can authorize: the aux Secret is
	// mounted by-reference (SecretKeySelector) in the pod's namespace = the
	// Run's namespace, so a cross-namespace Agent's same-named Secret is a
	// DIFFERENT physical object and must not authorize the Run's credential.
	type grant struct{ purpose, secret, key string }
	authorised := map[grant]bool{}
	for _, ref := range run.Spec.Agents {
		if resolveNamespace(ref, run.Namespace) != run.Namespace {
			// Cross-namespace Agent: its Secrets live in its own namespace,
			// not where this Run's pod will mount them. It cannot authorize.
			continue
		}
		agent := &ksquadv1alpha1.Agent{}
		if err := v.Reader.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: ref.Name}, agent); err != nil {
			if apierrors.IsNotFound(err) {
				// A genuinely missing Agent is already reported by
				// GuardRunAgents; it simply contributes no grants here.
				continue
			}
			// A transient/forbidden read must NOT be silently downgraded to
			// "no grant" — that would mischaracterise an API outage as a
			// policy violation ("credential not declared") below. This guard
			// runs its own Get (guards are independently switchable, so we
			// cannot lean on GuardRunAgents having covered it), so it owns
			// its own fail-closed diagnostic.
			errs = append(errs, invalidf(fmt.Sprintf("spec.agents[%d]", agentIndexOf(run, ref)), ref.Name,
				"admission read of Agent %s/%s failed (fail-closed): %v", run.Namespace, ref.Name, err))
			continue
		}
		for _, tc := range agent.Spec.ToolCredentials {
			authorised[grant{tc.Purpose, tc.SecretRef.Name, toolcred.EffectiveKey(tc.SecretRef.Key)}] = true
		}
	}
	// If any Agent read failed transiently we cannot compute the authorised
	// set reliably; return the fail-closed read diagnostic(s) WITHOUT running
	// the policy comparison, so an outage is never reported as a mis-declared
	// credential.
	if len(errs) > 0 {
		return errs
	}
	for i, tc := range run.Spec.ToolCredentials {
		if !authorised[grant{tc.Purpose, tc.SecretRef.Name, toolcred.EffectiveKey(tc.SecretRef.Key)}] {
			errs = append(errs, invalidf(fmt.Sprintf("spec.toolCredentials[%d]", i), tc,
				"Run tool credential (purpose %q, secret %q, key %q) is not declared by any same-namespace Agent in spec.agents; aux credentials must be projected from an admitted Agent — matching purpose, Secret name AND key, in the Run's namespace — not granted directly on the Run (ADR-045 D5)", tc.Purpose, tc.SecretRef.Name, toolcred.EffectiveKey(tc.SecretRef.Key)))
		}
	}
	return errs
}

// agentIndexOf returns the index of ref within run.Spec.Agents (by name +
// resolved namespace), or -1. Used only to give a fail-closed Agent-read
// diagnostic a precise field path.
func agentIndexOf(run *ksquadv1alpha1.Run, ref ksquadv1alpha1.ObjectRef) int {
	for i, a := range run.Spec.Agents {
		if a.Name == ref.Name && resolveNamespace(a, run.Namespace) == resolveNamespace(ref, run.Namespace) {
			return i
		}
	}
	return -1
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
