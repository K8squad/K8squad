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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/capability"
	"github.com/K8squad/K8squad/pkg/controller/team"
	"github.com/K8squad/K8squad/pkg/mcpauthtoken"
	"github.com/K8squad/K8squad/pkg/toolchain"
)

// Epic C (ISI-3287) Run assembly (plan §2.3, ADR-044): the operator
// computes a Run's resolved capability envelope — toolchains, MCP
// endpoints, egress linkage — pre-dispatch, records it on Run.status as
// an IMMUTABLE capability manifest, and projects the MCP IR into the
// sandbox namespace. The same component feeds the RBAC renderer's grant
// (Epic B) and the pod seam (pkg/capability.AssemblePod), so the grant
// admission proved and the pod dispatch assumes come from one resolution.
type Assembler struct {
	client.Client
	// Platform carries the cluster-catalog namespace and the
	// cluster-scope opt-in (deployment env, Helm values) — shared with
	// the RBAC renderer.
	Platform toolchain.PlatformConfig
	// Minter mints the per-run authoring capability token (ADR-0024a S3,
	// ISI-4869) with the shared HS256 key cmd/memory verifies with (D2
	// option (a)). nil = inert: no token is minted or projected, the built-in
	// memory-authoring endpoint carries no credential, and the memory edge
	// serves authoring on the trusted BFF header path only — symmetric with
	// cmd/memory's own "no KSQUAD_JWT_SIGNING_KEY ⇒ header path only" degrade.
	Minter *mcpauthtoken.Minter
}

// NewAssembler builds an assembler over a manager-managed or fake client.
func NewAssembler(c client.Client, platform toolchain.PlatformConfig) *Assembler {
	return &Assembler{Client: c, Platform: platform}
}

// ResolvedEnvelope is one Run's fully resolved capability plane: the
// toolchains (with effective RBAC envelopes), the MCP IR endpoints (with
// their source servers), and the manifest to record. Everything the pod
// seam, the RBAC renderer and the status projector need — computed once,
// fail-closed.
type ResolvedEnvelope struct {
	Requirements *capability.Requirements
	Toolchains   []toolchain.Resolved
	Endpoints    []capability.Endpoint
	Servers      []*api.MCPServer
	Manifest     *api.CapabilityManifest
	// Grants is the capability grant resolved for the Run's decomposing
	// agent(s) from the owning Team's grant store (ADR-0024a S4). Empty by
	// default (deny-by-default). Feeds the authoring-MCP gate (S2) and the
	// run-token capability claim (S3).
	Grants capability.GrantSet
}

// Resolve computes a Run's full capability envelope fail-closed
// (ADR-044 steps 1–5): requirements union, toolchain resolution
// (conflicts/unknowns fail closed via pkg/toolchain), MCP resolution
// (staleness/empty-effective-filter fail closed via pkg/capability), and
// the egress re-assertion (ADR-045). A Run with no capability demand
// resolves to an empty envelope — nothing staged, nothing wired, an
// empty manifest whose hash still keys the (bare) pool posture.
func (a *Assembler) Resolve(ctx context.Context, run *api.Run) (*ResolvedEnvelope, error) {
	platform := a.Platform.WithDefaults()
	resolver := &toolchain.Resolver{Reader: a.Client, Platform: platform}

	reqs, err := capability.Collect(ctx, a.Client, run)
	if err != nil {
		return nil, err
	}

	resolved, err := resolver.ResolveRefs(ctx, run.Namespace, reqs.ToolchainRefs, toolchain.DetailsFor(run))
	if err != nil {
		return nil, err
	}

	// ADR-0024a S4: resolve the authoring capability grant for the Run's
	// decomposing agent from the owning Team, fail-closed (deny-by-default).
	// Resolved control-plane-side (never from anything the sandbox supplies),
	// it gates S2's authoring-MCP auto-injection in ResolveMCP below and feeds
	// S3's run-token claim so the gate proved and the token minted agree.
	grants, err := capability.ResolveGrant(ctx, a.Client, run)
	if err != nil {
		return nil, err
	}

	endpoints, servers, err := capability.ResolveMCP(ctx, a.Client, run, reqs, grants)
	if err != nil {
		return nil, err
	}
	if err := capability.CheckEgressAll(ctx, a.Client, run, servers); err != nil {
		return nil, err
	}

	// ADR-0024a S3 (ISI-4869): bind the built-in memory-authoring endpoint —
	// injected by S2 ONLY for a Run granted work_item.author — to the per-run
	// Secret carrying its run capability token. The credential rides the
	// EXISTING credentialSecretRef → Bearer ${ENV} seam (no new render path);
	// setting it here records the ref on the immutable manifest, and
	// EnsureManifest mints + writes the Secret pre-dispatch. S1 leaves the
	// MCPServer's credentialSecretRef nil on purpose ("S3 wires the per-run
	// token") — this is that wire. Inert (no ref) without a minter.
	a.bindAuthoringCredential(run, endpoints)

	return &ResolvedEnvelope{
		Requirements: reqs,
		Toolchains:   resolved,
		Endpoints:    endpoints,
		Servers:      servers,
		Manifest:     capability.BuildManifest(resolved, endpoints, reqs.Skills),
		Grants:       grants,
	}, nil
}

// EnsureManifest converges the Run's recorded capability manifest and the
// projected MCP IR ConfigMap:
//
//   - first live reconcile computes the envelope and stamps
//     status.capabilityManifest (hash included);
//   - the manifest is IMMUTABLE afterwards (ADR-044 invariant): mid-flight
//     changes to Skills/Toolchains/MCPServers never widen a running
//     sandbox — the recorded manifest stays the audit truth, changes
//     apply to the next Run — so a set manifest is returned as-is;
//   - the IR ConfigMap is converged to the manifest's endpoints (drift
//     repair only; the content is stable because the manifest is).
//
// Fail-closed: a resolution error is returned for requeue — a Run never
// proceeds with a partial envelope.
func (a *Assembler) EnsureManifest(ctx context.Context, run *api.Run) (*api.CapabilityManifest, error) {
	if run.Status.CapabilityManifest != nil {
		recorded := endpointsFromManifest(run.Status.CapabilityManifest)
		if err := capability.EnsureMCPConfigMap(ctx, a.Client, run, recorded); err != nil {
			return nil, err
		}
		if err := capability.EnsureSkillConfigMaps(ctx, a.Client, run, inlineSkillsFromManifest(run.Status.CapabilityManifest)); err != nil {
			return nil, err
		}
		// Re-ensure the authoring token Secret from the recorded ref: a
		// restart after the manifest was stamped but before the Secret landed
		// must still provision it (create-if-absent, so a live one is left be).
		if err := a.ensureAuthoringToken(ctx, run, recorded); err != nil {
			return nil, err
		}
		return run.Status.CapabilityManifest, nil
	}

	env, err := a.Resolve(ctx, run)
	if err != nil {
		return nil, err
	}
	if err := capability.EnsureMCPConfigMap(ctx, a.Client, run, env.Endpoints); err != nil {
		return nil, err
	}
	if err := capability.EnsureSkillConfigMaps(ctx, a.Client, run, inlineSkillsFromManifest(env.Manifest)); err != nil {
		return nil, err
	}
	if err := a.ensureAuthoringToken(ctx, run, env.Endpoints); err != nil {
		return nil, err
	}
	return env.Manifest, nil
}

// ReleaseConfig drops the projected IR ConfigMap and the per-skill
// ConfigMaps when a Run goes terminal (the owner reference GC covers
// object deletion; this is the explicit, idempotent sweep for the drift
// case).
func (a *Assembler) ReleaseConfig(ctx context.Context, run *api.Run) error {
	if err := capability.EnsureMCPConfigMap(ctx, a.Client, run, nil); err != nil {
		return err
	}
	return capability.EnsureSkillConfigMaps(ctx, a.Client, run, nil)
}

// inlineSkillsFromManifest rebuilds the INLINE granted-skill set from a
// recorded manifest for per-skill ConfigMap convergence (the manifest is
// the audit truth; git-sourced skills are S-D and skip this projection).
func inlineSkillsFromManifest(m *api.CapabilityManifest) []capability.GrantedSkill {
	if m == nil || len(m.Skills) == 0 {
		return nil
	}
	var out []capability.GrantedSkill
	for _, s := range m.Skills {
		if s.SourceType != api.SkillSourceInline {
			continue
		}
		out = append(out, capability.GrantedSkill{
			Namespace:   s.Namespace,
			Name:        s.Name,
			SourceType:  s.SourceType,
			Inline:      s.Inline,
			Permissions: s.Permissions,
		})
	}
	return out
}

// endpointsFromManifest rebuilds the IR endpoints from a recorded
// manifest for IR ConfigMap convergence (the manifest is the audit
// truth; the ConfigMap follows it, never vice versa). It delegates to
// pkg/capability so the dispatch seam's manifest→IR rebuild (ISI-5017)
// and this one can never drift.
func endpointsFromManifest(m *api.CapabilityManifest) []capability.Endpoint {
	return capability.EndpointsFromManifest(m)
}

// authoringTokenSecretKey is the Secret data key the run capability token is
// written under. It mirrors pkg/capability's defaultCredentialKey (the key the
// SecretKeyRef env projection reads when a credentialSecretRef carries no
// explicit key) — the built-in authoring endpoint's ref sets no key, so both
// sides must agree on "token".
const authoringTokenSecretKey = "token"

// authoringTokenSecretName is the deterministic per-run Secret the run
// capability token is written to (ADR-0024a S3). Namespaced to the Run and
// owned by it, so it is garbage-collected with the Run — no orphaned tokens.
func authoringTokenSecretName(run *api.Run) string {
	return run.Name + "-authoring-token"
}

// authoringEndpoint returns the injected built-in memory-authoring endpoint in
// eps, or nil if the Run was not granted authoring (S2 injects it ONLY for a
// work_item.author-granted Run, so its presence is the grant, by construction).
func authoringEndpoint(eps []capability.Endpoint) *capability.Endpoint {
	for i := range eps {
		if eps[i].Name == team.AuthoringMCPServerName {
			return &eps[i]
		}
	}
	return nil
}

// bindAuthoringCredential points the injected authoring endpoint's
// credentialSecretRef at the per-run token Secret so the render seam projects
// `Authorization: Bearer ${KSQUAD_MCP_..._TOKEN}` from it. No-op — leaving the
// endpoint credential-less — when there is no minter (S3 inert), no dispatched
// agent to bind the token to, or the Run was not granted authoring. Gating the
// ref on the SAME conditions ensureAuthoringToken mints under keeps the two in
// lockstep: the endpoint never references a Secret that will not be written
// (which would wedge the pod on a missing SecretKeyRef).
func (a *Assembler) bindAuthoringCredential(run *api.Run, eps []capability.Endpoint) {
	if a.Minter == nil || len(run.Spec.Agents) == 0 {
		return
	}
	ep := authoringEndpoint(eps)
	if ep == nil {
		return
	}
	ep.CredentialSecretRef = &api.SecretRef{Name: authoringTokenSecretName(run)}
	ep.EnvNames = []string{capability.CredentialEnvName(ep.Name)}
}

// ensureAuthoringToken mints the Run's capability token and writes it to the
// per-run Secret the authoring endpoint references (ADR-0024a S3). The
// capabilities are baked in AT MINT TIME from the control-plane grant — the
// authoring endpoint is injected only for a work_item.author-granted Run, so
// its presence pins the token's caps to exactly that grant, never anything the
// sandbox supplies.
//
// Write is create-if-absent by design: the credential rides as a SecretKeyRef
// env, which the kubelet resolves ONCE at container start and never
// live-updates, so a stable token written pre-dispatch is what the pod reads —
// re-minting on a later reconcile could not reach a running container and would
// only churn the Secret. A missing signing key, ungranted Run, or agent-less
// Run leaves it a no-op (inert, no Secret).
func (a *Assembler) ensureAuthoringToken(ctx context.Context, run *api.Run, eps []capability.Endpoint) error {
	if a.Minter == nil || len(run.Spec.Agents) == 0 {
		return nil
	}
	ep := authoringEndpoint(eps)
	if ep == nil || ep.CredentialSecretRef == nil || ep.CredentialSecretRef.Name == "" {
		return nil
	}
	name := ep.CredentialSecretRef.Name

	var existing corev1.Secret
	err := a.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: name}, &existing)
	if err == nil {
		return nil // already provisioned — leave the pod-visible token be
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get authoring-token secret %s/%s: %w", run.Namespace, name, err)
	}

	token, err := a.Minter.Mint(mcpauthtoken.Claims{
		TeamID:    run.Spec.TeamRef.Name,
		Principal: string(run.GetOwnedBy()),
		// The dispatched (decomposing) agent, matched against the work item's
		// dispatch claim by S5 custody-match. Mirrors the task-io writer's
		// principal derivation (rundrive.SecretCredentialWriter).
		AgentID: run.Spec.Agents[0].Name,
		RunID:   string(run.UID),
		// Caps sourced from the grant, never the sandbox: the authoring
		// endpoint is present only for a work_item.author-granted Run (S2
		// gate). A wider cap vocabulary must record the granted slugs on the
		// manifest and revisit this.
		Capabilities: []string{capability.CapabilityWorkItemAuthor},
	})
	if err != nil {
		return fmt.Errorf("mint run authoring token for %s/%s: %w", run.Namespace, run.Name, err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: run.Namespace,
			Labels:    map[string]string{"app": "k8squad-run", "ksquad.io/run": run.Name},
			// Owned by the Run so the Secret is GC'd when the Run is deleted.
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: api.GroupVersion.String(),
				Kind:       "Run",
				Name:       run.Name,
				UID:        run.UID,
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{authoringTokenSecretKey: []byte(token)},
	}
	if err := a.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil // raced with another reconcile — the token is provisioned
		}
		return fmt.Errorf("create authoring-token secret %s/%s: %w", run.Namespace, name, err)
	}
	return nil
}

// wrapAssemblyError annotates assembly failures with the Run identity for
// the reconciler's requeue log (fail-closed, actionable).
func wrapAssemblyError(run *api.Run, err error) error {
	return fmt.Errorf("assemble capabilities for run %s/%s: %w", run.Namespace, run.Name, err)
}
