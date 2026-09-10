# deploy/otel — Collector chart-of-record (ISI-4163)

This directory owns the **collector topology** for k8squad. Decision and
rationale: ISI-4163 (revised after board feedback — operator ownership must be
optional, and BYO-collector must be supported).

## Layout

| Path | What |
|---|---|
| `chart/` | `k8squad-otel` Helm chart — the chart-of-record. Renders the topology in one of three modes (below). |
| `reference/` | Verbatim snapshot of the live in-cluster manifests (ISI-4153 capture) — drift baseline the chart was built from. |

## Three modes (`collector.mode`)

| Mode | Renders | Use when |
|---|---|---|
| `operator` (default) | `OpenTelemetryCollector` CR; the otel-operator reconciles it into Deployment + Service `otel-gateway-collector.<ns>` | The OpenTelemetry Operator is (or may be) installed cluster-side. Matches what runs in our cluster today. |
| `deployment` | Plain Deployment + ConfigMap + Service **literally named** `otel-gateway-collector.<ns>` — no operator required | Clusters without the otel-operator. |
| `external` | No gateway at all (node-logs optional, pointed at `collector.external.endpoint`) | The user already runs their own collector (BYO). Workload wiring is then done by setting `controlPlane.otel.endpoint` in `config/helm` to the user's collector. |

## Platform contract this preserves

`config/helm` (the deployed `k8squad` control-plane chart) injects
`OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-gateway-collector.observability:4317`
into every control-plane workload (`controlPlane.otel.endpoint`, live since
ISI-3484; honored as the operator's stdout fallback since ISI-4102).

- `operator` mode: the operator derives the Service name from the CR name
  (`otel-gateway` → `otel-gateway-collector`).
- `deployment` mode: the chart renders the Service with that literal name.
- `external` mode: the contract is explicitly the user's responsibility —
  they point `controlPlane.otel.endpoint` at their own collector.

**Do not rename `collector.name`** in operator/deployment mode without
updating the `config/helm` value in lockstep.

## Precondition (out-of-band secret, non-external modes)

The gateway exporter authenticates to the vendor backend via
`Secret/dynatrace-otlp` (key `token`) in the release namespace, referenced
through `env: KSQUAD_OTLP_AUTH`. Created out-of-band; **never** in this repo.

## Installing

```sh
# operator mode (default) — requires the otel-operator cluster-side
helm install k8squad-otel deploy/otel/chart -n observability --create-namespace

# no operator available
helm install k8squad-otel deploy/otel/chart -n observability --create-namespace \
  --set collector.mode=deployment

# user brings their own collector
helm install k8squad-otel deploy/otel/chart -n observability --create-namespace \
  --set collector.mode=external \
  --set collector.external.endpoint=http://my-collector.monitoring:4318
# and in config/helm: controlPlane.otel.endpoint=http://my-collector.monitoring:4317
```

## Relationship to the other charts

- `config/helm` (deployed): ships **no** collector templates by design — only
  the opt-in `OTelConfig` routing CR and the OTLP env wiring (which is also
  the BYO-collector seam). Collector topology is owned here.
- `deploy/helm/ksquad` (undeployed): its `otel-collector*.yaml` templates are
  **superseded** by this chart. Their retirement is tracked in ISI-4164.

Drift rule: changes to the in-cluster collectors land in this chart first (or
are captured back immediately after a manual apply), so repo and cluster do
not diverge again. `reference/` is the baseline for drift diffs.
