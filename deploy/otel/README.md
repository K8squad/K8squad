# deploy/otel — Collector chart-of-record (ISI-4163)

This directory is the **chart-of-record for the collector topology** running in
the `observability` namespace. Decision and rationale: ISI-4163.

## What lives here

| File | Contents | Applied as |
|---|---|---|
| `gateway-collector.yaml` | `OpenTelemetryCollector/otel-gateway` (opentelemetry.io/v1beta1) — workload OTLP gateway: memory_limiter → k8sattributes → resource → cumulativetodelta/redaction → tail_sampling → batch → Dynatrace | Managed by the OpenTelemetry Operator; the operator derives Service `otel-gateway-collector.observability` from the CR name |
| `node-logs-daemonset.yaml` | `otel-node-logs` DaemonSet + ConfigMap + SA + the shared `otel-k8sattributes` ClusterRole/Binding — node-level pod log collection, exports OTLP/HTTP to the gateway | Plain manifests (not operator-managed) |

## Platform contract this preserves

`config/helm` (the deployed `k8squad` control-plane chart) injects
`OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-gateway-collector.observability:4317`
into every control-plane workload (`config/helm/values.yaml` →
`controlPlane.otel.endpoint`, live since ISI-3484; honored as the operator's
stdout fallback since ISI-4102). The gateway CR name `otel-gateway` is what
makes the operator generate exactly that Service name — **do not rename the
CR** without updating the chart value in lockstep.

## Precondition (out-of-band secret)

The gateway exporter authenticates to Dynatrace via
`Secret/dynatrace-otlp` (key `token`) in the `observability` namespace,
referenced through `env: KSQUAD_OTLP_AUTH`. The Secret is created out-of-band
and is intentionally **not** in this repo.

## Applying

```sh
kubectl apply -k deploy/otel/
```

Requires the OpenTelemetry Operator (serving `opentelemetry.io/v1beta1`) to
already be installed; the operator install itself is cluster tooling and out
of scope here.

## Relationship to the Helm charts

- `config/helm` (deployed): ships **no** collector templates by design — it
  only renders the opt-in `OTelConfig` routing CR and the OTLP env wiring.
  Collector topology is owned here.
- `deploy/helm/ksquad` (undeployed): its `otel-collector*.yaml` templates are
  **superseded** by this directory (they render a raw Deployment named
  `<release>-otel-collector` in the release namespace, which neither matches
  the contract Service name nor the operator-managed live topology). Their
  retirement/guarding is tracked in the ISI-4163 migration follow-up.

Drift rule: changes to the in-cluster collectors must land here first (or be
captured back here immediately after `kubectl apply`), so repo and cluster do
not diverge again.
