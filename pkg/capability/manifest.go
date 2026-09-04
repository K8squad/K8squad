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

package capability

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/toolchain"
)

// BuildManifest folds the resolved envelope into the Run.status
// capability manifest (ADR-044 step 5): toolchains pinned, MCP endpoints
// with their effective tool filters, no credential material — followed by
// the capabilityHash that keys warm-pool inventory (step 7).
func BuildManifest(resolved []toolchain.Resolved, endpoints []Endpoint, skills []GrantedSkill) *api.CapabilityManifest {
	m := &api.CapabilityManifest{
		Toolchains:   make([]api.ResolvedToolchainRef, 0, len(resolved)),
		MCPEndpoints: make([]api.ResolvedMCPEndpoint, 0, len(endpoints)),
		Skills:       make([]api.GrantedSkill, 0, len(skills)),
	}
	for _, res := range resolved {
		m.Toolchains = append(m.Toolchains, api.ResolvedToolchainRef{
			Name:            res.Name,
			Version:         res.Version,
			Image:           res.Image,
			SourceNamespace: res.SourceNamespace,
		})
	}
	for _, ep := range endpoints {
		m.MCPEndpoints = append(m.MCPEndpoints, api.ResolvedMCPEndpoint{
			Name:                ep.Name,
			Transport:           api.MCPTransport(ep.Transport),
			URL:                 ep.URL,
			Headers:             ep.Headers,
			Command:             ep.Command,
			Args:                ep.Args,
			Image:               ep.Image,
			Sidecar:             ep.Transport == string(api.MCPTransportStdio) && ep.Image != "",
			AllowTools:          ep.AllowTools,
			DenyTools:           ep.DenyTools,
			CredentialSecretRef: ep.CredentialSecretRef,
			EgressPolicyRef:     ep.EgressPolicyRef,
		})
	}
	for _, skill := range skills {
		// Convert internal GrantedSkill to API type for serialization
		apiSkill := api.GrantedSkill{
			Namespace:   skill.Namespace,
			Name:        skill.Name,
			SourceType:  skill.SourceType,
			Inline:      skill.Inline,
			Permissions: skill.Permissions,
		}
		if skill.Git != nil {
			apiSkill.Git = skill.Git
		}
		m.Skills = append(m.Skills, apiSkill)
	}
	m.CapabilityHash = HashManifest(m)
	return m
}

// HashManifest computes the manifest's canonical-JSON sha256: the JSON
// encoding of the struct with the hash itself elided (deterministic —
// struct field order is fixed, lists are atomic, resolver/endpoint lists
// arrive sorted). Identical envelopes hash identically across reconciles;
// any envelope change (version pin, image, filter) changes the hash and
// therefore the warm-pool key.
func HashManifest(m *api.CapabilityManifest) string {
	sansHash := m.DeepCopy()
	sansHash.CapabilityHash = ""
	canonical, err := json.Marshal(sansHash)
	if err != nil {
		// CapabilityManifest contains only JSON-native fields; a marshal
		// failure is a programmer error surfaced as the zero hash, which
		// callers treat as "not yet computed".
		return ""
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}
