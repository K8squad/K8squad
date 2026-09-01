# k8squad Helm chart

Installs the K8squad control-plane API surface on a cluster:

- the control-plane namespace (`k8squad-system` by default),
- the control-plane services (apiserver, operator, console, memory,
  scm-webhook, event bus) when `controlPlane.enabled=true`, and
- optionally, the **default toolchain catalog** (see *Toolchains & the
  default catalog* below).

This chart ships **no CRDs**. The eleven `ksquad.io` CRDs are owned by the
standalone [`k8squad-crds`](../helm-crds/README.md) chart, which must be
installed/upgraded **first** so the control plane and the CRD schema roll on
independent cadences (ADR-0002, Option B — see *CRD lifecycle* below).

This is the chart CI publishes to <https://charts.k8squad.io>
(`make helm-package` / `.github/workflows/helm-release.yml`).

## Install

CRDs first, control plane second (ADR-0002 §3):

```sh
helm install k8squad-crds config/helm-crds --wait
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
| `controlPlane.apiserver.bootstrapAdmin.enabled` | `false` | Seed the first admin (feature 15.2) so the Console `/login` is usable on a fresh install. OFF by default; runs ONCE when `auth.user` is empty (see *First login* below). |
| `controlPlane.apiserver.bootstrapAdmin.username` | `""` | Admin username to seed. **Required** when `bootstrapAdmin.enabled`. |
| `controlPlane.apiserver.bootstrapAdmin.existingSecret` | `""` | Name of an existing Secret holding the bootstrap password. **Required** when enabled — the password is never read from plaintext values. |
| `controlPlane.apiserver.bootstrapAdmin.passwordKey` | `password` | Key within `existingSecret` holding the plaintext password. |
| `controlPlane.apiserver.bootstrapAdmin.teamId` | `""` | Optional: pin the admin's tenancy-root team UUID. Empty → auto-minted on first seed. |
| `controlPlane.apiserver.secureCookies` | `""` | Session-cookie `Secure` attribute. `""` = auto (emit `KSQUAD_SECURE_COOKIES=false` only for an http-only gateway, avoiding a login loop); `"true"`/`"false"` force it. See *Session cookies over plain HTTP*. |
| `exposure.gateway.enabled` | `false` | Render a `Gateway` + `HTTPRoute`s for the console + apiserver (see *Exposure* below). |
| `exposure.gateway.gatewayClassName` | `""` | **Required** when enabled — the existing `GatewayClass` to reference (e.g. `kgateway`). The chart never creates one. |
| `exposure.gateway.hostnames.console` / `.apiserver` | `""` | **Optional** host-based routing. Leave both empty to expose by IP (console owns `/` including the BFF at `/api/*`; no direct apiserver route). Set `hostnames.apiserver` to also expose the apiserver on its own host. |
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
controller you installed: kgateway, Cilium, Envoy Gateway, Istio, Traefik…).
The GatewayClass is the only required knob — **hostnames are optional**:

```sh
# Expose by IP — no DNS. console owns http://<gwIP>/ including the BFF at /api/*.
helm upgrade k8squad config/helm -n k8squad-system \
  --set controlPlane.enabled=true \
  --set exposure.gateway.enabled=true \
  --set exposure.gateway.gatewayClassName=kgateway

# …or route by host — set either or both hostnames:
  --set exposure.gateway.hostnames.console=ksquad.example.com \
  --set exposure.gateway.hostnames.apiserver=api.ksquad.example.com
```

This renders, in the control-plane namespace:

- a `Gateway` named `ksquad` referencing your `gatewayClassName`, with an HTTP
  listener (and an HTTPS listener when `listeners.https.enabled`);
- an `HTTPRoute` `ksquad-console` → the `ksquad-console` Service (`:80`) at
  path `/`, keyed on `hostnames.console` when set. The console owns the whole
  origin, including the BFF surface under `/api/*` (login, runs, …) — the
  browser's only path per ADR-013/§13;
- an `HTTPRoute` `ksquad-apiserver` → the `ksquad-apiserver` Service (`:8080`)
  **only when `hostnames.apiserver` is set** — direct apiserver ingress on its
  own host, with `timeouts.request: "0s"` so the long-lived SSE progress stream
  (§13) is never cut. `HTTPRoute.timeouts` is Extended conformance —
  kgateway/Envoy-based classes honor `0s`; verify per class.

**Hostnames are optional (expose by IP).** Omit `hostnames.*` and the console
route renders without a `hostnames` field, matching any host on the Gateway's
assigned address — the console owns `/` for EVERYTHING, including its `/api/*`
BFF. In this IP-only mode **no browser-facing apiserver route is rendered**: a
`/api` apiserver route would shadow the entire BFF and break login (ISI-3530).
The browser reaches the apiserver only through the BFF (ClusterIP, in-cluster).
Set a dedicated `hostnames.apiserver` to also expose the apiserver directly — it
then owns `/` on that host, collision-free by hostname specificity.

The install **fails fast** only if `exposure.gateway.enabled=true` without
`controlPlane.enabled` or without a `gatewayClassName` — no half-wired Gateway
with dangling routes. Find the assigned address with
`kubectl -n k8squad-system get gateway ksquad`; point any hostnames' DNS there.

For TLS termination at the edge, enable the https listener and supply a cert
Secret, optionally redirecting http→https:

```sh
  --set exposure.gateway.listeners.https.enabled=true \
  --set exposure.gateway.listeners.https.certSecretName=ksquad-tls \
  --set exposure.gateway.httpsRedirect=true
```

### Session cookies over plain HTTP

The apiserver defaults to **`SecureCookies=true`**, so the session cookie carries
the `Secure` attribute. Over a **plain-HTTP gateway** the browser silently drops
that cookie and the Console middleware bounces every post-login navigation back
to `/login`. To avoid a login loop the chart resolves
`controlPlane.apiserver.secureCookies`:

| Value | Behaviour |
| --- | --- |
| `""` (default, *auto*) | Emit `KSQUAD_SECURE_COOKIES=false` **only** when the gateway is enabled and TLS is **not** terminated at it (`listeners.https.enabled=false`). In every other case nothing is emitted and the apiserver keeps its secure default. |
| `"true"` / `"false"` | Force the value explicitly, overriding auto. |

The robust production posture is to **terminate TLS at the Gateway**
(`listeners.https.enabled` + `certSecretName`) and leave `secureCookies: ""` — the
browser is on HTTPS, the cookie stays `Secure` end-to-end, and no override is
rendered.

## First login (bootstrap admin)

A fresh install has **no users**, so the Console `/login` screen renders but
nobody can sign in yet. The apiserver seeds the first admin (feature 15.2) on
startup **only when `auth.user` is empty** and both a username and password are
supplied — idempotent by construction, so it is a no-op on every later start.
This is OFF by default: a deliberate opt-in for new installs.

The password is **never** read from plaintext values — it must come from a
Secret you create out-of-band:

```sh
# 1) Create the bootstrap password Secret (choose a strong password).
kubectl -n k8squad-system create secret generic ksquad-bootstrap-admin \
  --from-literal=password="$(openssl rand -base64 18)"

# 2) Enable the seed and point the apiserver at that Secret.
helm upgrade --install k8squad config/helm -n k8squad-system \
  --set controlPlane.enabled=true \
  --set controlPlane.apiserver.bootstrapAdmin.enabled=true \
  --set controlPlane.apiserver.bootstrapAdmin.username=admin \
  --set controlPlane.apiserver.bootstrapAdmin.existingSecret=ksquad-bootstrap-admin

# 3) Confirm the seed ran (once):
kubectl -n k8squad-system logs deploy/ksquad-apiserver | grep 'bootstrap admin'
#   → ksquad-apiserver: bootstrap admin "admin" created (principal user:admin) …
```

Then sign in at the Console `/login` with that username + password. **After the
first successful login, remove the bootstrap credential** — the seed has already
run and no longer needs it:

```sh
helm upgrade --install k8squad config/helm -n k8squad-system \
  --set controlPlane.apiserver.bootstrapAdmin.enabled=false   # …and your other values
kubectl -n k8squad-system delete secret ksquad-bootstrap-admin
```

Set `bootstrapAdmin.teamId` to a fixed UUID to pin the admin's tenancy-root team
(otherwise one is auto-minted on first seed and logged). Turning the seed on
against a cluster that already has users is safe — the apiserver logs
`bootstrap admin skipped — N users exist` and does nothing.

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

## CRD lifecycle (owned by the k8squad-crds chart)

This chart ships **zero** CRDs. Per
[ADR-0002](../../docs/adr/ADR-0002-crd-lifecycle-via-helm.md) (Option B), the
eleven `ksquad.io` CRDs live in the standalone, independently-versioned
[`k8squad-crds`](../helm-crds/README.md) chart, so CRD schema rolls forward — or
holds back — on its own cadence, decoupled from control-plane upgrades. That
chart renders the CRDs as ordinary templates (not Helm's install-only `crds/`
dir), so `helm upgrade k8squad-crds` reconciles their schema; and it annotates
them `helm.sh/resource-policy: keep`, so uninstalling never deletes user CRs.

**Ordering:** install/upgrade `k8squad-crds` **before** this chart (§3), so the
API server has the CRDs established before the control plane creates any CR
(e.g. the toolchain catalog `Toolchain`).

**Version skew (§4):** this chart declares the minimum CRD-chart version it
needs as `annotations."k8squad.io/min-crds-version"` in `Chart.yaml`, echoed in
`NOTES.txt` with a `kubectl get crd` self-check. A newer CRD chart is always
safe (additive-only); older than the minimum is unsupported.

The CRD source of truth is `config/crd/bases/*.yaml` (from `make manifests`),
synced into the CRD chart by `make helm-sync-crds`; `make verify-codegen` fails
CI on drift. See the [`k8squad-crds` README](../helm-crds/README.md) for its
values and the kind CI propagation test.

> The other chart, `deploy/helm/ksquad`, also ships no CRDs (services only) and
> likewise depends on `k8squad-crds` being installed first (ADR-0002 §8).

## Verification

```sh
make helm-lint      # helm lint both charts (config/helm + config/helm-crds)
make helm-template  # helm template both charts (local render, no cluster)
```
