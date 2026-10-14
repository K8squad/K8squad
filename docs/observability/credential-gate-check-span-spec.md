# `credential.gate_check` audit span — instrumentation spec (ISI-5850 / WS-9 S5 T7)

**Parent:** ISI-5818 (WS-9 S5 go-live gate). **Threat model:** Design §6 **T7 (audit trail)**.
**Blocks on:** ISI-5849 (driver pre-dispatch gate wiring — the admission point this span instruments).
**Decision shape:** `toolcred.RunGateDecision{Admitted, WithheldSkills, MissingTypes}` (ISI-5818, PR #902).

The canonical attribute/metric contract is registered in
`pkg/telemetry/semconv/credential.go` (rendered into
`docs/observability/semantic-conventions.md` → *Credential go-live gate telemetry*)
and regression-guarded by `pkg/telemetry/semconv/credential_conformance_test.go`.
This file specifies the **operational seam**: where the span is emitted, the
drop-in emitter helper, and the companion structured audit log — so the ISI-5849
wiring (or the ISI-5850 follow-up once ISI-5849 lands) is a copy-paste.

## 1. Where it is emitted

The go-live gate admission decision is computed in the **driver pre-dispatch**
path (`pkg/controller/rundrive`, ISI-5849), NOT the read-only run controller and
NOT the apiserver. The driver, right before it would dispatch a Run:

1. builds the Run's `[]toolcred.GatedSkill` from its skill annotations (S1 vocab);
2. computes the `satisfied map[string]bool` by resolving materialized Secrets
   **scoped by the project-encoded name** `proj-<project>-cred-<type>`
   (`toolcred.MaterializedSecretName`) — never a bare namespace label-list, or
   sibling projects sharing the team namespace over-admit;
3. calls `decision := toolcred.EvaluateRunGate(skills, satisfied)`.

The `credential.gate_check` span wraps step 3 as a **child of `run.reconcile`**
(same `telemetry.Tracer()` the driver already uses at
`pkg/controller/rundrive/driver.go:360`). Emit it on **every** admission
evaluation — admitted and withheld alike — so the audit trail shows both the
withhold and the later clear.

## 2. Drop-in emitter helper

Lands in `pkg/controller/rundrive` alongside the gate wiring once PR #902
(`toolcred.RunGateDecision`) is on `main`. It owns all three T7 signals — span,
metrics, structured audit log — so no admission path can emit one and forget
another.

```go
// GateCheckAudit is the T7 audit identity for one Run admission decision
// (ISI-5850). It is all identity, never credential material (NFR-2).
type GateCheckAudit struct {
	RunID        string // Run CR UID            → ksquad.run.id
	Principal    string // agent/seat identity   → ksquad.agent.name
	Project      string // run.Spec.ProjectRef   → ksquad.project.name
	WorkItemRef  string // ticket                → ksquad.work_item.ref
	Namespace    string // team namespace        → k8s.namespace.name
	GatedSkills  []string // SourceName per skill
	DeclaredType []string // deduped declared types across gated skills
}

// recordGateCheck emits the ksquad.credential.gate_check span, the
// gate_check.decisions{outcome} + withheld_by_type{credential_type} counters,
// and a structured audit log line — the full T7 trail for one admission.
// A credential VALUE never enters any of them.
func recordGateCheck(ctx context.Context, a GateCheckAudit, d toolcred.RunGateDecision) {
	outcome := "admitted"
	if !d.Admitted {
		outcome = "withheld"
	}

	_, span := telemetry.Tracer().Start(ctx, "ksquad.credential.gate_check")
	defer span.End()
	span.SetAttributes(
		attribute.String("ksquad.run.id", a.RunID),
		attribute.String("ksquad.agent.name", a.Principal),
		attribute.String("ksquad.project.name", a.Project),
		attribute.String("ksquad.work_item.ref", a.WorkItemRef),
		attribute.String("k8s.namespace.name", a.Namespace),
		attribute.StringSlice("ksquad.credential.gated_skills", a.GatedSkills),
		attribute.StringSlice("ksquad.credential.declared_types", a.DeclaredType),
		attribute.Bool("ksquad.credential.admitted", d.Admitted),
		attribute.String("ksquad.credential.outcome", outcome),
	)
	if !d.Admitted {
		span.SetAttributes(
			attribute.StringSlice("ksquad.credential.withheld_skills", d.WithheldSkills),
			attribute.StringSlice("ksquad.credential.missing_types", d.MissingTypes),
		)
	}

	gateInst().decisions.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	for _, ct := range d.MissingTypes { // bounded S1 vocab → safe label
		gateInst().withheldByType.Add(ctx, 1, metric.WithAttributes(attribute.String("credential_type", ct)))
	}

	// Structured audit log (T7). Same keyset; value never logged.
	logr.FromContextOrDiscard(ctx).Info("credential gate check",
		"outcome", outcome,
		"run", a.RunID, "principal", a.Principal,
		"project", a.Project, "namespace", a.Namespace, "workItem", a.WorkItemRef,
		"gatedSkills", a.GatedSkills, "declaredTypes", a.DeclaredType,
		"missingTypes", d.MissingTypes, "withheldSkills", d.WithheldSkills)
}
```

`gateInst()` is the lazy instrument holder (mirroring `funnelInst()` in
`internal/apiserver/obsfunnel.go`) created after `telemetry.Setup`:

```go
func gateInst() *gateMetrics {
	gateOnce.Do(func() {
		m := telemetry.Meter()
		g := &gateMetrics{}
		g.decisions, _ = m.Int64Counter("ksquad.credential.gate_check.decisions",
			metric.WithUnit("1"),
			metric.WithDescription("Go-live gate admission decisions by bounded outcome (ISI-5850, T7)."))
		g.withheldByType, _ = m.Int64Counter("ksquad.credential.gate_check.withheld_by_type",
			metric.WithUnit("1"),
			metric.WithDescription("Per-credential-type withhold count over the bounded S1 vocab (ISI-5850, T7)."))
		gateOnceVal = g
	})
	return gateOnceVal
}
```

Call site in the driver, immediately after the oracle:

```go
decision := toolcred.EvaluateRunGate(skills, satisfied)
recordGateCheck(ctx, GateCheckAudit{
	RunID: string(run.UID), Principal: run.Spec.Agent, Project: run.Spec.ProjectRef,
	WorkItemRef: run.Spec.WorkItemRef, Namespace: run.Namespace,
	GatedSkills: sourceNames(skills), DeclaredType: declaredTypes(skills),
}, decision)
if !decision.Admitted {
	// → coord.ApplyCredentialSignal(credential_required) / StepPausedCredentialRequired (ISI-5849)
}
```

## 3. PII / secret posture (NFR-2, mirrored from `obsfunnel.go`)

- A credential **value** never enters a span attribute, a metric label, or a log
  field. The audit key is **principal + credential type + namespace** — all
  identities.
- High-cardinality ids (`run.id`, `project`, `work_item.ref`) ride the **span
  only**, never a metric label. The only labels are `outcome` (2-value) and
  `credential_type` (closed S1 vocab).
- Add `TestNFR2GateCheckSecretNeverInTelemetry` next to the helper (same shape as
  `internal/apiserver/obsfunnel_test.go`): drive `recordGateCheck` with a
  populated decision through a span recorder + a captured logger, assert no
  attribute/log field value matches a planted secret sentinel, and assert the
  emitted metric labels ⊆ {`outcome`, `credential_type`}.

## 4. Flip-to-Stable checklist (ISI-5850 follow-up, auto-wakes when ISI-5849 lands)

1. Land the helper + call site in `pkg/controller/rundrive` (needs PR #902's
   `toolcred.RunGateDecision` on `main`).
2. Flip `CredentialSpanConventions` + `CredentialMetricConventions` entries from
   `Planned`/`WS-9` to `Stable` in `pkg/telemetry/semconv/credential.go`.
3. Regenerate the doc: `go test ./pkg/telemetry/semconv/ -run TestDocInSync -update-semconv-docs`.
4. Add the real driver-emitter fixture to
   `credential_conformance_test.go::TestCredentialConformance_StableMetricsEmitted`
   (mirror `TestSCMConformance_StableMetricsEmitted`); it now enforces
   registry ⊆ emitter.
5. Validate live against Dynatrace (O11y `validate-traces`): a withheld Run shows
   `outcome=withheld` + non-empty `missing_types`, and the later clear shows
   `outcome=admitted` on the same run id.
