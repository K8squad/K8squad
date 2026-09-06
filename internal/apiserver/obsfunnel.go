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

package apiserver

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/K8squad/K8squad/pkg/telemetry"
)

// ============================================================================
// Onboarding/compose activation-funnel observability (ISI-3669, obs plan §3/§4/§5).
// The M1–M6 funnel instruments for the three landed endpoints:
//
//	GET  /api/onboarding/progress   → span ksquad.onboarding.progress
//	POST /api/compose/squad         → span ksquad.compose.squad
//	POST /api/credentials           → span ksquad.credential.create
//
// (http.server.request.duration + the HTTP server spans come from the otelhttp
// wrapper installed in Server.Handler() — ISI-3668 — and need nothing here.)
//
// The laws this file obeys (mirroring internal/observability/cardinality_allowlist.go):
//   - metric label keys stay inside MetricLabelAllowlist: the funnel uses
//     exactly ONE label, "outcome", a closed per-endpoint enum defined below;
//   - unbounded identifiers (team UID, secret name, principal, credential
//     material) are NEVER metric labels. team.id rides as a SPAN attribute
//     only; secret material rides NOWHERE (NFR-2, §5.3 — enforced by
//     TestNFR2SecretNeverInTelemetry over captured span AND log output);
//   - instruments are created lazily on first handler use, so they bind to the
//     MeterProvider telemetry.Setup installed — a package-init instrument would
//     bind to the pre-Setup no-op meter forever (see telemetry.Meter docs).
// ============================================================================

// Bounded outcome enums — the single metric label "outcome".
const (
	outcomeUnauthenticated = "unauthenticated" // 401 at the choke point

	// GET /api/onboarding/progress
	outcomeProgressOK    = "ok"         // 200 — projection served
	outcomeProgressError = "read_error" // 502 — read model unavailable

	// POST /api/compose/squad
	outcomeSquadInvalid     = "invalid"      // 400/422 — body/template/fields
	outcomeSquadNoNamespace = "no_namespace" // 404 — team scope unresolved
	outcomeSquadCreated     = "created"      // 201 — full materialize
	outcomeSquadPartial     = "partial"      // 207 — verbatim per-object errors

	// POST /api/credentials
	outcomeCredInvalid     = "invalid"           // 400/422 — decode/validation
	outcomeCredUnsupported = "unsupported_class" // 501 — human-seat OAuth class
	outcomeCredNoNamespace = "no_namespace"      // 404 — team scope unresolved
	outcomeCredConflict    = "conflict"          // 409 — name already exists
	outcomeCredRejected    = "rejected"          // RBAC/admission forbade the write
	outcomeCredError       = "store_error"       // 502 — store unavailable
	outcomeCredCreated     = "created"           // 201
)

// funnel holds the activation-funnel instruments (obs plan §4). Created once,
// lazily, on the first instrumented request — after telemetry.Setup ran.
type funnel struct {
	onboardingProgress metric.Int64Counter   // ksquad.onboarding.progress.requests{outcome}
	onboardingDone     metric.Int64Histogram // ksquad.onboarding.progress.milestones_done
	composeSquad       metric.Int64Counter   // ksquad.compose.squad.requests{outcome}
	composeAgents      metric.Int64Histogram // ksquad.compose.squad.agents_applied
	credentialCreate   metric.Int64Counter   // ksquad.credential.create.requests{outcome}
}

var (
	funnelOnce    sync.Once
	funnelOnceVal *funnel
)

// funnelInst returns the process-wide funnel instruments, creating them on
// first use (see the file-header law about lazy creation).
func funnelInst() *funnel {
	funnelOnce.Do(func() {
		m := telemetry.Meter()
		f := &funnel{}
		f.onboardingProgress, _ = m.Int64Counter("ksquad.onboarding.progress.requests",
			metric.WithUnit("{request}"),
			metric.WithDescription("Onboarding-progress reads by bounded outcome (ISI-3669, obs plan §4 / M1 funnel)."))
		f.onboardingDone, _ = m.Int64Histogram("ksquad.onboarding.progress.milestones_done",
			metric.WithUnit("1"),
			metric.WithDescription("Distribution of completed onboarding milestones (0–4) per read (ISI-3669, obs plan §4)."))
		f.composeSquad, _ = m.Int64Counter("ksquad.compose.squad.requests",
			metric.WithUnit("{request}"),
			metric.WithDescription("Squad-materialize calls by bounded outcome (ISI-3669, obs plan §4 / M2 funnel)."))
		f.composeAgents, _ = m.Int64Histogram("ksquad.compose.squad.agents_applied",
			metric.WithUnit("{agent}"),
			metric.WithDescription("Agents successfully applied per squad-materialize call (ISI-3669, obs plan §4)."))
		f.credentialCreate, _ = m.Int64Counter("ksquad.credential.create.requests",
			metric.WithUnit("{request}"),
			metric.WithDescription("Managed-credential creates by bounded outcome (ISI-3669, obs plan §4 / M3 funnel)."))
		funnelOnceVal = f
	})
	return funnelOnceVal
}

// funnelSpan starts a funnel domain span off the request context. The span
// name is the low-cardinality domain operation, not the HTTP route.
func funnelSpan(ctx context.Context, name string) (context.Context, trace.Span) {
	return telemetry.Tracer().Start(ctx, name)
}

// funnelAttrs stamps bounded attributes onto the funnel span.
func funnelAttrs(span trace.Span, attrs ...attribute.KeyValue) {
	span.SetAttributes(attrs...)
}

// funnelOutcome records the terminal outcome on the span and the request
// counter in one place, so no return path can forget the funnel record. Call
// it exactly once per request, immediately before the handler returns.
func funnelOutcome(ctx context.Context, span trace.Span, counter metric.Int64Counter, outcome string) {
	span.SetAttributes(attribute.String("outcome", outcome))
	counter.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}
