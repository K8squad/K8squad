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

// Package instscore scores observed workload telemetry against the
// instrumentation-score rule set (https://github.com/instrumentation-score/spec),
// vendored at third_party/instrumentation-score.
//
// This file is the rule registry: the authoritative Go listing of every
// vendored rule (id, target, impact). The rulesync test in rulesync_test.go
// asserts the registry matches the vendored markdown, so upstream drift is a
// visible test failure rather than a silent score change.
package instscore

import "strings"

// Impact is a rule's assigned importance level. The specification allows
// exactly Critical, Important, Normal, and Low.
type Impact string

const (
	ImpactCritical  Impact = "Critical"
	ImpactImportant Impact = "Important"
	ImpactNormal    Impact = "Normal"
	ImpactLow       Impact = "Low"
)

// Weight returns the score weight the specification assigns to the impact
// level: Critical=40, Important=30, Normal=20, Low=10. It returns 0 for an
// unknown level, which the rulesync test guards against.
func (i Impact) Weight() int {
	switch i {
	case ImpactCritical:
		return 40
	case ImpactImportant:
		return 30
	case ImpactNormal:
		return 20
	case ImpactLow:
		return 10
	default:
		return 0
	}
}

// Target names the telemetry signal(s) a rule is evaluated against:
// Resource, Span, Metric, Log, or SDK. Multi-signal rules list each signal.
type Target string

const (
	TargetResource Target = "Resource"
	TargetSpan     Target = "Span"
	TargetMetric   Target = "Metric"
	TargetLog      Target = "Log"
	TargetSDK      Target = "SDK"
)

// Rule is one instrumentation-score rule as vendored at the pinned commit
// (see third_party/instrumentation-score/README.md for the pin).
type Rule struct {
	// ID is the rule identifier, e.g. "RES-001".
	ID string
	// Target lists the signals the rule applies to, in vendored order.
	Target []Target
	// Impact is the rule's importance level.
	Impact Impact
}

// Rules is the full registry: every rule vendored at pin
// ef080a8a42d2fe73a4f0ed8d64eda93bfe84adc7 (26 rules), sorted by ID.
// rulesync_test.go asserts this slice matches the vendored markdown.
var Rules = []Rule{
	{ID: "LOG-001", Target: []Target{TargetLog}, Impact: ImpactImportant},
	{ID: "LOG-002", Target: []Target{TargetLog}, Impact: ImpactImportant},
	{ID: "LOG-003", Target: []Target{TargetLog}, Impact: ImpactCritical},
	{ID: "MET-001", Target: []Target{TargetMetric}, Impact: ImpactImportant},
	{ID: "MET-002", Target: []Target{TargetMetric}, Impact: ImpactImportant},
	{ID: "MET-003", Target: []Target{TargetMetric}, Impact: ImpactImportant},
	{ID: "MET-004", Target: []Target{TargetMetric}, Impact: ImpactNormal},
	{ID: "MET-005", Target: []Target{TargetMetric}, Impact: ImpactNormal},
	{ID: "MET-006", Target: []Target{TargetMetric}, Impact: ImpactImportant},
	{ID: "MET-007", Target: []Target{TargetMetric}, Impact: ImpactNormal},
	{ID: "MET-008", Target: []Target{TargetMetric}, Impact: ImpactCritical},
	{ID: "RES-001", Target: []Target{TargetResource}, Impact: ImpactNormal},
	{ID: "RES-002", Target: []Target{TargetResource}, Impact: ImpactImportant},
	{ID: "RES-003", Target: []Target{TargetResource}, Impact: ImpactImportant},
	{ID: "RES-004", Target: []Target{TargetResource, TargetLog, TargetSpan}, Impact: ImpactImportant},
	{ID: "RES-005", Target: []Target{TargetResource}, Impact: ImpactCritical},
	{ID: "RES-006", Target: []Target{TargetResource}, Impact: ImpactNormal},
	{ID: "RES-007", Target: []Target{TargetResource}, Impact: ImpactImportant},
	{ID: "RES-010", Target: []Target{TargetResource}, Impact: ImpactCritical},
	{ID: "SDK-001", Target: []Target{TargetSDK}, Impact: ImpactLow},
	{ID: "SPA-001", Target: []Target{TargetSpan}, Impact: ImpactNormal},
	{ID: "SPA-002", Target: []Target{TargetSpan}, Impact: ImpactNormal},
	{ID: "SPA-003", Target: []Target{TargetSpan}, Impact: ImpactImportant},
	{ID: "SPA-004", Target: []Target{TargetSpan}, Impact: ImpactImportant},
	{ID: "SPA-005", Target: []Target{TargetSpan}, Impact: ImpactImportant},
	{ID: "SPA-006", Target: []Target{TargetSpan}, Impact: ImpactCritical},
}

// ByID returns the registry entry for a rule ID.
func ByID(id string) (Rule, bool) {
	for _, r := range Rules {
		if r.ID == id {
			return r, true
		}
	}
	return Rule{}, false
}

// parseTargets splits a vendored Target line value ("Resource, Log, Span")
// into individual signals.
func parseTargets(s string) []Target {
	parts := strings.Split(s, ",")
	out := make([]Target, 0, len(parts))
	for _, p := range parts {
		out = append(out, Target(strings.TrimSpace(p)))
	}
	return out
}
