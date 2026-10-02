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

package runtimes

import (
	"strings"
	"testing"

	apiv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/a2a"
)

// maxArgStrLen is the Linux per-env-string ceiling (MAX_ARG_STRLEN = 128 KiB):
// an exec with any single env string at/over it fails with E2BIG. ISI-5329: no
// adapter may emit an env string this large regardless of context size.
const maxArgStrLen = 128 * 1024

// bigContext is a system context far past the 128 KiB env-string ceiling — the
// exact size class that tripped `fork/exec … : argument list too long`
// (ISI-5328). 600 KiB models a ~200K-token budget (tokens, not bytes).
var bigContext = strings.Repeat("x", 600*1024)

func maxEnvLen(env []string) (string, int) {
	name, n := "", 0
	for _, e := range env {
		if len(e) > n {
			n = len(e)
			if i := strings.IndexByte(e, '='); i >= 0 {
				name = e[:i]
			} else {
				name = e
			}
		}
	}
	return name, n
}

func hasEnvPrefix(env []string, prefix string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// TestOpenCodeBigContextNoOversizedEnv is the ISI-5329 proof for opencode: a
// >128 KiB context produces NO env string near the exec ceiling (the context
// rides stdin, not KSQUAD_SYSTEM_CONTEXT), so exec no longer fails with E2BIG.
func TestOpenCodeBigContextNoOversizedEnv(t *testing.T) {
	rt, _ := Get(apiv1alpha1.RuntimeTypeOpenCode)
	spec, err := rt.Command(LaunchContext{
		Envelope: a2a.Envelope{SystemContext: bigContext, Input: "do the thing"},
		WorkDir:  "/work",
	})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if name, n := maxEnvLen(spec.Env); n >= maxArgStrLen {
		t.Fatalf("opencode env %s is %d bytes, over the %d exec ceiling (E2BIG)", name, n, maxArgStrLen)
	}
	// opencode must NOT set the inline context var at all — the whole prompt
	// rides stdin.
	if hasEnvPrefix(spec.Env, envSystemContext+"=") {
		t.Errorf("opencode must not set %s; env=%v", envSystemContext, spec.Env)
	}
	if !strings.Contains(spec.Stdin, bigContext) {
		t.Errorf("opencode must deliver the full context on stdin (len=%d)", len(spec.Stdin))
	}
}

// TestCodexBigContextSpillsToFile is the ISI-5329 proof for an env-channel
// runtime: a >128 KiB context produces NO oversized env string — the context
// spills to a workdir file and only its path rides KSQUAD_SYSTEM_CONTEXT_FILE,
// while the full context is in the materialized file.
func TestCodexBigContextSpillsToFile(t *testing.T) {
	rt, _ := Get(apiv1alpha1.RuntimeTypeCodex)
	spec, err := rt.Command(LaunchContext{
		Envelope: a2a.Envelope{SystemContext: bigContext, Input: "do the thing"},
		WorkDir:  "/work",
	})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if name, n := maxEnvLen(spec.Env); n >= maxArgStrLen {
		t.Fatalf("codex env %s is %d bytes, over the %d exec ceiling (E2BIG)", name, n, maxArgStrLen)
	}
	// The large context is NOT inline; only the path var is set.
	if hasEnvPrefix(spec.Env, envSystemContext+"=") {
		t.Errorf("codex must not set inline %s for a large context; env=%v", envSystemContext, spec.Env)
	}
	if !hasEnvPrefix(spec.Env, envSystemContextFile+"=/work/"+envelopeFileName) {
		t.Errorf("codex must set %s to the spill-file path; env=%v", envSystemContextFile, spec.Env)
	}
	// The spill file carries the full context verbatim.
	var found bool
	for _, f := range spec.WorkDirFiles {
		if f.Name == envelopeFileName {
			found = true
			if string(f.Content) != bigContext {
				t.Errorf("spill file content truncated: got %d bytes, want %d", len(f.Content), len(bigContext))
			}
		}
	}
	if !found {
		t.Fatalf("codex must spill the context into %s; files=%v", envelopeFileName, spec.WorkDirFiles)
	}
	// The small work instruction still rides env inline.
	if !hasEnvPrefix(spec.Env, envInput+"=do the thing") {
		t.Errorf("codex must keep the bounded work instruction inline on %s; env=%v", envInput, spec.Env)
	}
}

// TestEnvChannelRuntimesSpillLargeContext asserts every env-channel runtime
// (codex/hermes/openclaw) spills a big context to the file and never emits an
// oversized env string.
func TestEnvChannelRuntimesSpillLargeContext(t *testing.T) {
	for _, flavor := range []string{
		apiv1alpha1.RuntimeTypeCodex,
		apiv1alpha1.RuntimeTypeHermes,
		apiv1alpha1.RuntimeTypeOpenClaw,
	} {
		t.Run(flavor, func(t *testing.T) {
			rt, _ := Get(flavor)
			spec, err := rt.Command(LaunchContext{
				Envelope: a2a.Envelope{SystemContext: bigContext, Input: "go"},
				WorkDir:  "/work",
			})
			if err != nil {
				t.Fatalf("Command: %v", err)
			}
			if name, n := maxEnvLen(spec.Env); n >= maxArgStrLen {
				t.Fatalf("%s env %s is %d bytes, over the %d exec ceiling", flavor, name, n, maxArgStrLen)
			}
			if !hasEnvPrefix(spec.Env, envSystemContextFile+"=") {
				t.Errorf("%s must set %s; env=%v", flavor, envSystemContextFile, spec.Env)
			}
			if !hasEnvelopeFile(spec.WorkDirFiles) {
				t.Errorf("%s must spill the context file; files=%v", flavor, spec.WorkDirFiles)
			}
		})
	}
}
