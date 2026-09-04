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

// Package observability holds the authoritative, machine-checkable cardinality
// contract for the platform's metrics (obs-plan §5.6, story 13.6).
//
// This file is the single source of truth for which label keys a Prometheus
// *Vec metric is allowed to declare. The CI cardinality gate
// (scripts/ci/obs-cardinality-gate.sh) parses MetricLabelAllowlist out of this
// file and fails the build if any metric label key in pkg/, internal/, or cmd/
// falls outside it — so label discipline is tested, not hoped for.
//
// The law (obs-plan §1.2/§5.6): bounded enums are fine as metric labels;
// unbounded identifiers (run/work-item/principal IDs, trace IDs, pod names) are
// NEVER metric labels — they ride as span/log/exemplar attributes or as resource
// attributes (which Prometheus federates without per-series explosion).
package observability

// MetricLabelAllowlist is the bounded-cardinality label-key allowlist. A
// Prometheus *Vec metric may declare a label key ONLY if it appears here.
//
// Sources, kept explicit so a reviewer can trace every entry to a decision:
//   - obs-plan §5.6 — the original curated bounded-enum domains.
//   - story 13.9 (CEO 2026-08-12) — rate-limit/fallback dims are bounded by
//     construction (finite squads/agents/roles/providers/models).
//   - Epic D tool-usage (plan §2.4) — tool/skill/server are bounded registries.
//   - story 13.10 — auth/RBAC event dims (bounded outcome/action enums; the
//     two-valued user.role). user.id itself stays an exemplar (see forbidden).
//
// Adding a key here is a deliberate cardinality decision: it must be a bounded
// enum or a finite registry, never a free-text or per-entity identifier.
var MetricLabelAllowlist = []string{
	// obs-plan §5.6 — curated bounded-enum domains.
	"outcome", "terminal_reason", "phase", "from", "to", "runtime", "runtime_class",
	"operation", "result", "state", "kind", "trigger", "reason", "decision", "surface",
	"capability", "check", "direction", "pool_hit", "cause", "error_code",
	"provenance_class", "signal",
	// story 13.9 (CEO 2026-08-12) — rate-limit / fallback metric dims.
	"project", "agent", "role", "provider", "model", "primary_model", "fallback_model",
	// Epic D tool-usage (plan §2.4) — bounded tool/skill/server registries.
	"tool", "skill", "server",
	// story 13.10 — auth + RBAC bounded event dims (user.id stays an exemplar).
	"event_type", "resource_type", "action", "user_role",
}

// MetricLabelForbidden is the hard denylist: unbounded identifiers that must ride
// as resource attributes / exemplars / span attributes and NEVER as metric labels
// (obs-plan §1.2/§5.6). Membership in the allowlist gate is decided by
// MetricLabelAllowlist alone; this list exists so the gate can emit a louder,
// specific diagnostic when it catches one of the classic offenders. Every entry
// here is (and must stay) absent from MetricLabelAllowlist. Both dotted and
// snake_case spellings are listed because instrumentation may use either.
var MetricLabelForbidden = []string{
	"run_id", "run.id",
	"work_item_id", "work_item.id",
	"principal_id", "principal.id",
	"sandbox_pod", "sandbox.pod",
	"trace_id",
	"user_id", "user.id",
}
