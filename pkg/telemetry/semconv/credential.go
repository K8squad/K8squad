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

package ksqsemconv

// ============================================================================
// WS-9 S5 go-live gate audit telemetry (ISI-5850, Design §6 threat model T7).
//
// The go-live gate (ISI-5818 oracle `toolcred.EvaluateRunGate` → PR #902)
// withholds a [sec]-gated skill from a Run until its declared credential
// type(s) resolve to a materialized Secret. T7 (audit trail) requires a
// legible, operator-facing record of EVERY admission decision: why a Run was
// withheld, which credential types it is paused for, and when it cleared —
// WITHOUT ever recording a credential value.
//
// This span is NOT part of the run-trace vocabulary in SpanConventions (which
// is driven by toolusage.Mapper): it is emitted by the DRIVER pre-dispatch
// admission point (pkg/controller/rundrive) that ISI-5849 wires in, sibling to
// the apiserver credential funnel (ksquad.credential.create / .rotate /
// .delete, internal/apiserver/obsfunnel.go). So it lives in its own family
// here, with its own conformance suite (credential_conformance_test.go),
// exactly like the SCM family in scm.go.
//
// STABILITY. Every entry is Planned / WS-9 today: the admission point that
// emits them lands in ISI-5849 (driver pre-dispatch gate wiring), which this
// contract is written against. The conformance suite documents and
// regression-guards the shape now; it begins enforcing emission the moment
// ISI-5849 flips the family to Stable (same discipline as the WS-A/WS-B/WS-GH
// planned conventions).
//
// PII / SECRET POSTURE (NFR-2, mirrored from obsfunnel.go). A credential VALUE
// never travels on any attribute, label, or log field of this span. The audit
// key is principal + credential TYPE + namespace — all identities, never the
// secret material. Credential types are the bounded S1 binding vocabulary
// (pkg/toolcred), so `credential_type` is a safe metric label; run/project/
// work-item ids are high-cardinality and ride the SPAN only, never a label.
// ============================================================================

// SpanCredentialGateCheck is the go-live gate admission-decision span, emitted
// once per Run admission at the driver pre-dispatch gate (ISI-5849). Named in
// the ksquad.credential.* family so it reads alongside the apiserver funnel's
// ksquad.credential.create.
const SpanCredentialGateCheck = "ksquad.credential.gate_check" // #nosec G101 -- a telemetry span NAME, not a credential value (NFR-2: no secret material ever rides this family).

// CredentialSpanConventions documents the WS-9 go-live gate span family. These
// spans do not exist on the wire yet (Stability Planned); the emitter lands in
// ISI-5849 and this registry is the contract it lands against.
var CredentialSpanConventions = []SpanConvention{
	{
		Name:  SpanCredentialGateCheck,
		Brief: "The go-live credential gate admission decision for one Run, recorded at the driver pre-dispatch point (ISI-5849) off run.reconcile. Carries the T7 audit trail — principal, declared gated skills + their declared credential TYPES, the admit/withhold outcome, and (when withheld) the missing types the Run is paused for. A credential VALUE never travels here (NFR-2); the decision shape is toolcred.RunGateDecision (ISI-5818, PR #902). A companion structured audit log (docs/observability/credential-gate-check-span-spec.md) carries the same keyset for log-based audit.",
		Attributes: []Attribute{
			{Key: "ksquad.run.id", Type: TypeString, Requirement: Required, Stability: Planned, Workstream: "WS-9",
				Brief: "The Run being admitted (Run CR UID). High-cardinality — span attribute only, never a metric label."},
			{Key: "ksquad.agent.name", Type: TypeString, Requirement: Recommended, Stability: Planned, Workstream: "WS-9",
				Brief: "The principal the Run executes as (the agent/seat identity, T7 \"principal\"). An identity, never a secret."},
			{Key: "ksquad.project.name", Type: TypeString, Requirement: Required, Stability: Planned, Workstream: "WS-9",
				Brief: "The Run's project (run.Spec.ProjectRef). This IS the credential scope: the gate resolves materialized Secrets by the project-encoded name proj-<project>-cred-<type> (toolcred.MaterializedSecretName), NOT a namespace label-list, so sibling projects sharing the team namespace cannot over-admit (ISI-5849 project-scoping hazard)."},
			{Key: "ksquad.work_item.ref", Type: TypeString, Requirement: Recommended, Stability: Planned, Workstream: "WS-9",
				Brief: "The work item / ticket the withheld-or-admitted Run is servicing (ticket→run audit drill-down)."},
			{Key: "k8s.namespace.name", Type: TypeString, Requirement: Required, Stability: Planned, Workstream: "WS-9",
				Brief: "The team namespace the Run and its materialized credential Secrets live in (T7 \"namespace\"). Standard OTel resource semconv key."},
			{Key: "ksquad.credential.gated_skills", Type: TypeStringSlice, Requirement: Recommended, Stability: Planned, Workstream: "WS-9",
				Brief: "The gated skills the gate evaluated, each as its SourceName (\"namespace/name\"). Bounded by the Run's skill set. Skill names only — never credential material."},
			{Key: "ksquad.credential.declared_types", Type: TypeStringSlice, Requirement: Recommended, Stability: Planned, Workstream: "WS-9",
				Brief: "The deduped union of every gated skill's declared credential TYPE(s) from the S1 binding vocabulary (an any-of comma-list declares several). Type NAMES only — never values."},
			{Key: "ksquad.credential.admitted", Type: TypeBool, Requirement: Required, Stability: Planned, Workstream: "WS-9",
				Brief: "toolcred.RunGateDecision.Admitted — true iff EVERY gated skill in the Run is admitted (each gating type any-of-satisfied by a materialized Secret). The Run advances; false pauses it (credential_required)."},
			{Key: "ksquad.credential.outcome", Type: TypeString, Requirement: Required, Stability: Planned, Workstream: "WS-9",
				Brief: "Bounded admission outcome: \"admitted\" | \"withheld\". The single metric label on ksquad.credential.gate_check.decisions."},
			{Key: "ksquad.credential.withheld_skills", Type: TypeStringSlice, Requirement: Conditional, Stability: Planned, Workstream: "WS-9",
				Brief: "Present only when withheld: toolcred.RunGateDecision.WithheldSkills — each withheld skill's SourceName, in input order. Empty/absent when admitted."},
			{Key: "ksquad.credential.missing_types", Type: TypeStringSlice, Requirement: Conditional, Stability: Planned, Workstream: "WS-9",
				Brief: "Present only when withheld: toolcred.RunGateDecision.MissingTypes — the deduped, sorted union of credential TYPES the Run is paused for. This is what the Paused(credential_required) operator message names. Type names only — never values."},
		},
	},
}

// CredentialMetricConventions documents the WS-9 gate metrics. Labels are
// strictly bounded: `outcome` is a two-value enum and `credential_type` is the
// closed S1 binding vocabulary (pkg/toolcred) — run/project/work-item ids are
// NEVER metric labels (cardinality posture, mirrored from the SCM family).
var CredentialMetricConventions = []MetricConvention{
	{
		Name: "ksquad.credential.gate_check.decisions", Instrument: InstrumentCounter, Unit: "1",
		Labels: []string{"outcome"}, Stability: Planned, Workstream: "WS-9",
		Brief: "Go-live gate admission decisions, one per Run admission, dimensioned by bounded outcome (admitted|withheld). The RED numerator for \"what fraction of Runs are being withheld on credentials\" and the signal that a withheld Run cleared.",
	},
	{
		Name: "ksquad.credential.gate_check.withheld_by_type", Instrument: InstrumentCounter, Unit: "1",
		Labels: []string{"credential_type"}, Stability: Planned, Workstream: "WS-9",
		Brief: "Per-credential-type withhold count: a Run withheld for N missing types increments each type once. credential_type is the bounded S1 binding vocabulary, so this is cardinality-safe and answers \"which credential type blocks the most Runs\" without any high-cardinality id.",
	},
}

// DocumentedCredentialMetricNames returns every credential-gate metric name the
// registry documents (Stable or Planned) — the no-drift guard the conformance
// suite uses once the emitter lands.
func DocumentedCredentialMetricNames() map[string]bool {
	out := map[string]bool{}
	for _, m := range CredentialMetricConventions {
		out[m.Name] = true
	}
	return out
}

// StableCredentialMetricNames returns the credential-gate metric names the
// registry marks Stable (emission-enforced). Empty until ISI-5849 flips the
// family to Stable.
func StableCredentialMetricNames() []string {
	var out []string
	for _, m := range CredentialMetricConventions {
		if m.Stability == Stable {
			out = append(out, m.Name)
		}
	}
	return out
}
