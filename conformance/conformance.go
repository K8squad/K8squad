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

// Package conformance is the runnable KSquad A2A shim conformance suite
// (story 5.6, arch §7.5, spec §11.2). It is the vendor-facing proof that a
// runtime shim "works in any squad, zero core changes": a shim that passes is
// safe for the core reconciler to drive, because every A2A invariant the core
// relies on has been asserted independently.
//
// The suite checks the six dimensions the acceptance criteria name:
//
//	CheckAgentCard          — Agent Card schema validity (spec §6.1)
//	CheckTaskLifecycle      — §3.1 state machine, submit-reattach dedup (C1),
//	                          idempotent cancel (C8)
//	CheckSSEProgress        — gap-free monotonic SSE sequencing + resume (C4)
//	CheckArtifactEmission   — §5 artifact-ref shape + work-item binding
//	CheckCapabilityHonesty  — the card advertises no capability the runtime
//	                          fails to honor, and no runtime exercises a
//	                          capability it did not advertise (F15)
//	CheckCredentialMetadata — the auth block is metadata only; the raw
//	                          credential never appears on the card or the wire
//	                          (NFR-SEC3)
//
// The same assertions run on the default lane (the runtime's fixed vendor
// wire) and the Ollama lane (LaneOllama) — the runtime's model resolved to a
// BYO Ollama endpoint (story 5.7), driven with a zero-cost placeholder
// credential, giving a vendor a $0 way to prove conformance (ISI-2157).
//
// The suite is transport-honest: it drives the real pkg/shim engine through a
// scripted Runner so every lifecycle, sequencing and dedup guarantee is
// exercised without a live coding-agent CLI, and it is therefore runnable in a
// $0 CI lane with no external process. A vendor certifies their adapter by
// registering it (runtimes.Register) and running VerifyRuntime — zero core
// change.
package conformance

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Check identifies one conformance dimension (story 5.6 acceptance criteria).
type Check string

// The six conformance dimensions.
const (
	CheckAgentCard          Check = "agent-card-validity"
	CheckTaskLifecycle      Check = "task-lifecycle"
	CheckSSEProgress        Check = "sse-progress"
	CheckArtifactEmission   Check = "artifact-emission"
	CheckCapabilityHonesty  Check = "capability-honesty"
	CheckCredentialMetadata Check = "credential-metadata" // #nosec G101 -- a check name, not a credential.
)

// AllChecks is the ordered set of conformance dimensions the suite runs. A
// shim is conformant only when every check passes.
func AllChecks() []Check {
	return []Check{
		CheckAgentCard,
		CheckTaskLifecycle,
		CheckSSEProgress,
		CheckArtifactEmission,
		CheckCapabilityHonesty,
		CheckCredentialMetadata,
	}
}

// Lane names a model-provider lane the suite runs the assertions against.
type Lane string

const (
	// LaneDefault runs the runtime against its own fixed-vendor wire.
	LaneDefault Lane = "default"
	// LaneOllama runs the runtime with its model resolved to a BYO Ollama
	// endpoint (story 5.7) and a zero-cost placeholder credential — the $0
	// conformance lane (story 5.6 / ISI-2157). A runtime that does not
	// advertise byoModelEndpoint is not eligible for this lane.
	LaneOllama Lane = "ollama"
)

// Result is the outcome of one Check.
type Result struct {
	Check  Check  `json:"check"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

// Report is the full conformance outcome for one subject on one lane.
type Report struct {
	Subject string   `json:"subject"`
	Lane    Lane     `json:"lane"`
	Results []Result `json:"results"`
}

// OK reports whether every check passed (the gate-blocking verdict).
func (r Report) OK() bool {
	for _, res := range r.Results {
		if !res.Pass {
			return false
		}
	}
	return true
}

// Failures returns the checks that did not pass, for a compact verdict line.
func (r Report) Failures() []Result {
	var out []Result
	for _, res := range r.Results {
		if !res.Pass {
			out = append(out, res)
		}
	}
	return out
}

// String renders a human-readable verdict block for CLI output.
func (r Report) String() string {
	var b strings.Builder
	verdict := "PASS"
	if !r.OK() {
		verdict = "FAIL"
	}
	fmt.Fprintf(&b, "%s  %s [lane=%s]\n", verdict, r.Subject, r.Lane)
	for _, res := range r.Results {
		mark := "✓"
		if !res.Pass {
			mark = "✗"
		}
		fmt.Fprintf(&b, "  %s %-24s %s\n", mark, res.Check, res.Detail)
	}
	return b.String()
}

// checker accumulates per-Check results and enforces that every dimension is
// reported exactly once so a silently-skipped check can never read as a pass.
type checker struct {
	order   []Check
	results map[Check]Result
}

func newChecker() *checker {
	return &checker{results: map[Check]Result{}}
}

// pass records a passing check with a short evidence detail.
func (c *checker) pass(check Check, detail string) {
	c.record(Result{Check: check, Pass: true, Detail: detail})
}

// fail records a failing check with the reason it failed.
func (c *checker) fail(check Check, format string, args ...any) {
	c.record(Result{Check: check, Pass: false, Detail: fmt.Sprintf(format, args...)})
}

// require records pass/fail from a boolean, using failMsg only on failure.
func (c *checker) require(check Check, ok bool, passMsg, failMsg string) bool {
	if ok {
		c.pass(check, passMsg)
	} else {
		c.fail(check, "%s", failMsg)
	}
	return ok
}

func (c *checker) record(r Result) {
	if _, seen := c.results[r.Check]; !seen {
		c.order = append(c.order, r.Check)
	}
	// First failure on a dimension wins; a later pass never overwrites a
	// recorded failure (a check is conformant only if nothing broke it).
	if existing, seen := c.results[r.Check]; seen && !existing.Pass {
		return
	}
	c.results[r.Check] = r
}

// report finalizes the results, guaranteeing every AllChecks() dimension is
// present (a dimension never recorded is a hard fail — the suite refuses to
// certify a check it did not actually run).
func (c *checker) report(subject string, lane Lane) Report {
	for _, check := range AllChecks() {
		if _, seen := c.results[check]; !seen {
			c.fail(check, "check did not run (harness gap)")
		}
	}
	out := make([]Result, 0, len(c.order))
	for _, check := range AllChecks() {
		out = append(out, c.results[check])
	}
	return Report{Subject: subject, Lane: lane, Results: out}
}

// containsSecret reports whether the JSON encoding of v contains the sentinel
// credential anywhere — the credential-leak scan primitive. A non-marshalable
// value fails closed (treated as a potential leak) so a scan gap can never be
// read as "no leak".
func containsSecret(v any, secret string) (bool, error) {
	if secret == "" {
		return false, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return true, fmt.Errorf("credential scan could not marshal value: %w", err)
	}
	return strings.Contains(string(raw), secret), nil
}

// isSubset reports whether every element of sub is present in super.
func isSubset(sub, super []string) (string, bool) {
	set := make(map[string]struct{}, len(super))
	for _, s := range super {
		set[s] = struct{}{}
	}
	missing := make([]string, 0)
	for _, s := range sub {
		if _, ok := set[s]; !ok {
			missing = append(missing, s)
		}
	}
	sort.Strings(missing)
	return strings.Join(missing, ","), len(missing) == 0
}
