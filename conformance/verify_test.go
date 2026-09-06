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

// v1ShimSet is the runtime set story 5.6 certifies: OpenClaw + Hermes (5.5),
// opencode (5.8), and codex (epic ISI-3647, S8/ISI-3660 — the final integration
// gate). Every one MUST pass every check on the default lane.
var v1ShimSet = []string{
	"openclaw",
	"hermes",
	"opencode",
	"codex",
}

// TestConformance_V1ShimSet_DefaultLane is the gate-blocking assertion: the
// whole v1 shim set passes every conformance dimension on its native wire.
func TestConformance_V1ShimSet_DefaultLane(t *testing.T) {
	for _, name := range v1ShimSet {
		name := name
		t.Run(name, func(t *testing.T) {
			rt, err := runtimes.Get(name)
			if err != nil {
				t.Fatalf("runtime %q not registered: %v", name, err)
			}
			rep := VerifyRuntime(rt, Options{Lane: LaneDefault})
			if len(rep.Results) != len(AllChecks()) {
				t.Fatalf("report has %d results, want %d (a dimension was skipped)", len(rep.Results), len(AllChecks()))
			}
			for _, res := range rep.Results {
				if !res.Pass {
					t.Errorf("[%s] %s FAILED: %s", name, res.Check, res.Detail)
				}
			}
			if !rep.OK() {
				t.Errorf("runtime %q is NOT conformant on the default lane\n%s", name, rep)
			}
		})
	}
}

// TestConformance_Codex_IsAutoEnumerated is the ISI-3660 AC1 hook: the codex
// adapter (epic ISI-3647) is present in runtimes.Registered() — the set
// cmd/conformance auto-enumerates — so the vendor-facing `conformance` CLI
// certifies it with zero flag. A registry that dropped codex would silently
// exclude it from the gate; this catches that regression.
func TestConformance_Codex_IsAutoEnumerated(t *testing.T) {
	for _, name := range runtimes.Registered() {
		if name == "codex" {
			return
		}
	}
	t.Fatalf("codex not in runtimes.Registered() = %v — cmd/conformance would never certify it (AC1)", runtimes.Registered())
}

// TestConformance_Codex_A2ALifecycle is the ISI-3660 AC1 assertion: with the
// codex adapter registered, the conformance suite runs and codex passes every
// A2A capability/lifecycle check on BOTH the default (fixed OpenAI wire) and the
// $0 Ollama (BYO endpoint, story 5.7/D6) lanes. This is the final integration
// gate the epic's S1–S7 feed.
func TestConformance_Codex_A2ALifecycle(t *testing.T) {
	rt, err := runtimes.Get("codex")
	if err != nil {
		t.Fatalf("codex not registered: %v", err)
	}
	for _, lane := range []Lane{LaneDefault, LaneOllama} {
		lane := lane
		t.Run(string(lane), func(t *testing.T) {
			rep := VerifyRuntime(rt, Options{Lane: lane})
			if len(rep.Results) != len(AllChecks()) {
				t.Fatalf("report has %d results, want %d (a dimension was skipped)", len(rep.Results), len(AllChecks()))
			}
			for _, res := range rep.Results {
				if !res.Pass {
					t.Errorf("[codex lane=%s] %s FAILED: %s", lane, res.Check, res.Detail)
				}
			}
			if !rep.OK() {
				t.Errorf("codex is NOT conformant on the %s lane\n%s", lane, rep)
			}
		})
	}
}

// TestConformance_ReportShape asserts every AllChecks() dimension is reported
// exactly once and in order, so a silently-skipped check can never read green.
func TestConformance_ReportShape(t *testing.T) {
	rt, _ := runtimes.Get("opencode")
	rep := VerifyRuntime(rt, Options{Lane: LaneDefault})
	if got, want := len(rep.Results), len(AllChecks()); got != want {
		t.Fatalf("got %d results want %d", got, want)
	}
	for i, check := range AllChecks() {
		if rep.Results[i].Check != check {
			t.Errorf("result %d is %q want %q", i, rep.Results[i].Check, check)
		}
	}
}

// --- Negative "teeth" tests: every check must catch its violation. ---

// hostileRuntime is a configurable adapter used to prove the card-level checks
// fail closed. Registered under unique type names so the process-wide registry
// never collides with the v1 set.
type hostileRuntime struct {
	typeName   string
	caps       a2a.Capabilities
	model      a2a.ModelInfo
	cliVersion string
	credShape  runtimes.CredentialShape
}

func (h hostileRuntime) Type() string                              { return h.typeName }
func (h hostileRuntime) CLIVersion() string                        { return h.cliVersion }
func (h hostileRuntime) Capabilities() a2a.Capabilities            { return h.caps }
func (h hostileRuntime) DefaultModel() a2a.ModelInfo               { return h.model }
func (h hostileRuntime) CredentialShape() runtimes.CredentialShape { return h.credShape }
func (h hostileRuntime) Command(lc runtimes.LaunchContext) (runtimes.ExecSpec, error) {
	return runtimes.ExecSpec{Path: "hostile", Args: []string{"run"}}, nil
}

func TestConformance_AgentCard_CatchesNoStreaming(t *testing.T) {
	rt := hostileRuntime{
		typeName:   "hostile-nostream",
		caps:       a2a.Capabilities{Streaming: false, ArtifactKinds: []string{"file"}},
		model:      a2a.ModelInfo{ID: "m", ContextWindow: 1000},
		cliVersion: "v1",
		credShape:  runtimes.ShapeAPIKey,
	}
	runtimes.Register(rt)
	rep := VerifyRuntime(rt, Options{Lane: LaneDefault})
	assertFails(t, rep, CheckAgentCard)
}

func TestConformance_AgentCard_CatchesEmptyArtifactKinds(t *testing.T) {
	rt := hostileRuntime{
		typeName:   "hostile-nokinds",
		caps:       a2a.Capabilities{Streaming: true, ArtifactKinds: nil},
		model:      a2a.ModelInfo{ID: "m", ContextWindow: 1000},
		cliVersion: "v1",
		credShape:  runtimes.ShapeAPIKey,
	}
	runtimes.Register(rt)
	rep := VerifyRuntime(rt, Options{Lane: LaneDefault})
	// Empty kinds breaks the card check AND leaves nothing to emit (§5).
	assertFails(t, rep, CheckAgentCard)
	assertFails(t, rep, CheckArtifactEmission)
}

func TestConformance_AgentCard_CatchesZeroContextWindow(t *testing.T) {
	rt := hostileRuntime{
		typeName:   "hostile-noctx",
		caps:       a2a.Capabilities{Streaming: true, ArtifactKinds: []string{"file"}},
		model:      a2a.ModelInfo{ID: "m", ContextWindow: 0},
		cliVersion: "v1",
		credShape:  runtimes.ShapeAPIKey,
	}
	runtimes.Register(rt)
	rep := VerifyRuntime(rt, Options{Lane: LaneDefault})
	assertFails(t, rep, CheckAgentCard)
}

// TestConformance_CredentialLeak_CaughtOnWire proves the credential scan has
// teeth: a card or event carrying the sentinel is flagged.
func TestConformance_CredentialLeak_CaughtOnCard(t *testing.T) {
	leaky := a2a.AgentCard{Auth: a2a.AuthInfo{Type: "api-key", SecretRef: sentinelCredential}}
	if leaked, _ := containsSecret(leaky, sentinelCredential); !leaked {
		t.Fatal("credential scan failed to catch the raw credential embedded in a card")
	}
	log := []a2a.Event{{Type: a2a.EventMessage, Payload: a2a.MessagePayload{Text: "here is my key " + sentinelCredential}}}
	if leaked, _ := containsSecret(log, sentinelCredential); !leaked {
		t.Fatal("credential scan failed to catch the raw credential on the wire")
	}
	// A clean card + log must NOT be flagged.
	if leaked, _ := containsSecret(a2a.AgentCard{Auth: a2a.AuthInfo{SecretRef: "just-a-ref"}}, sentinelCredential); leaked {
		t.Fatal("credential scan false-positived on a clean card")
	}
}

// TestConformance_CapabilityHonesty_CatchesUnadvertisedKind proves the subset
// check flags an artifact kind the card never advertised.
func TestConformance_CapabilityHonesty_CatchesUnadvertisedKind(t *testing.T) {
	if _, ok := isSubset([]string{"file", "patch"}, []string{"file"}); ok {
		t.Fatal("subset check failed to catch an unadvertised artifact kind")
	}
	if missing, ok := isSubset([]string{"file"}, []string{"file", "patch"}); !ok {
		t.Fatalf("subset check false-negatived: missing=%q", missing)
	}
}

// TestConformance_SSE_CatchesSeqGap proves the SSE check flags a non-gap-free
// sequence (C4).
func TestConformance_SSE_CatchesSeqGap(t *testing.T) {
	h := &harness{rt: mustGet(t, "opencode")}
	c := newChecker()
	gapped := []a2a.Event{
		{Seq: 1, A2ATaskID: conformanceRunID, Type: a2a.EventStatus, Payload: a2a.StatusPayload{State: a2a.TaskSubmitted}},
		{Seq: 3, A2ATaskID: conformanceRunID, Type: a2a.EventStatus, Payload: a2a.StatusPayload{State: a2a.TaskCompleted}},
	}
	h.checkSSE(c, nil, nil, gapped) // len<3 → no resume path, gap detected statically
	if r := c.results[CheckSSEProgress]; r.Pass {
		t.Fatal("SSE check passed a sequence with a seq gap (should catch C4 violation)")
	}
}

// TestConformance_Artifacts_CatchesBadShape proves the artifact check flags a
// ref that is not content-addressed or not bound to the work item (§5).
func TestConformance_Artifacts_CatchesBadShape(t *testing.T) {
	h := &harness{rt: mustGet(t, "opencode")}
	c := newChecker()
	bad := []a2a.Event{{Type: a2a.EventArtifactRef, Payload: a2a.ArtifactRef{
		Kind: "file", WorkItemID: "WRONG", URI: "", SHA256: "short",
	}}}
	h.checkArtifacts(c, a2a.AgentCard{}, bad)
	if r := c.results[CheckArtifactEmission]; r.Pass {
		t.Fatal("artifact check passed a malformed artifact ref")
	}
}

// --- helpers ---

func assertFails(t *testing.T, rep Report, check Check) {
	t.Helper()
	for _, res := range rep.Results {
		if res.Check == check {
			if res.Pass {
				t.Fatalf("expected %s to FAIL but it passed: %s", check, res.Detail)
			}
			return
		}
	}
	t.Fatalf("check %s not present in report", check)
}

func mustGet(t *testing.T, name string) runtimes.Runtime {
	t.Helper()
	rt, err := runtimes.Get(name)
	if err != nil {
		t.Fatalf("runtime %q not registered: %v", name, err)
	}
	return rt
}
