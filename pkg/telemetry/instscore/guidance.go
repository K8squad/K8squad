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

// Failed-rule guidance (phase 4 of the ISI-7741 plan): the read surface
// (apiserver GET /api/v1/instscore/services*) enriches each verdict with the
// rule's description and — for failed rules — remediation guidance, so the
// console can show WHAT to fix without leaving the page.
//
// Like the rest of this package the text is PINNED to the vendored rules at
// commit ef080a8a42d2fe73a4f0ed8d64eda93bfe84adc7 rather than parsed at
// runtime: guidance.go carries the vendored Description and Criteria verbatim,
// normalized to single-space prose (the vendored markdown escapes "=" as "\="
// and wraps long paragraphs; multi-paragraph Criteria run to the next bold
// header). The extended rulesync test re-extracts the vendored text with the
// same normalization and asserts this file matches, so upstream drift stays a
// visible test failure, never a silent guidance change.

package instscore

import "strings"

// ruleDescriptions pins each vendored rule's one-line Description.
var ruleDescriptions = map[string]string{
	"LOG-001": "Debug-level logs are not enabled in production environments for longer than 14 days.",
	"LOG-002": "Log records have their `severityNumber` set.",
	"LOG-003": "Log records do not contain sensitive data such as PII, financial identifiers, credentials, or health information.",
	"MET-001": "Metric attributes have bound cardinality.",
	"MET-002": "Metrics have useful metric units.",
	"MET-003": "Metric names are consistently associated with the same metric unit.",
	"MET-004": "Histogram metrics consistently use the same histogram buckets per metric name.",
	"MET-005": "Metric names do not contain the name of the metric unit.",
	"MET-006": "Metric names do not equal semantic convention attribute keys.",
	"MET-007": "Metrics do not carry volatile or verbose resource attributes.",
	"MET-008": "Metrics do not contain sensitive data such as PII, financial identifiers, credentials, or health information.",
	"RES-001": "`service.instance.id` is present.",
	"RES-002": "`service.instance.id` is unique across logical resources within a given `service.name`.",
	"RES-003": "`k8s.pod.uid` is present in telemetry collected from applications running on a Kubernetes cluster, or from the control pane of the Kubernetes cluster itself.",
	"RES-004": "Semantic conventions attributes are used at the right level.",
	"RES-005": "`service.name` is present",
	"RES-006": "`service.criticality` uses the canonical form of its well-known values",
	"RES-007": "`deployment.environment.name` is present",
	"RES-010": "Resource attribute values do not contain sensitive data such as PII, financial identifiers, credentials, or health information.",
	"SDK-001": "Dependencies (language and runtime) are supported by the SDK.",
	"SPA-001": "Traces contain a limited number of `INTERNAL` spans per service.",
	"SPA-002": "Traces do not contain orphan spans.",
	"SPA-003": "Span names have bound cardinality.",
	"SPA-004": "Root spans are not `CLIENT` spans.",
	"SPA-005": "Traces do not contain a high number of short duration spans.",
	"SPA-006": "Spans do not contain sensitive data such as PII, financial identifiers, credentials, or health information.",
}

// ruleGuidance pins each vendored rule's Criteria — the remediation target a
// failed rule's caller should compare their instrumentation against.
var ruleGuidance = map[string]string{
	"LOG-001": "Log records with `severity.text` = `DEBUG` are not observed in a production environment for longer than 14 days, where a production environment is defined by the value of `deployment.environment.name` _Resource_ attribute.",
	"LOG-002": "No log record with `severity.text` = `UNSET` is observed.",
	"LOG-003": "Log record data MUST NOT contain sensitive data. Common formats include: - Email addresses (e.g., `user@example.com`) - Credit card numbers (13-19 digits, after removing spaces and hyphens, that pass Luhn validation) - Social security numbers or national ID numbers (e.g., `123-45-6789`) - User identifiers that can be tied back to a person, such as `enduser.id`, even when opaque or pseudonymous - Bearer tokens, API keys, or other credentials (e.g., `Bearer eyJ...`, `sk-...`, `AKIA...`) - Passwords or secrets in key-value patterns (e.g., `password=...`, `secret=...`, `token=...`) - Financial identifiers such as bank account numbers or tax IDs - Health data in key-value patterns (e.g., `diagnosis=...`, `patient_id=...`, `insurance_id=...`) The formats above are examples, not an exhaustive list, and this rule intentionally does not prescribe regular expressions. Detecting only the listed formats is not sufficient to pass: sensitive data in any other format, including formats introduced after this rule was written (such as the alphanumeric Brazilian CNPJ), also violates this rule. This rule applies to every string and integer value in log record data (integers are matched in their decimal form), including values nested in array or map values: - Log record bodies - Log record attributes - Log record event names - Instrumentation scope names, versions, and attributes Keys are evaluated together with their values: a non-empty value under a key naming a sensitive field, such as `password`, `secret`, `token`, `api_key`, or `ssn`, matches regardless of its format.",
	"MET-001": "Attribute keys on metrics, aggregated by metric name, MUST have less than 10.000 unique values within a 1-hour window.",
	"MET-002": "All Metrics MUST have a non-default unit of measurement compliant with [Unified Code for Units of Measure](https://github.com/ucum-org/ucum).",
	"MET-003": "Given a metric name, all time series with that metric name in the past 14 days MUST have the same unit of measurement.",
	"MET-004": "Given a metric name, all time series of type histogram with that metric name in the past 14 days MUST have the same histogram buckets.",
	"MET-005": "Metric names MUST NOT contain the metric unit (example: `collection.duration.seconds`).",
	"MET-006": "Metric names MUST NOT equal any of the attribute keys specified in semantic conventions.",
	"MET-007": "Resource attributes of metric data MUST NOT include any of the following keys: `host.ip`, `host.mac`, `process.command_args`, `process.command_line`, `process.runtime.description`, `os.description`.",
	"MET-008": "Metric data MUST NOT contain sensitive data. Common formats include: - Email addresses (e.g., `user@example.com`) - Credit card numbers (13-19 digits, after removing spaces and hyphens, that pass Luhn validation) - Social security numbers or national ID numbers (e.g., `123-45-6789`) - User identifiers that can be tied back to a person, such as `enduser.id`, even when opaque or pseudonymous - Bearer tokens, API keys, or other credentials (e.g., `Bearer eyJ...`, `sk-...`, `AKIA...`) - Passwords or secrets in key-value patterns (e.g., `password=...`, `secret=...`, `token=...`) - Financial identifiers such as bank account numbers or tax IDs - Health data in key-value patterns (e.g., `diagnosis=...`, `patient_id=...`, `insurance_id=...`) The formats above are examples, not an exhaustive list, and this rule intentionally does not prescribe regular expressions. Detecting only the listed formats is not sufficient to pass: sensitive data in any other format, including formats introduced after this rule was written (such as the alphanumeric Brazilian CNPJ), also violates this rule. This rule applies to every string and integer value in metric data (integers are matched in their decimal form), including values nested in array or map values: - Metric names, descriptions, and units - Data point attributes - Exemplar attributes - Instrumentation scope names, versions, and attributes Keys are evaluated together with their values: a non-empty value under a key naming a sensitive field, such as `password`, `secret`, `token`, `api_key`, or `ssn`, matches regardless of its format.",
	"RES-001": "The `service.instance.id` resource attribute MUST appear in the resource.",
	"RES-002": "The `service.instance.id` resource attribute MUST be unique across logical resources, e.g., different Kubernetes pods.",
	"RES-003": "The `k8s.pod.uid` resource attribute MUST be present on resources associated with telemetry describing Kubernetes pods.",
	"RES-004": "Attribute keys specified in the semantic conventions MUST appear at the right level in OTLP.",
	"RES-005": "Resource attributes MUST contain a `service.name` key with a non-empty string value. The attribute MUST not be null, undefined, or an empty string.",
	"RES-006": "If the `service.criticality` resource attribute is present and its value, after trimming whitespace and ignoring case, equals one of the well-known values `critical`, `high`, `medium`, or `low`, the value MUST be exactly that lowercase well-known value. Custom values that do not match a well-known value pass this rule.",
	"RES-007": "Resource attributes MUST contain a `deployment.environment.name` key with a non-empty string value. The attribute MUST NOT be null, undefined, or an empty string.",
	"RES-010": "Resource attribute values MUST NOT contain sensitive data. Common formats include: - Email addresses (e.g., `user@example.com`) - Credit card numbers (13-19 digits, after removing spaces and hyphens, that pass Luhn validation) - Social security numbers or national ID numbers (e.g., `123-45-6789`) - User identifiers that can be tied back to a person, such as `enduser.id`, even when opaque or pseudonymous - Bearer tokens, API keys, or other credentials (e.g., `Bearer eyJ...`, `sk-...`, `AKIA...`) - Passwords or secrets in key-value patterns (e.g., `password=...`, `secret=...`, `token=...`, `trustStorePassword=...`) - Financial identifiers such as bank account numbers or tax IDs - Health data in key-value patterns (e.g., `diagnosis=...`, `patient_id=...`, `insurance_id=...`) The formats above are examples, not an exhaustive list, and this rule intentionally does not prescribe regular expressions. Detecting only the listed formats is not sufficient to pass: sensitive data in any other format, including formats introduced after this rule was written (such as the alphanumeric Brazilian CNPJ), also violates this rule. This rule applies to every string and integer value in resource attributes (integers are matched in their decimal form), including values nested in array or map values, such as the elements of `process.command_args`. Keys are evaluated together with their values: a non-empty value under a key naming a sensitive field, such as `password`, `secret`, `token`, `api_key`, or `ssn`, matches regardless of its format.",
	"SDK-001": "The versions of language and runtime found on the instrumentation dependencies for the SDK SHOULD be within supported values.",
	"SPA-001": "When grouping spans by trace identifier and `service.name`, no more than 10 spans in a single trace SHOULD have `span.kind = SpanKind.SPAN_KIND_INTERNAL`.",
	"SPA-002": "Given any span with a `parent_span_id` reference, here MUST exist a span with the same trace id whose `span_id` equals that `parent_span_id` value.",
	"SPA-003": "TODO",
	"SPA-004": "A root span MUST NOT have kind `CLIENT`.",
	"SPA-005": "When grouping spans by `trace_id`, each trace MUST NOT have more than 20 spans with a `duration` of less than 5 milliseconds.",
	"SPA-006": "Span data MUST NOT contain sensitive data. Common formats include: - Email addresses (e.g., `user@example.com`) - Credit card numbers (13-19 digits, after removing spaces and hyphens, that pass Luhn validation) - Social security numbers or national ID numbers (e.g., `123-45-6789`) - User identifiers that can be tied back to a person, such as `enduser.id`, even when opaque or pseudonymous - Bearer tokens, API keys, or other credentials (e.g., `Bearer eyJ...`, `sk-...`, `AKIA...`) - Passwords or secrets in key-value patterns (e.g., `password=...`, `secret=...`, `token=...`) - Financial identifiers such as bank account numbers or tax IDs - Health data in key-value patterns (e.g., `diagnosis=...`, `patient_id=...`, `insurance_id=...`) The formats above are examples, not an exhaustive list, and this rule intentionally does not prescribe regular expressions. Detecting only the listed formats is not sufficient to pass: sensitive data in any other format, including formats introduced after this rule was written (such as the alphanumeric Brazilian CNPJ), also violates this rule. This rule applies to every string and integer value in span data (integers are matched in their decimal form), including values nested in array or map values: - Span names and status messages - Span attributes - Span event names and attributes - Span link attributes - Instrumentation scope names, versions, and attributes Keys are evaluated together with their values: a non-empty value under a key naming a sensitive field, such as `password`, `secret`, `token`, `api_key`, or `ssn`, matches regardless of its format.",
}

// Description returns the vendored rule's Description text.
func Description(ruleID string) (string, bool) {
	d, ok := ruleDescriptions[ruleID]
	return d, ok
}

// FailedGuidance returns the vendored rule's Criteria — what MUST hold for the
// rule to pass — as the guidance shown alongside a failed verdict.
func FailedGuidance(ruleID string) (string, bool) {
	g, ok := ruleGuidance[ruleID]
	return g, ok
}

// normalizeVendoredText is the exact normalization applied when this file was
// generated (whitespace collapsed to single spaces, the vendored "\\=" escape
// unescaped). The rulesync test re-applies it to the vendored markdown before
// comparing, so the pinned copies here cannot drift from upstream silently.
func normalizeVendoredText(s string) string {
	return strings.ReplaceAll(strings.Join(strings.Fields(s), " "), "\\=", "=")
}
