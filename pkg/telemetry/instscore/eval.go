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

package instscore

import (
	"fmt"
	"sort"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// evaluators maps every registered rule ID to its evaluator. The eval_test
// "registry coverage" guard asserts the map covers all of Rules, and each
// evaluator has table tests.
//
// Every evaluator returns a Verdict with deterministic evidence. The no-data
// policy: a rule whose target signal is entirely absent from the window
// fails with "no <signal> observed in window" — the specification has no
// N/A, and the conservative default keeps scores comparable across
// implementations (see the plan on ISI-7741).
var evaluators = map[string]func(*Window) Verdict{
	"LOG-001": evalLOG001,
	"LOG-002": evalLOG002,
	"LOG-003": evalLOG003,
	"MET-001": evalMET001,
	"MET-002": evalMET002,
	"MET-003": evalMET003,
	"MET-004": evalMET004,
	"MET-005": evalMET005,
	"MET-006": evalMET006,
	"MET-007": evalMET007,
	"MET-008": evalMET008,
	"RES-001": evalRES001,
	"RES-002": evalRES002,
	"RES-003": evalRES003,
	"RES-004": evalRES004,
	"RES-005": evalRES005,
	"RES-006": evalRES006,
	"RES-007": evalRES007,
	"RES-010": evalRES010,
	"SDK-001": evalSDK001,
	"SPA-001": evalSPA001,
	"SPA-002": evalSPA002,
	"SPA-003": evalSPA003,
	"SPA-004": evalSPA004,
	"SPA-005": evalSPA005,
	"SPA-006": evalSPA006,
}

// pass/fail verdict helpers keep evaluator bodies declarative.
func pass(ev string) Verdict { return Verdict{Status: StatusPass, Evidence: ev} }
func fail(ev string) Verdict { return Verdict{Status: StatusFail, Evidence: ev} }

// notEval marks a rule excluded from the formula (spec-mandated disclosure
// for rules an implementation does not implement).
func notEval(ev string) Verdict {
	return Verdict{Status: StatusNotEvaluated, Evidence: ev}
}

// noData builds the plan-mandated no-data failure for a rule, naming the
// target signals that are absent from the window (a multi-target rule like
// RES-004 only lists the missing ones).
func noData(w *Window, rule Rule) Verdict {
	var missing []string
	for _, t := range rule.Target {
		switch t {
		case TargetResource, TargetSDK:
			if len(w.Resources) == 0 {
				missing = append(missing, string(t))
			}
		case TargetSpan:
			if len(w.Spans) == 0 {
				missing = append(missing, string(t))
			}
		case TargetMetric:
			if len(w.Metrics) == 0 {
				missing = append(missing, string(t))
			}
		case TargetLog:
			if len(w.Logs) == 0 {
				missing = append(missing, string(t))
			}
		}
	}
	return fail("no " + strings.Join(missing, ", ") + " observed in window")
}

// evalLOG001: DEBUG logs are not observed in a production environment for
// longer than 14 days. Production is deployment.environment.name ∈
// {production, prod} (case-insensitive); the observation span is
// max-min timestamp of DEBUG records in production resources.
func evalLOG001(w *Window) Verdict {
	if len(w.Logs) == 0 {
		return noData(w, ruleFor("LOG-001"))
	}
	var minT, maxT uint64
	seen := false
	count := 0
	for _, l := range w.Logs {
		if l.SeverityText != "DEBUG" {
			continue
		}
		env := strings.ToLower(attrValueString(l.Resource.Attributes, "deployment.environment.name"))
		if env != "production" && env != "prod" {
			continue
		}
		count++
		if !seen || l.TimeNano < minT {
			minT = l.TimeNano
		}
		seen = true
		if l.TimeNano > maxT {
			maxT = l.TimeNano
		}
	}
	if count == 0 {
		return pass("no DEBUG-severity log records observed in production environments")
	}
	spanDays := float64(maxT-minT) / 1e9 / 86400
	if spanDays > 14 {
		return fail(fmt.Sprintf("%d DEBUG log records observed in production spanning %.1f days (> 14)", count, spanDays))
	}
	return pass(fmt.Sprintf("%d DEBUG log records observed in production, but only spanning %.1f days (≤ 14)", count, spanDays))
}

// evalLOG002: no log record with severity UNSET — implemented on the
// authoritative machine field: fail iff any record's severity_number is
// SEVERITY_NUMBER_UNSPECIFIED (0).
func evalLOG002(w *Window) Verdict {
	if len(w.Logs) == 0 {
		return noData(w, ruleFor("LOG-002"))
	}
	unset := 0
	for _, l := range w.Logs {
		if l.SeverityNumber == 0 {
			unset++
		}
	}
	if unset > 0 {
		return fail(fmt.Sprintf("%d log record(s) with severity_number UNSET observed", unset))
	}
	return pass(fmt.Sprintf("all %d log records carry a severity_number", len(w.Logs)))
}

// evalLOG003: log records contain no sensitive data. Scope per the rule:
// bodies, attributes, event names, instrumentation scope names/versions/
// attributes. The format detector is the rule's "common formats" set —
// deterministic, not exhaustive (the rule itself states as much).
func evalLOG003(w *Window) Verdict {
	if len(w.Logs) == 0 {
		return noData(w, ruleFor("LOG-003"))
	}
	var fs []finding
	for _, l := range w.Logs {
		if kind, ok := scanAnyValue(l.Body); ok {
			fs = append(fs, finding{Where: "body", Kind: kind})
		}
		fs = append(fs, fsFromAttrs("attribute", l.Attrs)...)
		fs = append(fs, fsFromScope(l.Scope)...)
	}
	if len(fs) > 0 {
		return fail(sensitiveEvidence(fs))
	}
	return pass(fmt.Sprintf("no sensitive data detected in %d log record(s) (format-detector bound: detects the rule's listed formats)", len(w.Logs)))
}

// evalMET001: per metric name and attribute key, unique values per 1-hour
// bucket must stay under 10,000.
func evalMET001(w *Window) Verdict {
	if len(w.Metrics) == 0 {
		return noData(w, ruleFor("MET-001"))
	}
	// (metric, hour, key) -> set of value signatures
	type key struct {
		metric string
		hour   uint64
		key    string
	}
	vals := map[key]map[string]struct{}{}
	for _, m := range w.Metrics {
		for _, p := range m.Points {
			hour := p.TimeNano / uint64(3_600_000_000_000)
			for _, kv := range p.Attrs {
				k := key{metric: m.Name, hour: hour, key: kv.GetKey()}
				if vals[k] == nil {
					vals[k] = map[string]struct{}{}
				}
				vals[k][anyValueString(kv.GetValue())] = struct{}{}
			}
		}
	}
	worst := 0
	var worstKey key
	for k, set := range vals {
		if len(set) > worst {
			worst = len(set)
			worstKey = k
		}
	}
	if worst >= 10_000 {
		return fail(fmt.Sprintf("attribute %q of metric %q reached %d unique values in one 1-hour window (limit: < 10000)", worstKey.key, worstKey.metric, worst))
	}
	return pass(fmt.Sprintf("max unique values for any (metric, attribute, hour): %d (< 10000)", worst))
}

// evalMET002: every metric carries a non-default unit. The OTLP proto
// default ("") is the default unit. UCUM codes are short symbols
// ("ms", "KiBy", "{req}", "By/s"); spelled-out word forms ("seconds",
// "milliseconds", "requests") are not valid UCUM, so a pure-letter unit of
// five or more characters fails as a word form (the longest pure-letter
// UCUM codes, e.g. "Cel", "mol", stay under the bound), as do units with
// whitespace or underscores.
func evalMET002(w *Window) Verdict {
	if len(w.Metrics) == 0 {
		return noData(w, ruleFor("MET-002"))
	}
	bad := map[string]int{}
	for _, m := range w.Metrics {
		if !ucumShapedUnit(m.Unit) {
			bad[m.Unit]++
		}
	}
	if len(bad) > 0 {
		return fail(fmt.Sprintf("%d metric(s) with default (empty) or non-UCUM (word-form/whitespace) unit: %s", sumCounts(bad), keysOf(bad)))
	}
	return pass(fmt.Sprintf("all %d metric(s) carry a non-default UCUM-shaped unit", len(w.Metrics)))
}

// ucumShapedUnit reports whether a unit string is plausibly UCUM: non-empty,
// no whitespace/underscores, and not a spelled-out word form.
func ucumShapedUnit(unit string) bool {
	if unit == "" || strings.ContainsAny(unit, " \t\n_") {
		return false
	}
	letters := 0
	for _, r := range unit {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			letters++
		}
	}
	return letters != len([]rune(unit)) || letters < 5
}

// evalMET003: a metric name maps to exactly one unit across the window.
func evalMET003(w *Window) Verdict {
	if len(w.Metrics) == 0 {
		return noData(w, ruleFor("MET-003"))
	}
	units := map[string]map[string]struct{}{}
	for _, m := range w.Metrics {
		if units[m.Name] == nil {
			units[m.Name] = map[string]struct{}{}
		}
		units[m.Name][m.Unit] = struct{}{}
	}
	var conflicted []string
	for name, set := range units {
		if len(set) > 1 {
			conflicted = append(conflicted, fmt.Sprintf("%s (%s)", name, strings.Join(sortedSet(set), ", ")))
		}
	}
	if len(conflicted) > 0 {
		sort.Strings(conflicted)
		return fail(fmt.Sprintf("metric name(s) with inconsistent units: %s", strings.Join(conflicted, "; ")))
	}
	return pass(fmt.Sprintf("all %d metric name(s) map to a single unit", len(units)))
}

// evalMET004: histogram metrics of one name use one bucket set across the
// window (exact explicit-bounds equality).
func evalMET004(w *Window) Verdict {
	if len(w.Metrics) == 0 {
		return noData(w, ruleFor("MET-004"))
	}
	buckets := map[string]map[string]struct{}{}
	seen := false
	for _, m := range w.Metrics {
		if m.Type != "histogram" {
			continue
		}
		seen = true
		if buckets[m.Name] == nil {
			buckets[m.Name] = map[string]struct{}{}
		}
		buckets[m.Name][m.BucketsSig] = struct{}{}
	}
	if !seen {
		return pass("no histogram metrics observed in window")
	}
	var conflicted []string
	for name, set := range buckets {
		if len(set) > 1 {
			conflicted = append(conflicted, fmt.Sprintf("%s (%d bucket sets)", name, len(set)))
		}
	}
	if len(conflicted) > 0 {
		sort.Strings(conflicted)
		return fail(fmt.Sprintf("histogram metric name(s) with inconsistent buckets: %s", strings.Join(conflicted, "; ")))
	}
	return pass(fmt.Sprintf("all %d histogram metric name(s) use a single bucket set", len(buckets)))
}

// evalMET005: metric names do not spell out their unit
// (vendored example: `collection.duration.seconds`).
func evalMET005(w *Window) Verdict {
	if len(w.Metrics) == 0 {
		return noData(w, ruleFor("MET-005"))
	}
	bad := map[string]bool{}
	for _, m := range w.Metrics {
		if nameContainsUnit(m.Name, m.Unit) {
			bad[m.Name] = true
		}
	}
	if len(bad) > 0 {
		return fail(fmt.Sprintf("metric name(s) containing their unit: %s", strings.Join(sortedSet(setOf(bad)), ", ")))
	}
	return pass(fmt.Sprintf("no metric name contains its unit (%d metric name(s) checked)", countNames(w.Metrics)))
}

// evalMET006: metric names do not equal semconv attribute keys.
func evalMET006(w *Window) Verdict {
	if len(w.Metrics) == 0 {
		return noData(w, ruleFor("MET-006"))
	}
	var bad []string
	for _, m := range w.Metrics {
		if semconvMetricName(m.Name) {
			bad = append(bad, m.Name)
		}
	}
	if len(bad) > 0 {
		return fail(fmt.Sprintf("metric name(s) equal to semconv attribute keys: %s", strings.Join(dedupSorted(bad), ", ")))
	}
	return pass("no metric name equals a semconv attribute key")
}

// evalMET007: resources of metric data carry none of the six volatile keys.
func evalMET007(w *Window) Verdict {
	if len(w.Metrics) == 0 {
		return noData(w, ruleFor("MET-007"))
	}
	forbidden := map[string]bool{
		"host.ip":                     true,
		"host.mac":                    true,
		"process.command_args":        true,
		"process.command_line":        true,
		"process.runtime.description": true,
		"os.description":              true,
	}
	seen := map[string]int{}
	for _, m := range w.Metrics {
		for _, kv := range m.Resource.Attributes {
			if forbidden[kv.GetKey()] {
				seen[kv.GetKey()]++
			}
		}
	}
	if len(seen) > 0 {
		return fail(fmt.Sprintf("volatile resource attribute(s) present on metric data: %s", strings.Join(sortedSet(setFromCounts(seen)), ", ")))
	}
	return pass("no volatile/verbose resource attributes on metric data")
}

// evalMET008: metric data contains no sensitive data. Scope per the rule:
// metric names, descriptions, units, data point attributes, exemplar
// attributes, scope names/versions/attributes.
func evalMET008(w *Window) Verdict {
	if len(w.Metrics) == 0 {
		return noData(w, ruleFor("MET-008"))
	}
	var fs []finding
	for _, m := range w.Metrics {
		if kind, ok := scanString(m.Name); ok {
			fs = append(fs, finding{Where: "metric name " + m.Name, Kind: kind})
		}
		if kind, ok := scanString(m.Unit); ok {
			fs = append(fs, finding{Where: "unit of " + m.Name, Kind: kind})
		}
		for _, p := range m.Points {
			fs = append(fs, fsFromAttrs("data point attribute", p.Attrs)...)
			for _, ex := range p.Exemplars {
				fs = append(fs, fsFromAttrs("exemplar attribute", ex.FilteredAttributes)...)
			}
		}
		fs = append(fs, fsFromScope(m.Scope)...)
	}
	if len(fs) > 0 {
		return fail(sensitiveEvidence(fs))
	}
	return pass(fmt.Sprintf("no sensitive data detected in %d metric(s) (format-detector bound: detects the rule's listed formats)", len(w.Metrics)))
}

// evalRES001: every resource carries a non-empty service.instance.id.
func evalRES001(w *Window) Verdict {
	if len(w.Resources) == 0 {
		return noData(w, ruleFor("RES-001"))
	}
	missing := 0
	for _, r := range w.Resources {
		if attrValueString(r.Attributes, "service.instance.id") == "" {
			missing++
		}
	}
	if missing > 0 {
		return fail(fmt.Sprintf("%d of %d resource(s) lack service.instance.id", missing, len(w.Resources)))
	}
	return pass(fmt.Sprintf("all %d resource(s) carry service.instance.id", len(w.Resources)))
}

// evalRES002: service.instance.id values are unique across logical
// resources. A logical resource is identified by k8s.pod.uid, falling back
// to k8s.pod.name, falling back to the full attribute fingerprint minus
// service.instance.id itself.
func evalRES002(w *Window) Verdict {
	if len(w.Resources) == 0 {
		return noData(w, ruleFor("RES-002"))
	}
	byInstance := map[string]map[string]struct{}{}
	for _, r := range w.Resources {
		id := attrValueString(r.Attributes, "service.instance.id")
		if id == "" {
			continue // RES-001 covers absence
		}
		if byInstance[id] == nil {
			byInstance[id] = map[string]struct{}{}
		}
		byInstance[id][logicalResourceID(r)] = struct{}{}
	}
	var dup []string
	for id, ids := range byInstance {
		if len(ids) > 1 {
			dup = append(dup, id)
		}
	}
	if len(dup) > 0 {
		sort.Strings(dup)
		return fail(fmt.Sprintf("service.instance.id value(s) shared by multiple logical resources: %s", strings.Join(dup, ", ")))
	}
	return pass(fmt.Sprintf("every service.instance.id maps to one logical resource (%d resource(s))", len(w.Resources)))
}

// logicalResourceID identifies the logical resource (pod) behind an
// attribute set; see evalRES002.
func logicalResourceID(r *ResourceEvidence) string {
	if v := attrStringRaw(r.Attributes, "k8s.pod.uid"); v != "" {
		return "poduid:" + v
	}
	if v := attrStringRaw(r.Attributes, "k8s.pod.name"); v != "" {
		return "podname:" + v
	}
	// fingerprint minus the instance id pair
	var parts []string
	for _, kv := range r.Attributes {
		if kv.GetKey() == "service.instance.id" {
			continue
		}
		parts = append(parts, kv.GetKey()+"="+anyValueString(kv.GetValue()))
	}
	sort.Strings(parts)
	return "attrs:" + strings.Join(parts, ";")
}

// evalRES003: k8s-associated resources carry k8s.pod.uid. A resource is
// k8s-associated when it carries any k8s.* attribute; non-k8s resources pass.
func evalRES003(w *Window) Verdict {
	if len(w.Resources) == 0 {
		return noData(w, ruleFor("RES-003"))
	}
	missing := 0
	k8s := 0
	for _, r := range w.Resources {
		isK8s := false
		for _, kv := range r.Attributes {
			if strings.HasPrefix(kv.GetKey(), "k8s.") {
				isK8s = true
				break
			}
		}
		if !isK8s {
			continue
		}
		k8s++
		if attrStringRaw(r.Attributes, "k8s.pod.uid") == "" {
			missing++
		}
	}
	if missing > 0 {
		return fail(fmt.Sprintf("%d of %d Kubernetes resource(s) lack k8s.pod.uid", missing, k8s))
	}
	if k8s == 0 {
		return pass("no Kubernetes-associated resources in window")
	}
	return pass(fmt.Sprintf("all %d Kubernetes resource(s) carry k8s.pod.uid", k8s))
}

// evalRES004: semconv attribute keys appear at the right OTLP level —
// resource-level keys not on spans/logs, record-level keys not on resources.
func evalRES004(w *Window) Verdict {
	if !w.HasSignal(TargetLog, TargetSpan) || len(w.Resources) == 0 {
		return noData(w, ruleFor("RES-004"))
	}
	var fs []string
	for _, r := range w.Resources {
		for _, kv := range r.Attributes {
			if recordLevelKeys[kv.GetKey()] {
				fs = append(fs, fmt.Sprintf("resource attribute %s (record-level key)", kv.GetKey()))
			}
		}
	}
	for _, s := range w.Spans {
		for _, kv := range s.Attrs {
			if resourceOnlyKeys[kv.GetKey()] {
				fs = append(fs, fmt.Sprintf("span attribute %s (resource-level key)", kv.GetKey()))
			}
		}
	}
	for _, l := range w.Logs {
		for _, kv := range l.Attrs {
			if resourceOnlyKeys[kv.GetKey()] {
				fs = append(fs, fmt.Sprintf("log attribute %s (resource-level key)", kv.GetKey()))
			}
		}
	}
	if len(fs) > 0 {
		return fail(fmt.Sprintf("%d misplaced attribute(s): %s", len(fs), joinCapped(dedupSorted(fs), 5)))
	}
	return pass("semconv attribute keys observed at the right level")
}

// evalRES005: every resource carries a non-empty service.name.
func evalRES005(w *Window) Verdict {
	if len(w.Resources) == 0 {
		return noData(w, ruleFor("RES-005"))
	}
	if w.ServiceName == "" {
		return fail("service.name missing or empty on the evaluated resources")
	}
	return pass(fmt.Sprintf("service.name %q present on all %d resource(s)", w.ServiceName, len(w.Resources)))
}

// evalRES006: service.criticality, when present, uses the canonical
// lowercase well-known value.
func evalRES006(w *Window) Verdict {
	if len(w.Resources) == 0 {
		return noData(w, ruleFor("RES-006"))
	}
	wellKnown := map[string]string{"critical": "critical", "high": "high", "medium": "medium", "low": "low"}
	var bad []string
	for _, r := range w.Resources {
		v := attrStringRaw(r.Attributes, "service.criticality")
		if v == "" {
			continue
		}
		trimmed := strings.TrimSpace(v)
		if canonical, isKnown := wellKnown[strings.ToLower(trimmed)]; isKnown && trimmed != canonical {
			bad = append(bad, fmt.Sprintf("%q", v))
		}
	}
	if len(bad) > 0 {
		return fail(fmt.Sprintf("service.criticality not in canonical lowercase form: %s", strings.Join(dedupSorted(bad), ", ")))
	}
	return pass("service.criticality absent or canonical")
}

// evalRES007: every resource carries a non-empty deployment.environment.name.
func evalRES007(w *Window) Verdict {
	if len(w.Resources) == 0 {
		return noData(w, ruleFor("RES-007"))
	}
	missing := 0
	for _, r := range w.Resources {
		if attrValueString(r.Attributes, "deployment.environment.name") == "" {
			missing++
		}
	}
	if missing > 0 {
		return fail(fmt.Sprintf("%d of %d resource(s) lack deployment.environment.name", missing, len(w.Resources)))
	}
	return pass(fmt.Sprintf("all %d resource(s) carry deployment.environment.name", len(w.Resources)))
}

// evalRES010: resource attribute values contain no sensitive data
// (including nested arrays such as process.command_args elements).
func evalRES010(w *Window) Verdict {
	if len(w.Resources) == 0 {
		return noData(w, ruleFor("RES-010"))
	}
	var fs []finding
	for _, r := range w.Resources {
		fs = append(fs, fsFromAttrs("resource attribute", r.Attributes)...)
	}
	if len(fs) > 0 {
		return fail(sensitiveEvidence(fs))
	}
	return pass(fmt.Sprintf("no sensitive data detected in %d resource(s) (format-detector bound: detects the rule's listed formats)", len(w.Resources)))
}

// evalSDK001 is not evaluated: the criteria ("versions of language and
// runtime found on the instrumentation dependencies for the SDK SHOULD be
// within supported values") requires an SDK support matrix, which is not
// derivable from OTLP evidence and is not vendored at the pin. The rule is
// disclosed as unimplemented per the specification's incompleteness clause.
func evalSDK001(w *Window) Verdict {
	return notEval("criteria requires an SDK language/runtime support matrix; not derivable from OTLP evidence at pin ef080a8a — excluded from the formula, disclosed per spec §Rules")
}

// evalSPA001: no (trace, service) group holds more than 10 INTERNAL spans.
func evalSPA001(w *Window) Verdict {
	if len(w.Spans) == 0 {
		return noData(w, ruleFor("SPA-001"))
	}
	type key struct{ trace, service string }
	counts := map[key]int{}
	for _, s := range w.Spans {
		if s.Kind == tracepb.Span_SPAN_KIND_INTERNAL {
			counts[key{s.TraceID, serviceNameOf(s.Resource)}]++
		}
	}
	worst := 0
	var worstTrace string
	for k, n := range counts {
		if n > worst {
			worst = n
			worstTrace = k.trace
		}
	}
	if worst > 10 {
		return fail(fmt.Sprintf("trace %s holds %d INTERNAL spans for one service (limit: 10)", traceIDHex(worstTrace), worst))
	}
	return pass(fmt.Sprintf("max INTERNAL spans in any (trace, service): %d (≤ 10)", worst))
}

// evalSPA002: every non-empty parent_span_id resolves to a span in the same
// trace. Matching is literal per the vendored criteria (a self-referencing
// parent therefore resolves; the criteria does not forbid it).
func evalSPA002(w *Window) Verdict {
	if len(w.Spans) == 0 {
		return noData(w, ruleFor("SPA-002"))
	}
	spansByTrace := map[string]map[string]struct{}{}
	for _, s := range w.Spans {
		if spansByTrace[s.TraceID] == nil {
			spansByTrace[s.TraceID] = map[string]struct{}{}
		}
		spansByTrace[s.TraceID][s.SpanID] = struct{}{}
	}
	orphans := 0
	for _, s := range w.Spans {
		if s.ParentID == "" {
			continue
		}
		if _, ok := spansByTrace[s.TraceID][s.ParentID]; !ok {
			orphans++
		}
	}
	if orphans > 0 {
		return fail(fmt.Sprintf("%d span(s) reference a parent_span_id with no matching span in their trace", orphans))
	}
	return pass(fmt.Sprintf("all parent references resolve within their traces (%d span(s))", len(w.Spans)))
}

// evalSPA003 is not evaluated: the vendored rule's Criteria field is a
// literal upstream TODO at pin ef080a8a, and inventing a cardinality bound
// would add a rule the specification does not define (which the spec's
// MUST-NOT clause forbids). Disclosed as unimplemented per the
// specification's incompleteness clause.
func evalSPA003(w *Window) Verdict {
	return notEval("criteria is an upstream TODO at pin ef080a8a; no bound is defined to evaluate — excluded from the formula, disclosed per spec §Rules")
}

// evalSPA004: root spans (empty parent_span_id) are not CLIENT spans.
func evalSPA004(w *Window) Verdict {
	if len(w.Spans) == 0 {
		return noData(w, ruleFor("SPA-004"))
	}
	bad := 0
	roots := 0
	for _, s := range w.Spans {
		if s.ParentID != "" {
			continue
		}
		roots++
		if s.Kind == tracepb.Span_SPAN_KIND_CLIENT {
			bad++
		}
	}
	if bad > 0 {
		return fail(fmt.Sprintf("%d of %d root span(s) have kind CLIENT", bad, roots))
	}
	return pass(fmt.Sprintf("no CLIENT root spans (%d root span(s))", roots))
}

// evalSPA005: no trace holds more than 20 spans shorter than 5 ms.
func evalSPA005(w *Window) Verdict {
	if len(w.Spans) == 0 {
		return noData(w, ruleFor("SPA-005"))
	}
	short := map[string]int{}
	for _, s := range w.Spans {
		if s.EndNano > s.StartNano && s.EndNano-s.StartNano < 5_000_000 {
			short[s.TraceID]++
		}
	}
	worst := 0
	var worstTrace string
	for t, n := range short {
		if n > worst {
			worst = n
			worstTrace = t
		}
	}
	if worst > 20 {
		return fail(fmt.Sprintf("trace %s holds %d spans shorter than 5ms (limit: 20)", traceIDHex(worstTrace), worst))
	}
	return pass(fmt.Sprintf("max sub-5ms spans in any trace: %d (≤ 20)", worst))
}

// evalSPA006: span data contains no sensitive data. Scope per the rule:
// names, status messages, attributes, event names and attributes, link
// attributes, scope names/versions/attributes.
func evalSPA006(w *Window) Verdict {
	if len(w.Spans) == 0 {
		return noData(w, ruleFor("SPA-006"))
	}
	var fs []finding
	for _, s := range w.Spans {
		if kind, ok := scanString(s.Name); ok {
			fs = append(fs, finding{Where: "span name", Kind: kind})
		}
		if kind, ok := scanString(s.StatusMsg); ok {
			fs = append(fs, finding{Where: "status message", Kind: kind})
		}
		fs = append(fs, fsFromAttrs("attribute", s.Attrs)...)
		for _, e := range s.Events {
			if kind, ok := scanString(e.Name); ok {
				fs = append(fs, finding{Where: "event name", Kind: kind})
			}
			fs = append(fs, fsFromAttrs("event attribute", e.Attributes)...)
		}
		for _, l := range s.Links {
			fs = append(fs, fsFromAttrs("link attribute", l.Attributes)...)
		}
		fs = append(fs, fsFromScope(s.Scope)...)
	}
	if len(fs) > 0 {
		return fail(sensitiveEvidence(fs))
	}
	return pass(fmt.Sprintf("no sensitive data detected in %d span(s) (format-detector bound: detects the rule's listed formats)", len(w.Spans)))
}

// ---- shared helpers ----

func ruleFor(id string) Rule {
	r, _ := ByID(id)
	return r
}

func serviceNameOf(r *ResourceEvidence) string { return attrValueString(r.Attributes, "service.name") }

// attrStringRaw returns the string value of a key even when empty, and
// whether the key exists as a string attribute.
func attrStringRaw(attrs []*commonpb.KeyValue, key string) string {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

func fsFromAttrs(prefix string, kvs []*commonpb.KeyValue) []finding {
	var out []finding
	for _, kv := range kvs {
		if kind, ok := scanKeyValue(kv.GetKey(), kv.GetValue()); ok {
			out = append(out, finding{Where: prefix + " " + kv.GetKey(), Kind: kind})
		}
	}
	return out
}

func fsFromScope(scope *commonpb.InstrumentationScope) []finding {
	if scope == nil {
		return nil
	}
	var out []finding
	if kind, ok := scanString(scope.Name); ok {
		out = append(out, finding{Where: "scope name", Kind: kind})
	}
	if kind, ok := scanString(scope.Version); ok {
		out = append(out, finding{Where: "scope version", Kind: kind})
	}
	out = append(out, fsFromAttrs("scope attribute", scope.Attributes)...)
	return out
}

func sensitiveEvidence(fs []finding) string {
	return "sensitive data detected — " + capFindings(fs, 5)
}

func sumCounts(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func keysOf(m map[string]int) string { return strings.Join(sortedSet(setFromCounts(m)), ", ") }

func setFromCounts(m map[string]int) map[string]struct{} {
	out := map[string]struct{}{}
	for k := range m {
		out[k] = struct{}{}
	}
	return out
}

func setOf(m map[string]bool) map[string]struct{} {
	out := map[string]struct{}{}
	for k := range m {
		out[k] = struct{}{}
	}
	return out
}

func sortedSet(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func countNames(ms []*MetricEvidence) int {
	names := map[string]struct{}{}
	for _, m := range ms {
		names[m.Name] = struct{}{}
	}
	return len(names)
}

func dedupSorted(in []string) []string {
	set := map[string]struct{}{}
	for _, s := range in {
		set[s] = struct{}{}
	}
	return sortedSet(set)
}

func joinCapped(parts []string, limit int) string {
	if len(parts) <= limit {
		return strings.Join(parts, "; ")
	}
	return strings.Join(parts[:limit], "; ") + fmt.Sprintf("; … and %d more", len(parts)-limit)
}

// traceIDHex renders a binary trace id as hex for evidence strings.
func traceIDHex(t string) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 0, len(t)*2)
	for i := 0; i < len(t); i++ {
		b := t[i]
		out = append(out, hexdigits[b>>4], hexdigits[b&0xf])
	}
	if len(out) > 16 {
		out = out[:16]
	}
	s := string(out)
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return s[i:]
		}
	}
	return s
}
