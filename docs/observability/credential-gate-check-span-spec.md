# Credential go-live gate audit span — operational spec (ISI-5850, WS-9 S5, T7)

> **Status:** contract landed, emitter **Planned** (lands in ISI-5849).
> **Source of truth for the shape:** `pkg/telemetry/semconv/credential.go`
> (`CredentialSpanConventions` / `CredentialMetricConventions`), rendered into
> `docs/observability/semantic-conventions.md` and regression-guarded by
> `pkg/telemetry/semconv/credential_conformance_test.go`. This doc is the
> **operational seam**: where the span is emitted, the drop-in emitter helper,
> the companion structured audit log, and the ISI-5849 integration checklist.

## 1. Why — Design §6 threat model T7 (audit trail)

The WS-9 S5 go-live gate (ISI-5818 oracle `toolcred.EvaluateRunGate`, PR #902)
withholds a `[sec]`-gated Skill/MCPServer from a Run until at least one of its
declared credential type(s) resolves to a materialized Secret in the project
(any-of + fail-closed — see `pkg/toolcred/gate.go`). T7 requires that **every
admission decision is legible**: an operator must be able to answer, after the
fact and in real time,

- *why* a Run was withheld — which gated skills, which declared types,
- *which* credential type(s) the Run is paused for (so they know what to add),
- *when* a withheld Run cleared (the admitted decision that followed),

**without a credential value ever appearing in telemetry.** The gate decision
is computed over credential **type names** and a `satisfied map[string]bool` —
no secret material is in scope at the decision point — so the audit is
value-free *by construction*, not merely by redaction.

This span is the sibling of the apiserver credential funnel
(`ksquad.credential.create` / `.rotate` / `.delete`,
`internal/apiserver/obsfunnel.go`): same `ksquad.credential.*` family, same
NFR-2 secret posture, same "bounded enum is the only metric label" cardinality
firewall. It differs in **where** it fires: the funnel instruments HTTP write
choke points in the apiserver; the gate check fires at the **driver
pre-dispatch admission point** in the run controller.

## 2. Span contract (summary — registry is authoritative)

Span name: **`ksquad.credential.gate_check`**, one per Run admission,
child of `run.reconcile` (reuse the reconcile `ctx`, `pkg/controller/rundrive/driver.go`).

| Attribute | Type | Req | Notes |
|---|---|---|---|
| `ksquad.run.id` | string | Required | Run CR UID. High-cardinality → span only. |
| `ksquad.agent.name` | string | Recommended | Principal (seat identity), never a secret. |
| `ksquad.project.name` | string | Required | The credential **scope** (`proj-<project>-cred-<type>`). |
| `ksquad.work_item.ref` | string | Recommended | Ticket→run audit drill-down. |
| `k8s.namespace.name` | string | Required | Team namespace. |
| `ksquad.credential.gated_skills` | string[] | Recommended | Evaluated skills, each `namespace/name`. |
| `ksquad.credential.declared_types` | string[] | Recommended | Deduped union of declared **type names**. |
| `ksquad.credential.admitted` | bool | Required | `RunGateDecision.Admitted`. |
| `ksquad.credential.outcome` | string | Required | `admitted` \| `withheld` (the metric label). |
| `ksquad.credential.withheld_skills` | string[] | Conditional | `RunGateDecision.WithheldSkills` — withheld only. |
| `ksquad.credential.missing_types` | string[] | Conditional | `RunGateDecision.MissingTypes` — withheld only. |

Metrics (on `telemetry.Meter()`, bounded labels only):

| Metric | Instrument | Label(s) |
|---|---|---|
| `ksquad.credential.gate_check.decisions` | counter | `outcome` ∈ {admitted, withheld} |
| `ksquad.credential.gate_check.withheld_by_type` | counter | `credential_type` (closed S1 vocab) |

## 3. Structured audit log (same keyset, log-based audit)

Emitted on the same decision, best-effort, after the gate verdict is computed
and before the admit/pause side-effect — so a telemetry failure never changes
the admission outcome. One `slog.InfoContext` record, keys mirroring the span:

```go
slog.InfoContext(ctx, "rundrive: credential go-live gate check",
    "run.id", rec.RunID,
    "agent.name", rec.AgentName,
    "project", rec.Project,
    "work_item.ref", rec.WorkItemRef,
    "namespace", rec.Namespace,
    "gated_skills", rec.GatedSkills,       // []string, names only
    "declared_types", rec.DeclaredTypes,   // []string, type names only
    "admitted", rec.Admitted,
    "outcome", rec.outcome(),              // "admitted" | "withheld"
    "withheld_skills", rec.WithheldSkills, // nil when admitted
    "missing_types", rec.MissingTypes,     // nil when admitted
)
```

No field carries a credential value; every field is an identity or a type name.

## 4. Drop-in emitter helper (ISI-5849 pastes this + one call)

The helper is a **pure value-in / telemetry-out** function so it can be unit
tested against an in-memory exporter without the controller-runtime driver, and
so it carries no dependency on the oracle package (the caller maps
`toolcred.RunGateDecision` → the primitive record). Suggested home:
`pkg/coord/gatecheck_obs.go` (coord is imported by `rundrive`, so the helper is
callable from either the driver pre-dispatch point **or** `coord.ApplyGateHold`
with no import cycle).

```go
package coord

import (
	"context"
	"log/slog"
	"sync"

	"github.com/K8squad/K8squad/pkg/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// GateCheckRecord is the value-free audit input for one Run's go-live
// credential-gate admission decision (ISI-5850 / T7). Every field is an
// identity or a credential TYPE name — a credential value never appears.
type GateCheckRecord struct {
	RunID         string
	AgentName     string
	Project       string
	WorkItemRef   string
	Namespace     string
	GatedSkills   []string // SourceNames ("namespace/name") evaluated by the gate
	DeclaredTypes []string // deduped union of declared credential TYPE names
	Admitted      bool     // toolcred.RunGateDecision.Admitted
	WithheldSkills []string // toolcred.RunGateDecision.WithheldSkills (nil when admitted)
	MissingTypes  []string  // toolcred.RunGateDecision.MissingTypes (nil when admitted)
}

func (r GateCheckRecord) outcome() string {
	if r.Admitted {
		return "admitted"
	}
	return "withheld"
}

var (
	gateCheckOnce      sync.Once
	gateCheckDecisions metric.Int64Counter
	gateCheckWithheld  metric.Int64Counter
)

func gateCheckInst() (metric.Int64Counter, metric.Int64Counter) {
	gateCheckOnce.Do(func() {
		m := telemetry.Meter()
		gateCheckDecisions, _ = m.Int64Counter("ksquad.credential.gate_check.decisions",
			metric.WithUnit("1"),
			metric.WithDescription("Go-live gate admission decisions by bounded outcome (ISI-5850, T7)."))
		gateCheckWithheld, _ = m.Int64Counter("ksquad.credential.gate_check.withheld_by_type",
			metric.WithUnit("1"),
			metric.WithDescription("Per-credential-type withhold count (ISI-5850, T7)."))
	})
	return gateCheckDecisions, gateCheckWithheld
}

// EmitCredentialGateCheck records the T7 audit span, bounded metrics and
// structured log for one admission decision. Best-effort and side-effect-free
// on the admission outcome: call it once, after the verdict is computed.
func EmitCredentialGateCheck(ctx context.Context, rec GateCheckRecord) {
	outcome := rec.outcome()

	_, span := telemetry.Tracer().Start(ctx, "ksquad.credential.gate_check")
	defer span.End()
	span.SetAttributes(
		attribute.String("ksquad.run.id", rec.RunID),
		attribute.String("ksquad.agent.name", rec.AgentName),
		attribute.String("ksquad.project.name", rec.Project),
		attribute.String("ksquad.work_item.ref", rec.WorkItemRef),
		attribute.String("k8s.namespace.name", rec.Namespace),
		attribute.StringSlice("ksquad.credential.gated_skills", rec.GatedSkills),
		attribute.StringSlice("ksquad.credential.declared_types", rec.DeclaredTypes),
		attribute.Bool("ksquad.credential.admitted", rec.Admitted),
		attribute.String("ksquad.credential.outcome", outcome),
	)
	if !rec.Admitted {
		span.SetAttributes(
			attribute.StringSlice("ksquad.credential.withheld_skills", rec.WithheldSkills),
			attribute.StringSlice("ksquad.credential.missing_types", rec.MissingTypes),
		)
	}

	decisions, withheld := gateCheckInst()
	decisions.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	if !rec.Admitted {
		for _, t := range rec.MissingTypes { // bounded: S1 binding vocabulary
			withheld.Add(ctx, 1, metric.WithAttributes(attribute.String("credential_type", t)))
		}
	}

	slog.InfoContext(ctx, "rundrive: credential go-live gate check",
		"run.id", rec.RunID, "agent.name", rec.AgentName, "project", rec.Project,
		"work_item.ref", rec.WorkItemRef, "namespace", rec.Namespace,
		"gated_skills", rec.GatedSkills, "declared_types", rec.DeclaredTypes,
		"admitted", rec.Admitted, "outcome", outcome,
		"withheld_skills", rec.WithheldSkills, "missing_types", rec.MissingTypes)
}
```

### The one call site (ISI-5849, driver pre-dispatch)

At the admission point where the driver folds the gate over the Run's gated
skills and turns `RunGateDecision` into either an advance or
`Paused(credential_required)`:

```go
dec := toolcred.EvaluateRunGate(gatedSkills, satisfied)
coord.EmitCredentialGateCheck(ctx, coord.GateCheckRecord{
    RunID:          runID,
    AgentName:      agentName,
    Project:        run.Spec.ProjectRef.Name,
    WorkItemRef:    run.Spec.WorkItemRef,
    Namespace:      run.Namespace,
    GatedSkills:    sourceNames(gatedSkills),
    DeclaredTypes:  dedupeDeclaredTypes(gatedSkills),
    Admitted:       dec.Admitted,
    WithheldSkills: dec.WithheldSkills,
    MissingTypes:   dec.MissingTypes,
})
// ... then: if !dec.Admitted { ApplyGateHold(...) } else { advance }
```

## 5. PII / cardinality posture (enforced)

- **NFR-2 (no secret):** the gate decision never contains credential material;
  the emitter only forwards identities and type names.
  `credential_conformance_test.go` statically forbids any value-bearing
  attribute/label key (`value`/`secret`/`token`/`password`/`material`/`payload`).
- **Cardinality firewall:** the only metric labels are `outcome` (2 values) and
  `credential_type` (closed S1 binding vocabulary). Run / project / work-item /
  skill ids are **span attributes only**, never metric labels — guarded by
  `TestCredentialConformance_MetricLabelsBounded`.

## 6. Flip-to-Stable checklist (do this in / after ISI-5849)

1. Paste the helper into `pkg/coord/gatecheck_obs.go`; add the one call at the
   driver pre-dispatch admission point.
2. Add a unit test for the helper against an in-memory exporter + manual metric
   reader: assert span name/attrs, `outcome`, `missing_types` on withhold, and
   NFR-2 (no value-bearing key present on the captured span).
3. In `pkg/telemetry/semconv/credential.go`, flip the family's `Stability`
   entries from `Planned` → `Stable`. `StableCredentialMetricNames()` goes
   non-empty; `TestCredentialConformance_StableMetricsEmitted` then demands a
   real registry⊆emitter fixture — add it mirroring
   `TestSCMConformance_StableMetricsEmitted`.
4. Regenerate the rendered doc:
   `go test ./pkg/telemetry/semconv/ -run TestDocInSync -update-semconv-docs`.
