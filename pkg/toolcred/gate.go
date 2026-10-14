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

package toolcred

import "sort"

// gate.go — the WS-9 S5 go-live gate oracle (design isi-5777 §4/§5, ISI-5818).
//
// A [sec]-gated Skill/MCPServer declares the credential type(s) it consumes via
// the AnnotationCredentialType value (SplitCredentialTypes parses it). The gate
// withholds that capability from a Run until its credential need resolves to a
// materialized Secret in the project namespace. This file is the PURE decision
// core — no Kubernetes, no controller, no clock — so the any-of + fail-closed
// semantics are exhaustively unit-testable in isolation; the run controller
// supplies the "which types are materialized" set (the oracle input) and acts
// on the verdict (admit vs Paused(credential_required)).
//
// Two properties are load-bearing:
//
//  1. ANY-OF, not all-of (ISI-5819 S6 refinement). A skill that declares a
//     comma-separated set (e.g. image-gen: "gemini,fal.ai") is admitted once
//     AT LEAST ONE declared type is materialized — it drives whichever provider
//     the project provisioned and degrades gracefully on the others. The design
//     §4 "every type resolves" wording is the single-type special case of this.
//
//  2. FAIL CLOSED. An unknown declared type can never be satisfied (the write
//     path refuses to materialize a Secret for a type the registry does not
//     map, binding.go), so it never contributes to admission. A skill whose
//     ONLY declared types are unknown is therefore withheld — a mis-typed
//     annotation fails safe (capability denied), never fails open.

// SkillGateVerdict is one gated capability's go-live decision. It is returned
// for every evaluated skill — Admitted true for an ungated or satisfied skill,
// false for a withheld one — so the caller can both act (withhold) and report
// (name what is missing) without re-deriving the decision.
type SkillGateVerdict struct {
	// Declared is the parsed, order-preserving declared type list (post-split,
	// pre-filter) for diagnostics — exactly what the annotation asked for.
	Declared []string
	// Gating is the subset of Declared that actually gates go-live: a hard
	// requirement the credential vault must satisfy. A known type with
	// RequiredByGate true, OR an unknown token (fail closed: an unknown need is
	// a hard, unsatisfiable requirement, never silently dropped). A known type
	// flagged RequiredByGate false is advisory and omitted — it never withholds.
	Gating []string
	// Admitted reports whether the skill may go live: true when it is ungated
	// (no gating type) or when at least one gating type is materialized (any-of).
	Admitted bool
	// Missing is the gating types still unsatisfied, sorted. Empty when Admitted.
	// It is what the Paused(credential_required) reason names so the operator
	// knows exactly which credential type(s) to add. For an any-of skill this is
	// the full gating set (admission needs only ONE of them), so the message
	// reads "add any of: gemini, fal.ai".
	Missing []string
}

// EvaluateSkillGate decides one skill's go-live admission from its raw
// AnnotationCredentialType value and the set of credential types materialized
// for the Run's project (satisfied[type] == true ⇔ a label-scoped Secret for
// that type exists in the project namespace). Pure and total: a nil/empty
// annotation is an ungated skill (Admitted, no gating types).
//
// The satisfied set is expected to contain only known types (materialization
// fails closed on unknowns, binding.go), so an unknown declared token is in
// Gating but can never appear in satisfied — withholding until a real
// annotation or a real credential appears.
func EvaluateSkillGate(annotation string, satisfied map[string]bool) SkillGateVerdict {
	declared := SplitCredentialTypes(annotation)
	v := SkillGateVerdict{Declared: declared}

	for _, t := range declared {
		b, known := LookupBinding(t)
		switch {
		case known && !b.RequiredByGate:
			// Advisory type — present in the registry but not a go-live gate.
			// Never withholds; not part of the any-of requirement.
			continue
		default:
			// Known+required, or unknown (fail closed): a hard gating need.
			v.Gating = append(v.Gating, t)
		}
	}

	if len(v.Gating) == 0 {
		// Ungated (no annotation, or only advisory types): admit.
		v.Admitted = true
		return v
	}

	var missing []string
	for _, t := range v.Gating {
		// Fail closed at the oracle layer too: only a KNOWN type can be
		// satisfied (the write path never materializes a Secret for an unknown
		// type), so an unknown gating token is never admitted even if a
		// malformed caller places it in the satisfied set.
		if IsKnownType(t) && satisfied[t] {
			v.Admitted = true // any-of: one satisfied gating type is enough.
		} else {
			missing = append(missing, t)
		}
	}
	if !v.Admitted {
		sort.Strings(missing)
		v.Missing = dedupeSorted(missing)
	}
	return v
}

// GatedSkill is one capability the gate evaluates: its identity (for the
// per-skill verdict and actionable errors) and the raw credential-type
// annotation it carries. SourceName is "namespace/name" of the Skill/MCPServer.
type GatedSkill struct {
	SourceName string
	Annotation string
}

// RunGateDecision is the whole-Run go-live verdict: admit the Run or withhold it
// with the concise, deterministic set of credential types the operator must
// provide. It is the single value the controller turns into either a normal
// advance or a Paused(credential_required) hold whose message names MissingTypes.
type RunGateDecision struct {
	// Admitted is true iff EVERY gated skill in the Run is admitted. One
	// withheld skill withholds the Run (a Run runs all its granted skills; a
	// capability it cannot satisfy must not silently drop — fail closed).
	Admitted bool
	// WithheldSkills names each withheld skill (namespace/name) in input order.
	WithheldSkills []string
	// MissingTypes is the deduped, sorted union of every withheld skill's
	// unsatisfied gating types — the operator-facing "credential required: …"
	// list. Empty when Admitted.
	MissingTypes []string
}

// EvaluateRunGate folds the per-skill gate over all of a Run's gated skills
// against the project's materialized-type set. The Run is admitted only when
// every gated skill is admitted (fail closed: a Run must not go live missing a
// credential any of its granted capabilities needs); MissingTypes aggregates
// what to add.
func EvaluateRunGate(skills []GatedSkill, satisfied map[string]bool) RunGateDecision {
	d := RunGateDecision{Admitted: true}
	var missing []string
	for _, s := range skills {
		v := EvaluateSkillGate(s.Annotation, satisfied)
		if v.Admitted {
			continue
		}
		d.Admitted = false
		d.WithheldSkills = append(d.WithheldSkills, s.SourceName)
		missing = append(missing, v.Missing...)
	}
	if !d.Admitted {
		sort.Strings(missing)
		d.MissingTypes = dedupeSorted(missing)
	}
	return d
}

// ProjectSatisfiedTypes builds the oracle's satisfied set for a Run's project
// (the EvaluateRunGate input), fail-closed and PROJECT-SCOPED by the credential
// Secret NAME — never a namespace label-list, which would over-admit across the
// sibling projects that share a team namespace (ISI-5849). For each candidate
// credential type it computes the project-encoded materialized Secret name
// (MaterializedSecretName) and marks the type satisfied iff a Secret of EXACTLY
// that name is present (present(name) == true). An unknown type can never be
// satisfied (MaterializedSecretName returns ok=false, so it is skipped) — the
// same fail-closed property the gate relies on, enforced at the input boundary
// so a malformed caller cannot seed an unknown type into the satisfied set.
//
// candidateTypes is typically the deduped union of the Run's gated skills'
// declared types (SplitCredentialTypes over each GatedSkill.Annotation);
// present is the live membership probe the controller supplies (a Secret Get /
// cache lookup scoped to the project namespace). Pure given present: the whole
// project-scoping decision is unit-testable with a map-backed present.
func ProjectSatisfiedTypes(project string, candidateTypes []string, present func(secretName string) bool) map[string]bool {
	satisfied := map[string]bool{}
	if present == nil {
		return satisfied
	}
	for _, t := range candidateTypes {
		name, ok := MaterializedSecretName(project, t)
		if !ok {
			continue // unknown type: never materializable, fail closed
		}
		if present(name) {
			satisfied[t] = true
		}
	}
	return satisfied
}

// dedupeSorted removes adjacent duplicates from an already-sorted slice,
// returning nil for an empty input so an admitted decision carries no list.
func dedupeSorted(sorted []string) []string {
	if len(sorted) == 0 {
		return nil
	}
	out := sorted[:1]
	for _, s := range sorted[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}
