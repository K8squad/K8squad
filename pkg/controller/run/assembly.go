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

	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/capability"
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

	endpoints, servers, err := capability.ResolveMCP(ctx, a.Client, run, reqs)
	if err != nil {
		return nil, err
	}
	if err := capability.CheckEgressAll(ctx, a.Client, run, servers); err != nil {
		return nil, err
	}

	return &ResolvedEnvelope{
		Requirements: reqs,
		Toolchains:   resolved,
		Endpoints:    endpoints,
		Servers:      servers,
		Manifest:     capability.BuildManifest(resolved, endpoints, reqs.Skills),
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
		if err := capability.EnsureMCPConfigMap(ctx, a.Client, run, endpointsFromManifest(run.Status.CapabilityManifest)); err != nil {
			return nil, err
		}
		if err := capability.EnsureSkillConfigMaps(ctx, a.Client, run, inlineSkillsFromManifest(run.Status.CapabilityManifest)); err != nil {
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
// truth; the ConfigMap follows it, never vice versa).
func endpointsFromManifest(m *api.CapabilityManifest) []capability.Endpoint {
	if m == nil || len(m.MCPEndpoints) == 0 {
		return nil
	}
	out := make([]capability.Endpoint, 0, len(m.MCPEndpoints))
	for _, ep := range m.MCPEndpoints {
		out = append(out, capability.Endpoint{
			Name:                ep.Name,
			Transport:           string(ep.Transport),
			URL:                 ep.URL,
			Headers:             ep.Headers,
			Command:             ep.Command,
			Args:                ep.Args,
			Image:               ep.Image,
			EnvNames:            envNamesFor(ep),
			AllowTools:          ep.AllowTools,
			DenyTools:           ep.DenyTools,
			CredentialSecretRef: ep.CredentialSecretRef,
			EgressPolicyRef:     ep.EgressPolicyRef,
		})
	}
	return out
}

func envNamesFor(ep api.ResolvedMCPEndpoint) []string {
	if ep.CredentialSecretRef == nil {
		return nil
	}
	return []string{capability.CredentialEnvName(ep.Name)}
}

// wrapAssemblyError annotates assembly failures with the Run identity for
// the reconciler's requeue log (fail-closed, actionable).
func wrapAssemblyError(run *api.Run, err error) error {
	return fmt.Errorf("assemble capabilities for run %s/%s: %w", run.Namespace, run.Name, err)
}
