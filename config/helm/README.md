# k8squad Helm chart

Installs the K8squad control-plane API surface on a cluster:

- the `ksquad.io` CRD group from `crds/` (`Team`, `Agent`, `AgentRuntime`,
  `Role`, `Skill`, `Project`, `Run`, `EgressPolicy`, `OtelConfig`,
  `MCPServer`, `Toolchain` — Helm installs CRDs on first install and never
  upgrades or deletes them — see *CRD lifecycle* below),
- the control-plane namespace (`k8squad-system` by default), and
- optionally, the **default toolchain catalog** (see *Toolchains & the
  default catalog* below).

This is the chart CI publishes to <https://charts.k8squad.io>
(`make helm-package` / `.github/workflows/helm-release.yml`).

## Install

```sh
helm install k8squad config/helm -n k8squad-system --create-namespace
kubectl api-resources --api-group=ksquad.io
```

The chart also creates the control-plane namespace itself when
`namespace.create` is true (default), so installing into another release
namespace is fine.

## Values

| Key | Default | Description |
| --- | --- | --- |
| `namespace.create` | `true` | Create the control-plane namespace as a chart resource. |
| `namespace.name` | `k8squad-system` | Control-plane namespace name. |
| `namespace.labels` | `{}` | Extra labels on the namespace. |
| `namespace.annotations` | `{}` | Extra annotations on the namespace. |
| `nameOverride` / `fullnameOverride` | `""` | Standard label-name overrides. |
| `tools.defaultCatalog.enabled` | `false` | Render the curated toolchain catalog as `Toolchain` objects in the control-plane namespace. |
| `tools.defaultCatalog.entries` | curated set | Per-tool catalog entries (versions, images, RBAC) — override or extend. |
| `tools.rbac.clusterScopeEnabled` | `false` | Allow cluster-catalog Toolchains to declare `rbac.scope: cluster`. Never default-on. |
| `controlPlane.nats.enabled` | `true` | Bundle the NATS/JetStream event bus (§16/§17.4/ADR-023). ON by default so `controlPlane.enabled=true` yields a complete plane; auto-enables `eventRelay` and derives its `natsUrl`. |
| `controlPlane.nats.persistence.storageClassName` | `""` | **Required** when the bundled bus is on — the JetStream PVC never uses the cluster-default class (§16.2). |
| `controlPlane.nats.persistence.size` | `4Gi` | JetStream file-store PVC size. |
| `controlPlane.nats.ha.enabled` / `.replicas` | `false` / `3` | Clustered JetStream RAFT quorum (replicas must be odd, ≥3). |
| `controlPlane.eventRelay.natsUrl` | `""` | Point the relay at an EXTERNAL NATS instead of the bundled bus (takes precedence; set `controlPlane.nats.enabled=false`). |
| `exposure.gateway.enabled` | `false` | Render a `Gateway` + `HTTPRoute`s for the console + apiserver (see *Exposure* below). |
| `exposure.gateway.gatewayClassName` | `""` | **Required** when enabled — the existing `GatewayClass` to reference (e.g. `kgateway`). The chart never creates one. |
| `exposure.gateway.hostnames.console` / `.apiserver` | `""` | **Required** when enabled — host-based routing for each workload. |
| `exposure.gateway.listeners.https.enabled` / `.certSecretName` | `false` / `""` | Terminate TLS at the Gateway; `certSecretName` is required when the https listener is on. |
| `exposure.gateway.httpsRedirect` | `false` | Render an http→https `RequestRedirect` route (needs both listeners enabled). |

The event bus is best-effort: Postgres/CNPG is the sole source of truth
(ADR-001), the write path uses a transactional outbox, and NATS-down never
blocks a Run/claim/write — only live plugin event streaming/replay is degraded.

## Exposure (Gateway API)

By default the control-plane workloads are `ClusterIP` Services — a bare
`controlPlane.enabled=true` needs no Gateway API to succeed, and you reach the
console with `kubectl -n k8squad-system port-forward svc/ksquad-console 8080:80`.

To have the chart route ingress traffic automatically, enable the exposure
layer and name an **existing** `GatewayClass` (the chart *references* a
GatewayClass and never creates one — the class is owned by whichever Gateway
controller you installed: kgateway, Cilium, Envoy Gateway, Istio, Traefik…):

```sh
helm upgrade k8squad config/helm -n k8squad-system \
  --set controlPlane.enabled=true \
  --set exposure.gateway.enabled=true \
  --set exposure.gateway.gatewayClassName=kgateway \
  --set exposure.gateway.hostnames.console=ksquad.example.com \
  --set exposure.gateway.hostnames.apiserver=api.ksquad.example.com
```

This renders, in the control-plane namespace:

- a `Gateway` named `ksquad` referencing your `gatewayClassName`, with an HTTP
  listener (and an HTTPS listener when `listeners.https.enabled`);
- an `HTTPRoute` `ksquad-console` → the `ksquad-console` Service (`:80`),
  matched on `hostnames.console`;
- an `HTTPRoute` `ksquad-apiserver` → the `ksquad-apiserver` Service (`:8080`),
  matched on `hostnames.apiserver`, with `timeouts.request: "0s"` so the
  long-lived SSE progress stream (§13) is never cut. `HTTPRoute.timeouts` is
  Extended conformance — kgateway/Envoy-based classes honor `0s`; verify per
  class.

The install **fails fast** if `exposure.gateway.enabled=true` without
`controlPlane.enabled`, without a `gatewayClassName`, or without both
hostnames — no half-wired Gateway with dangling routes. Point both hostnames'
DNS at the address your controller assigns the `ksquad` Gateway
(`kubectl -n k8squad-system get gateway ksquad`).

For TLS termination at the edge, enable the https listener and supply a cert
Secret, optionally redirecting http→https:

```sh
  --set exposure.gateway.listeners.https.enabled=true \
  --set exposure.gateway.listeners.https.certSecretName=ksquad-tls \
  --set exposure.gateway.httpsRedirect=true
```

## Toolchains & the default catalog

Skills declare their CLI needs as `name@version` refs
(`Skill.spec.requires.toolchains`, e.g. `gh@2.98`). Those refs resolve against
`Toolchain` objects — the **cluster catalog** lives in the control-plane
namespace, and team namespaces may only *narrow* it (subset versions/rules),
never widen it. Enable the curated default catalog with one flag:

```sh
helm install k8squad config/helm -n k8squad-system --create-namespace \
  --set tools.defaultCatalog.enabled=true

# or on an existing release:
helm upgrade k8squad config/helm -n k8squad-system --set tools.defaultCatalog.enabled=true
```

What you get (`kubectl get toolchains -n k8squad-system`):

| Tool | Version | RBAC granted |
| --- | --- | --- |
| `kubectl` | `1.36` | read-only core + apps (get/list/watch) |
| `git` | `2.45` | none (staged onto PATH only) |
| `gh` | `2.98` | none |
| `go` | `1.26` | none |
| `node` | `22` | none |
| `dtctl` | `1.0` | none |
| `helm` | `3.21` | none |
| `python` | `3.12` | none |
| `docker-cli` | `29` | none (client only; daemon stays the `dockerd` sidecar) |
| `uv` | `0.5` | none |
| `jq` | `1.7` | none |
| `yq` | `4` | none |
| `curl` | `8` | none |
| `make` | `4` | none |

Design notes (plan §2.2/§2.2b, ISI-3280):

- **No RBAC plumbing.** The operator renders each Run's unioned toolchain RBAC
  into a per-Run `Role` bound to the squad ServiceAccount, garbage-collected
  with the Run. Deploying a new tool for squads is one `Toolchain` object (or
  one flag) — never a hand-written `Role`/`RoleBinding`.
- **Fail-closed resolution.** A Run whose skills require an unknown toolchain
  name or version is rejected at admission with a message naming the demanding
  skill; version conflicts across a Run's skills fail the same way.
- **Overrides.** Set `tools.defaultCatalog.entries.<tool>.versions` to pin
  different versions or images; add keys to extend the catalog. Team-namespace
  `Toolchain` overrides may narrow catalog entries but never widen them
  (admission-enforced).
- **Cluster scope.** `tools.rbac.clusterScopeEnabled=true` injects
  `KSQUAD_TOOLCHAIN_CLUSTER_SCOPE_ENABLED` on the webhook and operator,
  admitting `rbac.scope: cluster` **only** for cluster-catalog entries —
  rendered as a per-Run `ClusterRole`. This is an explicit platform opt-in;
  it is never default-on and never renderable from a team namespace.

Related: `MCPServer` objects (the other half of the capability plane) are
cluster-user-authored CRDs, not chart-rendered — see the BMAD squad example in
[`examples/bmad-team/02b-mcpservers.yaml`](../../examples/bmad-team/02b-mcpservers.yaml)
and the [getting-started guide](../../docs/getting-started-bmad.md).

## CRD lifecycle

Files under `crds/` are **generated artifacts**, byte-identical copies of
`config/crd/bases/*.yaml` produced by `make manifests`. Never edit them by
hand. After regenerating manifests, sync them into the chart:

```sh
make helm-sync-crds
```

Helm semantics for the `crds/` directory:

- CRDs are installed before templates on **first** install of the release.
- `helm upgrade` does **not** update CRDs in `crds/`; upgrades of the CRD
  contract are applied via `kubectl apply -f config/crd/bases/` (or
  `kubectl apply -k config/crd`) until the CRDs stabilize post-1.0.
- `helm uninstall` does not delete CRDs (by design, to protect CR data).

## Verification

```sh
make helm-lint      # helm lint config/helm
make helm-template  # helm template --include-crds (local render, no cluster)
```
