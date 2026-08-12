# KSquad Helm chart

Brings up the `ksquad-system` control plane and — per CEO directive (ISI-2149)
and architecture §16.1 / §16.2 / §9.4 — **creates and parameterizes** its
exposure and storage rather than assuming cluster defaults.

This chart scope (ISI-2149) is the two seams below. Control-plane Deployments
(operator, apiserver, console), CNPG/NATS operators, and RBAC are cluster
prerequisites or layered by their own tickets.

## Prerequisites

- Kubernetes ≥ 1.29
- **CloudNativePG operator** installed (this chart renders a CNPG `Cluster` CR)
- **NATS/JetStream operator** (dependency #2, §16) if you enable the event bus
- For `exposure.mode=gateway`: a **Gateway API controller** with a
  `GatewayClass` already installed (cilium / envoy / istio / traefik). The chart
  **references** the class and **never creates one**.

## Exposure — `exposure.*` (§16.1)

The chart creates exposure for the console and the apiserver; it does not assume
it. Pick one mode explicitly — there is no default guess.

| `exposure.mode` | Renders | SSE guarantee | When |
|---|---|---|---|
| `gateway` | `Gateway` + `HTTPRoute`×2 (+ optional redirect) | apiserver route timeout set to `0s` — honored only where the GatewayClass supports HTTPRoute timeouts (**verify per class**, see below) | Preferred production path |
| `ingress` | plain `Ingress` with SSE-safe annotations | Controller-dependent, **not** portable | Cluster has an Ingress controller but no Gateway API |
| `clusterip` | `Service` only | n/a (port-forward / your own LB) | Zero-dependency; always comes up on a bare cluster |

- **`gatewayClassName` is required** in `gateway` mode — the install fails fast
  if unset. cilium/envoy/istio/traefik are all valid targets.
- Listeners, TLS cert Secret, and http→https redirect are all values-driven, so
  you wire your own DNS/cert story without editing templates.
- The apiserver `HTTPRoute` sets `timeouts.request: "0s"` to keep a long-lived SSE
  progress stream (§13) from being cut. **Caveat (verify per GatewayClass):**
  `HTTPRoute.timeouts` is *Extended* conformance, not Core — implementations MAY
  ignore it:
  - **Envoy Gateway, Istio** — honor `timeouts`; `0s` disables the route timeout. ✓
  - **Cilium** — historically ignores the `timeouts` field silently; Envoy's
    default 15s route timeout then cuts the SSE stream. Confirm your Cilium
    version honors HTTPRoute timeouts, or terminate SSE via a Cilium-native
    `CiliumEnvoyConfig` / fall back to `ingress` mode. ⚠
  - **Traefik** — has no default route timeout, so SSE survives regardless of
    whether `0s` is honored. ✓ (by absence of a default, not by honoring the field)

  Pre-flight this the same way you pre-flight the StorageClass capability matrix
  below: verify your chosen `gatewayClassName` supports HTTPRoute timeouts before
  relying on SSE in production.

## Storage — `storage.*` (§16.2 / §9.4)

**Every PVC's `StorageClass` comes from values — never the cluster default.** An
unset class fails `helm install` fast rather than silently binding to whatever
the cluster marks default.

- Postgres (CNPG `Cluster` CR): `storage.postgres.storageClassName`
- Per-Project workspace PVCs (created by the operator at Project reconcile):
  `storage.workspace.storageClassName` (+ `accessMode`), handed to the operator
  via the `*-storage` ConfigMap
- NATS/JetStream file store: `storage.nats.storageClassName`

Each falls back to the global `storage.storageClassName` when its own is empty;
if neither is set the install fails.

### StorageClass capability matrix

Pre-flight your class against what you actually need — some behaviors are
storage-class-capability dependent:

| Capability | Requirement | KSquad use |
|---|---|---|
| `ReadWriteOnce` (RWO) | every class | **Default.** Pairs with worktree-per-Run (§9.4) |
| `ReadWriteMany` (RWX) | only some classes (CephFS, NFS, Azure Files, EFS…) | Set `storage.workspace.accessMode=ReadWriteMany` **only if supported** — enables true parallel-Run writes |
| Volume expansion | class `allowVolumeExpansion: true` | Growing a workspace/DB PVC after install |
| Snapshots | CSI driver + `VolumeSnapshotClass` | Backup/restore workflows |

RWO is the safe default and works everywhere; RWX is opt-in and gated on your
class supporting it.

## Quick start

```sh
helm install ksquad ./deploy/helm/ksquad \
  --namespace ksquad-system --create-namespace \
  --set exposure.mode=gateway \
  --set exposure.gateway.gatewayClassName=cilium \
  --set exposure.gateway.listeners.https.certSecretName=ksquad-tls \
  --set exposure.hostnames.console=ksquad.example.com \
  --set exposure.hostnames.apiserver=api.ksquad.example.com \
  --set storage.storageClassName=fast-ssd
```

Zero-dependency smoke (bare cluster):

```sh
helm install ksquad ./deploy/helm/ksquad \
  --set exposure.mode=clusterip \
  --set storage.storageClassName=standard
```

## Verify the chart

```sh
./deploy/helm/ksquad/ci/test.sh
```

Lints the chart, renders all three exposure modes, and asserts the fail-fast
guards (missing `gatewayClassName` / `storageClassName`) actually fail.
