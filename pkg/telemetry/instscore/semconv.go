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

import "strings"

// This file pins the semantic-conventions reference data the evaluators
// compare observed telemetry against. The lists are deliberately small,
// documented, and versioned with the code: RES-004 and MET-006 need a
// machine-checkable notion of "an attribute key specified in the semantic
// conventions" and "the right level for it", and the spec rules reference
// these registries without vendoring them. Extending a list only ever makes
// the evaluators stricter for newly-listed keys; additions should cite the
// semconv registry entry they mirror.

// resourceOnlyKeys are semconv attribute keys that belong on the Resource.
// Observing them in span or log record attributes fails RES-004.
var resourceOnlyKeys = map[string]bool{
	// service registry (https://opentelemetry.io/docs/specs/semconv/resource/)
	"service.name":                true,
	"service.version":             true,
	"service.namespace":           true,
	"service.instance.id":         true,
	"service.criticality":         true,
	"deployment.environment.name": true,
	"deployment.environment":      true, // deprecated spelling, still resource-level
	"service.build.id":            true,
	"service.instance.name":       true, // deprecated
	// telemetry SDK identity
	"telemetry.sdk.name":     true,
	"telemetry.sdk.language": true,
	"telemetry.sdk.version":  true,
	"telemetry.auto.version": true,
	// process / runtime (resource-level per semconv)
	"process.pid":                 true,
	"process.executable.name":     true,
	"process.executable.path":     true,
	"process.command_args":        true,
	"process.command_line":        true,
	"process.owner":               true,
	"process.runtime.name":        true,
	"process.runtime.version":     true,
	"process.runtime.description": true,
	// host / OS
	"host.id":        true,
	"host.name":      true,
	"host.type":      true,
	"host.arch":      true,
	"host.ip":        true,
	"host.mac":       true,
	"os.type":        true,
	"os.name":        true,
	"os.version":     true,
	"os.description": true,
	// container / k8s (resource-level)
	"container.name":         true,
	"container.id":           true,
	"container.runtime.name": true,
	"container.image.name":   true,
	"container.image.tag":    true,
	"k8s.cluster.name":       true,
	"k8s.namespace.name":     true,
	"k8s.pod.name":           true,
	"k8s.pod.uid":            true,
	"k8s.pod.ip":             true,
	"k8s.node.name":          true,
	"k8s.node.uid":           true,
	"k8s.deployment.name":    true,
	"k8s.replicaset.name":    true,
	"k8s.statefulset.name":   true,
	"k8s.daemonset.name":     true,
	"k8s.job.name":           true,
	"k8s.cronjob.name":       true,
	// cloud
	"cloud.provider":          true,
	"cloud.account.id":        true,
	"cloud.region":            true,
	"cloud.availability_zone": true,
	"cloud.resource_id":       true,
}

// recordLevelKeys are semconv attribute keys that belong on records (spans,
// logs, metric data points). Observing them in resource attributes fails
// RES-004.
var recordLevelKeys = map[string]bool{
	// enduser (record-level)
	"enduser.id":    true,
	"enduser.role":  true,
	"enduser.scope": true,
	// http (span/log record level)
	"http.request.method":          true,
	"http.request.method_previous": true,
	"http.response.status_code":    true,
	"http.route":                   true,
	"http.request.body.size":       true,
	"http.response.body.size":      true,
	"http.request.header":          true,
	"http.response.header":         true,
	// url
	"url.full":     true,
	"url.scheme":   true,
	"url.path":     true,
	"url.query":    true,
	"url.fragment": true,
	// db (client/request record level)
	"db.system":               true,
	"db.statement":            true,
	"db.operation.name":       true,
	"db.operation.batch.size": true,
	"db.collection.name":      true,
	"db.namespace":            true,
	"db.query.text":           true,
	"db.response.status_code": true,
	// rpc / messaging / faas record-level keys
	"rpc.system":                 true,
	"rpc.service":                true,
	"rpc.method":                 true,
	"messaging.system":           true,
	"messaging.destination.name": true,
	"messaging.operation.name":   true,
	"messaging.message.id":       true,
	"faas.trigger":               true,
	"faas.execution":             true,
	"faas.invocation_id":         true,
	// exception (event/record level)
	"exception.type":       true,
	"exception.message":    true,
	"exception.stacktrace": true,
	// thread
	"thread.id":   true,
	"thread.name": true,
}

// semconvMetricNames is the MET-006 deny-list: a metric name that exactly
// equals one of these semconv attribute keys fails the rule (the vendored
// example is `http.response.status_code`). Merged from both level maps —
// any semconv attribute key is a confusing metric name.
func semconvMetricName(name string) bool {
	return resourceOnlyKeys[name] || recordLevelKeys[name]
}

// unitWords maps UCUM unit codes to the English word forms a metric name must
// not contain (MET-005, vendored example: `collection.duration.seconds`).
// Single-symbol codes cannot be matched as substrings without absurd false
// positives ("m" matches everything), so they resolve to their word form
// only; multi-character codes match both the code itself and the word form.
var unitWords = map[string][]string{
	"s":       {"second"},
	"min":     {"minute"},
	"h":       {"hour"},
	"d":       {"day"},
	"By":      {"byte"},
	"bit":     {"bit"},
	"%":       {"percent"},
	"V":       {"volt"},
	"A":       {"amp"},
	"W":       {"watt"},
	"J":       {"joule"},
	"Hz":      {"hertz"},
	"Cel":     {"celsius"},
	"B":       {"bel"},
	"odbm":    {"dbm"},
	"{r}":     {"request"},
	"{req}":   {"request"},
	"{obj}":   {"object"},
	"{err}":   {"error"},
	"{pg}":    {"page"},
	"{msg}":   {"message"},
	"{query}": {"query"},
}

// unitPrefixes are UCUM (and binary) prefixes stripped before word matching,
// so "ms" → second, "KiBy" → byte, "MBy" → byte.
var unitPrefixes = []string{
	"Ki", "Mi", "Gi", "Ti", "Pi", "Ei", "Zi", "Yi",
	"Y", "Z", "E", "P", "T", "G", "M", "k", "h", "da", "d", "c", "m", "u", "µ", "n", "p", "f", "a", "z", "y",
}

// nameContainsUnit reports whether a metric name spells out its unit (MET-005).
// Matching is case-insensitive on the lowercased name. Words match as
// substrings ("seconds" contains "second"); bare codes match only when the
// code is at least three characters ("KiBy" matches, "s" does not).
func nameContainsUnit(name, unit string) bool {
	if unit == "" {
		return false
	}
	lname := strings.ToLower(name)
	for _, word := range unitWordForms(unit) {
		if strings.Contains(lname, word) {
			return true
		}
	}
	return false
}

// unitWordForms expands a UCUM code into the lowercase substrings that count
// as "the unit appearing in the name".
func unitWordForms(unit string) []string {
	lunit := strings.ToLower(unit)
	var forms []string
	if len(lunit) >= 3 {
		forms = append(forms, lunit)
	}
	base := unit
	for _, p := range unitPrefixes {
		if len(unit) > len(p) && strings.HasPrefix(unit, p) {
			base = strings.TrimPrefix(unit, p)
			break
		}
	}
	if words, ok := unitWords[base]; ok {
		forms = append(forms, words...)
	}
	return forms
}
