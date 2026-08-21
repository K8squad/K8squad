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

package sandbox

import (
	"fmt"
	"hash/fnv"
	"strings"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// Pod name/label constants for sandbox pods (stories 4.2/4.5).
const (
	// LabelRun ties a sandbox pod to the one Run it serves. A pod carries
	// exactly one value over its lifetime — the teardown-and-replace
	// contract (§9.3) makes rebinding a construction failure.
	LabelRun = "ksquad.io/run"

	// LabelSquad ties a sandbox pod to its Team namespace's squad.
	LabelSquad = "ksquad.io/team"

	// SandboxPodPrefix is the name prefix of every sandbox pod. The Run name
	// plus a short hash of the Run UID disambiguate (DNS-1123-safe,
	// collision-safe — same discipline as the 4.1 namespace derivation).
	SandboxPodPrefix = "ksquad-sandbox-"
)

// PodNameFor derives the deterministic sandbox pod name for a Run:
// ksquad-sandbox-<normalized-run-name>-<8-hex-hash(run.uid)> (story 4.2 AC3:
// per-Run pod — one Run, one sandbox).
func PodNameFor(run *api.Run) string {
	normalized := NormalizeRunName(run.Name)
	const budget = 63 - len(SandboxPodPrefix) - 1 - 8
	if len(normalized) > budget {
		normalized = normalized[:budget]
	}
	return fmt.Sprintf("%s%s-%s", SandboxPodPrefix, normalized, shortHashRun(string(run.UID)))
}

// NormalizeRunName maps a Run name onto a DNS-1123-safe fragment (same rules
// as team.NormalizeName, kept local so the sandbox package has no import
// cycle with the controller package).
func NormalizeRunName(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "run"
	}
	return out
}

func shortHashRun(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())[:8]
}
