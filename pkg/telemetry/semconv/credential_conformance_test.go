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

// Credential go-live gate semconv conformance — ISI-5850 / WS-9 S5, Design §6
// threat model T7. The gate span + metrics are emitted by the driver
// pre-dispatch admission point (ISI-5849), which is not landed yet, so the
// whole family is Stability Planned. Driving the real emitter here would pull
// the controller-runtime driver into the contract package, so — exactly like
// the SCM span suite — these conventions are validated for self-consistency and
// for the PII/cardinality posture T7 demands. The suite starts enforcing
// emission (registry ⊆ emitter) the moment ISI-5849 flips the family to Stable:
// StableCredentialMetricNames() goes non-empty and a real-emitter assertion is
// added alongside, mirroring TestSCMConformance_StableMetricsEmitted.
package ksqsemconv_test

import (
	"strings"
	"testing"

	ksqsemconv "github.com/K8squad/K8squad/pkg/telemetry/semconv"
)

// credValueKey reports whether an attribute/label key looks like it could carry
// credential material. The go-live gate audit keys on principal + TYPE +
// namespace and must NEVER carry a value (NFR-2, T7).
func credValueKey(key string) bool {
	k := strings.ToLower(key)
	for _, bad := range []string{"value", "secret", "token", "password", "material", "payload"} {
		if strings.Contains(k, bad) {
			return true
		}
	}
	return false
}

// TestCredentialConformance_SpansWellFormed validates the gate span conventions
// are self-consistent: unique names in the ksquad.credential.* family, at least
// one attribute each, and every attribute a real type/requirement/stability in
// the ksquad.*/k8s.* namespace — and never a value-bearing key (T7 / NFR-2).
func TestCredentialConformance_SpansWellFormed(t *testing.T) {
	validType := map[ksqsemconv.AttrType]bool{
		ksqsemconv.TypeString: true, ksqsemconv.TypeInt: true, ksqsemconv.TypeDouble: true,
		ksqsemconv.TypeBool: true, ksqsemconv.TypeStringSlice: true,
	}
	validReq := map[ksqsemconv.Requirement]bool{
		ksqsemconv.Required: true, ksqsemconv.Conditional: true, ksqsemconv.Recommended: true,
	}
	seen := map[string]bool{}
	for _, sc := range ksqsemconv.CredentialSpanConventions {
		if seen[sc.Name] {
			t.Errorf("duplicate credential span convention %q", sc.Name)
		}
		seen[sc.Name] = true
		if !strings.HasPrefix(sc.Name, "ksquad.credential.") {
			t.Errorf("credential span %q is not in the ksquad.credential.* family", sc.Name)
		}
		if len(sc.Attributes) == 0 {
			t.Errorf("credential span %q documents no attributes", sc.Name)
		}
		attrSeen := map[string]bool{}
		for _, a := range sc.Attributes {
			if attrSeen[a.Key] {
				t.Errorf("credential span %q lists attribute %q twice", sc.Name, a.Key)
			}
			attrSeen[a.Key] = true
			if !strings.HasPrefix(a.Key, "ksquad.") && !strings.HasPrefix(a.Key, "k8s.") {
				t.Errorf("credential span %q attribute %q is not in the ksquad.*/k8s.* namespace", sc.Name, a.Key)
			}
			if credValueKey(a.Key) {
				t.Errorf("credential span %q attribute %q looks value-bearing — T7/NFR-2 forbids credential material in telemetry", sc.Name, a.Key)
			}
			if !validType[a.Type] {
				t.Errorf("credential span %q attribute %q has invalid type %q", sc.Name, a.Key, a.Type)
			}
			if !validReq[a.Requirement] {
				t.Errorf("credential span %q attribute %q has invalid requirement %q", sc.Name, a.Key, a.Requirement)
			}
			if a.Stability != ksqsemconv.Stable && a.Stability != ksqsemconv.Planned {
				t.Errorf("credential span %q attribute %q has invalid stability %q", sc.Name, a.Key, a.Stability)
			}
		}
	}
}

// TestCredentialConformance_GateCheckHasOutcomeDecision asserts the gate_check
// span carries the two load-bearing audit attributes T7 requires: the boolean
// decision and the bounded outcome that the metric is dimensioned by.
func TestCredentialConformance_GateCheckHasOutcomeDecision(t *testing.T) {
	var gate *ksqsemconv.SpanConvention
	for i := range ksqsemconv.CredentialSpanConventions {
		if ksqsemconv.CredentialSpanConventions[i].Name == ksqsemconv.SpanCredentialGateCheck {
			gate = &ksqsemconv.CredentialSpanConventions[i]
		}
	}
	if gate == nil {
		t.Fatalf("registry is missing the %q span", ksqsemconv.SpanCredentialGateCheck)
	}
	want := map[string]bool{
		"ksquad.credential.admitted": false,
		"ksquad.credential.outcome":  false,
	}
	for _, a := range gate.Attributes {
		if _, ok := want[a.Key]; ok {
			if a.Requirement != ksqsemconv.Required {
				t.Errorf("gate_check attribute %q must be Required (it is the audit decision), got %q", a.Key, a.Requirement)
			}
			want[a.Key] = true
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("gate_check span is missing required audit attribute %q", k)
		}
	}
}

// TestCredentialConformance_MetricLabelsBounded is the cardinality guard: the
// gate metrics may be dimensioned ONLY by bounded enums (outcome,
// credential_type) — never a run/project/work-item id. It also forbids a
// value-bearing label outright (T7 / NFR-2).
func TestCredentialConformance_MetricLabelsBounded(t *testing.T) {
	allowed := map[string]bool{"outcome": true, "credential_type": true}
	seen := map[string]bool{}
	for _, m := range ksqsemconv.CredentialMetricConventions {
		if seen[m.Name] {
			t.Errorf("duplicate credential metric convention %q", m.Name)
		}
		seen[m.Name] = true
		if !strings.HasPrefix(m.Name, "ksquad.credential.gate_check.") {
			t.Errorf("credential metric %q is not in the ksquad.credential.gate_check.* family", m.Name)
		}
		for _, l := range m.Labels {
			if credValueKey(l) {
				t.Errorf("credential metric %q carries value-bearing label %q — T7/NFR-2 forbids it", m.Name, l)
			}
			if !allowed[l] {
				t.Errorf("credential metric %q label %q is not a bounded enum (allowed: outcome, credential_type) — a high-cardinality id must never be a label", m.Name, l)
			}
		}
	}
}

// TestCredentialConformance_StableMetricsEmitted is the forward guard. While the
// family is Planned (ISI-5849 unlanded) StableCredentialMetricNames() is empty
// and this is a no-op; when ISI-5849 flips the family to Stable it becomes a
// real registry⊆emitter assertion (a driver-emitter fixture is added here then,
// mirroring TestSCMConformance_StableMetricsEmitted).
func TestCredentialConformance_StableMetricsEmitted(t *testing.T) {
	if names := ksqsemconv.StableCredentialMetricNames(); len(names) != 0 {
		t.Fatalf("credential-gate family has Stable metrics %v but no real-emitter fixture is wired yet — add a driver-emitter assertion here (see suite header)", names)
	}
}
