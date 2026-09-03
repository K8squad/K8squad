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

package v1alpha1

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Story 1.3 same-object admission rules live as CEL (x-kubernetes-validations)
// and structural defaults compiled into the generated CRD schemas. These
// tests are the falsification proxy for that half of the story: deleting a
// marker from the types (or the regen skipping it) flips the corresponding
// assertion, so no rule can silently disappear from the installed CRDs.
//
// CEL evaluates `self` only — cross-object existence guards cannot be
// expressed here and live in internal/webhook/v1alpha1 (see its
// delete-a-guard falsification suite).

func loadCRD(t *testing.T, file string) string {
	t.Helper()
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	return string(data)
}

// squashed collapses all whitespace so assertions survive controller-gen's
// 80-column YAML line wrapping.
func squashed(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// FR-D3: the runtime type stays open-ended — an out-of-set type is admitted
// ONLY behind experimental=true. Both halves of the ticket in one rule.
func TestCRDHasFRD3RuntimeRule(t *testing.T) {
	yaml := squashed(loadCRD(t, "../../config/crd/bases/ksquad.io_agentruntimes.yaml"))
	assert.Contains(t, yaml, squashed("self.type in ['openclaw','claude-code','opencode','hermes','codex'] || self.experimental"),
		"FR-D3 CEL rule must be compiled into the AgentRuntime CRD")
	// ISI-3654 AC2: codex is a conformant runtime, so type:codex is admitted by
	// CEL WITHOUT spec.experimental=true — it sits inside the conformant set.
	assert.Contains(t, yaml, "'codex'",
		"codex must be a conformant runtime in the FR-D3 CEL set (admitted without experimental)")
	assert.Contains(t, yaml, "spec.experimental=true to admit",
		"FR-D3 denial must carry the fix message")
	assert.NotContains(t, yaml, "type: enum:",
		"spec.type must NOT be a closed enum (FR-D3: shim vendors register new types with zero schema change)")
}

// Story 1.2 deferred the Skill source inline⇔git consistency to story 1.3:
// exactly one body carrier matches the discriminator, fail-closed.
func TestCRDHasSkillSourceOneOfRules(t *testing.T) {
	yaml := squashed(loadCRD(t, "../../config/crd/bases/ksquad.io_skills.yaml"))
	assert.Contains(t, yaml, squashed("self.type != 'inline' || (has(self.inline) && self.inline != '' && !has(self.git))"))
	assert.Contains(t, yaml, squashed("self.type != 'git' || (has(self.git) && self.git.repoRef != '' && self.git.ref != '' && !has(self.inline))"))
}

// Story 1.3 structural defaulting: the documented platform defaults for a
// Run's sandbox posture apply at admission, server-side, no mutating
// webhook needed.
func TestCRDHasSandboxDefaults(t *testing.T) {
	yaml := loadCRD(t, "../../config/crd/bases/ksquad.io_runs.yaml")
	assert.Contains(t, yaml, "runtimeClass:", "sandboxPolicy must be in the Run CRD")
	// The defaults must sit directly under runtimeClass / class schemas.
	runtimeClassIdx := strings.Index(yaml, "runtimeClass:")
	require.NotEqual(t, -1, runtimeClassIdx)
	section := yaml[runtimeClassIdx : runtimeClassIdx+400]
	assert.Contains(t, section, "default: gvisor", "runtimeClass must default to gvisor (§9.1)")
	classIdx := strings.Index(yaml, "class:")
	require.NotEqual(t, -1, classIdx)
	section = yaml[classIdx : classIdx+400]
	assert.Contains(t, section, "default: interactive", "class must default to interactive (§9.2)")
}

// RetryPolicy sanity: negative retry counts and zero/negative backoff are
// rejected fail-closed, and the retry loop is bounded (maxRetries <= 20,
// backoffSeconds <= 3600 — the run_types.go markers, ISI-3297 resync).
func TestCRDHasRetryPolicyRules(t *testing.T) {
	yaml := squashed(loadCRD(t, "../../config/crd/bases/ksquad.io_runs.yaml"))
	assert.Contains(t, yaml, squashed("!has(self.maxRetries) || (self.maxRetries >= 0 && self.maxRetries <= 20)"))
	assert.Contains(t, yaml, squashed("!has(self.backoffSeconds) || (self.backoffSeconds >= 1 && self.backoffSeconds <= 3600)"))
}

// Structural defaulting for the retry loop (ISI-3297): the documented
// MaxRetries=5 / BackoffSeconds=60 defaults apply at admission, server-side.
func TestCRDHasRetryPolicyDefaults(t *testing.T) {
	yaml := loadCRD(t, "../../config/crd/bases/ksquad.io_runs.yaml")
	for _, field := range []string{"maxRetries:", "backoffSeconds:"} {
		idx := strings.Index(yaml, field)
		require.NotEqual(t, -1, idx, field+" must be in the Run CRD")
		section := yaml[idx : idx+400]
		switch field {
		case "maxRetries:":
			assert.Contains(t, section, "default: 5", "maxRetries must default to 5 (anti-infinite-loop safety default)")
		case "backoffSeconds:":
			assert.Contains(t, section, "default: 60", "backoffSeconds must default to 60s (§8 backoff base)")
		}
	}
}

// ContextBudget tiers are non-negative wherever the budget appears
// (Project default and Agent override embed the same struct rule).
func TestCRDHasContextBudgetRule(t *testing.T) {
	for _, file := range []string{
		"../../config/crd/bases/ksquad.io_agents.yaml",
		"../../config/crd/bases/ksquad.io_projects.yaml",
	} {
		yaml := squashed(loadCRD(t, file))
		assert.Contains(t, yaml, squashed("(!has(self.workItem) || self.workItem >= 0)"), file)
	}
}

// The admission manifests must fail closed (a broken webhook denies,
// never admits) for every guarded kind. Story 1.6 unified the attribution
// wiring — each of the four attributed kinds (Team, Project, Agent, Run)
// has exactly one mutating and one validating entry — story 1.5 adds
// the OTelConfig validating webhook, Epic A (ISI-3285) adds the
// MCPServer and Skill validating webhooks, and Epic B (ISI-3286) adds
// the Toolchain validating webhook (the Run toolchain guard chains onto
// the existing run validating path).
func TestWebhookManifestsFailClosed(t *testing.T) {
	yaml := loadCRD(t, "../../config/webhook/manifests.yaml")
	for _, name := range []string{
		"mteam-attribution.ksquad.io", "vteam-attribution.ksquad.io",
		"mproject-attribution.ksquad.io", "vproject-attribution.ksquad.io",
		"magent-attribution.ksquad.io", "vagent-attribution.ksquad.io",
		"mrun-attribution.ksquad.io", "vrun-attribution.ksquad.io",
		"votelconfig-v1alpha1.ksquad.io",
		"vmcpserver-v1alpha1.ksquad.io",
		"vskill-crossrefs.ksquad.io",
		"vtoolchain-v1alpha1.ksquad.io",
	} {
		assert.Contains(t, yaml, "name: "+name)
	}
	// Legacy duplicate-name registration is gone: the story 1.3 paths are
	// served by the chained attribution webhooks, not separate .kb.io ones.
	assert.NotContains(t, yaml, "name: vteam.kb.io")
	assert.NotContains(t, yaml, "name: vagent.kb.io")
	assert.NotContains(t, yaml, "name: vrun.kb.io")
	assert.Equal(t, 12, strings.Count(yaml, "failurePolicy: Fail"),
		"every mutating and validating webhook must fail closed")
}
