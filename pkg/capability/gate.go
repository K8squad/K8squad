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

// gate.go — the WS-9 S5 go-live gate's input adapter (ISI-5818/ISI-5849). It
// bridges the Run's recorded CapabilityManifest (the immutable audit truth the
// driver reads from status without re-listing Skills) to the pure toolcred gate
// oracle (EvaluateRunGate). Keeping the adapter in the capability package — the
// one place that already imports both the api CRD types and toolcred — avoids
// coupling the low-level, dependency-free toolcred decision core to the CRD API.
package capability

import (
	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/toolcred"
)

// GatedSkillsFromManifest extracts the go-live gate input from a Run's recorded
// capability manifest: one toolcred.GatedSkill per granted skill that carries a
// ksquad.io/credential-type annotation (CredentialType non-empty), in manifest
// order. SourceName is "namespace/name" for the oracle's actionable withhold
// messages. An ungated skill (empty CredentialType) is omitted — it never gates
// go-live. Returns nil for a nil/empty manifest (a bare Run grants nothing to
// gate), which EvaluateRunGate admits.
func GatedSkillsFromManifest(m *api.CapabilityManifest) []toolcred.GatedSkill {
	if m == nil || len(m.Skills) == 0 {
		return nil
	}
	var out []toolcred.GatedSkill
	for _, s := range m.Skills {
		if s.CredentialType == "" {
			continue
		}
		out = append(out, toolcred.GatedSkill{
			SourceName: s.Namespace + "/" + s.Name,
			Annotation: s.CredentialType,
		})
	}
	return out
}

// CandidateCredentialTypes is the deduped, order-preserving union of every
// declared credential type across the gated skills (SplitCredentialTypes per
// skill) — the candidate set toolcred.ProjectSatisfiedTypes probes for
// materialized project Secrets. Dedup keeps the first occurrence; an ungated
// skill contributes none. Fail-closed is the oracle's job: an unknown token
// here is harmless because ProjectSatisfiedTypes skips it (no materializable
// name) and EvaluateSkillGate keeps it in the gating set (never admitted).
func CandidateCredentialTypes(gated []toolcred.GatedSkill) []string {
	seen := map[string]bool{}
	var out []string
	for _, g := range gated {
		for _, t := range toolcred.SplitCredentialTypes(g.Annotation) {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out
}
