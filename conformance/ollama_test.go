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

package conformance

import (
	"testing"

	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/shim/runtimes"
)

// TestConformance_OllamaLane_V1ShimSet is the story-5.6 Ollama-lane assertion:
// every byoModelEndpoint-capable v1 runtime passes the same six checks with its
// model resolved to a BYO Ollama endpoint and a zero-cost placeholder key — the
// $0 way for a vendor to prove conformance (story 5.7/5.8, ISI-2157).
func TestConformance_OllamaLane_V1ShimSet(t *testing.T) {
	for _, name := range v1ShimSet {
		name := name
		t.Run(name, func(t *testing.T) {
			rt := mustGet(t, name)
			if !rt.Capabilities().BYOModelEndpoint {
				t.Skipf("runtime %q does not advertise byoModelEndpoint", name)
			}
			rep := VerifyRuntime(rt, Options{Lane: LaneOllama, OllamaModel: "qwen3"})
			for _, res := range rep.Results {
				if !res.Pass {
					t.Errorf("[%s ollama] %s FAILED: %s", name, res.Check, res.Detail)
				}
			}
			if !rep.OK() {
				t.Errorf("runtime %q is NOT conformant on the Ollama lane\n%s", name, rep)
			}
		})
	}
}

// TestConformance_OllamaLane_ZeroPaidCredential proves the defining $0 property:
// the resolved model wire uses the placeholder key against the BYO endpoint and
// carries no paid provider credential.
func TestConformance_OllamaLane_ZeroPaidCredential(t *testing.T) {
	rt := mustGet(t, "opencode")
	rep := VerifyRuntime(rt, Options{Lane: LaneOllama, OllamaEndpoint: "http://ollama:11434/v1", OllamaModel: "llama3"})
	for _, res := range rep.Results {
		if res.Check == CheckCredentialMetadata && !res.Pass {
			t.Fatalf("Ollama-lane credential check failed: %s", res.Detail)
		}
	}
	if !rep.OK() {
		t.Fatalf("opencode failed the Ollama lane:\n%s", rep)
	}
}

// TestConformance_OllamaLane_IneligibleRuntime proves a runtime that does not
// advertise byoModelEndpoint is refused on the Ollama lane rather than silently
// "passing" a lane it cannot honor (capability honesty).
func TestConformance_OllamaLane_IneligibleRuntime(t *testing.T) {
	rt := hostileRuntime{
		typeName:   "hostile-nobyo",
		caps:       a2a.Capabilities{Streaming: true, ArtifactKinds: []string{"file"}, BYOModelEndpoint: false},
		model:      a2a.ModelInfo{ID: "m", ContextWindow: 1000},
		cliVersion: "v1",
		credShape:  runtimes.ShapeAPIKey,
	}
	runtimes.Register(rt)
	rep := VerifyRuntime(rt, Options{Lane: LaneOllama})
	assertFails(t, rep, CheckCapabilityHonesty)
	if rep.OK() {
		t.Fatal("an ineligible runtime was certified on the Ollama lane")
	}
}
