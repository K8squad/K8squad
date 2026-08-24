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

### Access-mode behavior per StorageClass (§9.4)

`storage.workspace.accessMode` sets the access mode the operator stamps on **every**
per-Project workspace PVC. Its behavior is deliberately class-dependent — the chart
surfaces the trade-off rather than guessing:

- **`ReadWriteOnce` (RWO) — the default.** Every StorageClass supports RWO. It pairs
  with the worktree-per-Run isolation model (§9.4): each Run gets its own git worktree
  on a single-node-mounted volume, so RWO is sufficient and portable. **Leave it here
  unless you have a specific reason not to.**
- **`ReadWriteMany` (RWX) — optional, only where the class supports it.** RWX lets
  multiple pods mount the same workspace volume for true parallel-Run writes, but only
  a subset of classes provide it (CephFS, NFS, Azure Files, EFS, …). Setting RWX on a
  class that lacks it makes every workspace PVC hang `Pending` — a silent install
  stall. The chart **cannot verify class capabilities offline**, so it does not reject
  RWX; instead it **warns** at render (`helm install` NOTES) that you must pre-flight
  the class. This is the honest seam: RWX is a valid choice you own the pre-flight for.
- **`ReadWriteOncePod` (RWOP)** — a valid enum member (K8s ≥ 1.29) for restricting a
  PVC to a single pod; same class-support caveat applies.
- **Volume expansion and snapshots are class-dependent too**, independent of access
  mode: growing a PVC needs `allowVolumeExpansion: true` on the class; snapshot/restore
  needs a CSI driver plus a `VolumeSnapshotClass`. Neither is something the chart can
  turn on — they are capabilities of the class you point it at.

**Schema teeth (`values.schema.json`):** the chart validates `storage.workspace.accessMode`
against the enum `ReadWriteOnce | ReadWriteMany | ReadWriteOncePod`, so a typo
(`ReadWriteMnay`, `rwx`, `ReadOnlyMany`) fails `helm install`/`template` up front with a
clear schema error — never a PVC that silently never binds. RWX passes the enum (it is
valid) and triggers the render-time warning above.

## Event bus — NATS / JetStream (the plugin seam)

CEO decision (2026-08-11, ADR-023): **data in Postgres, events on NATS**. The
chart brings up **NATS with JetStream enabled** as stateful dependency #2 — the
substrate plugins subscribe to (Epic 12). Postgres stays the sole source of
truth; NATS holds only in-flight/replayable event copies.

- **Bundled, JetStream on.** `nats.enabled: true` (default) renders a NATS
  StatefulSet with JetStream and a file-store PVC. It is **parent-rendered**
  (like the CNPG `Cluster` CR) rather than an upstream subchart, specifically so
  the JetStream PVC's `storageClassName` stays the single `storage.nats.storageClassName`
  knob (§16.2) — never the cluster default. `helm template`/`install` fails fast
  if that class is unset while `nats.enabled`.
- **Single-replica default; HA via a values toggle** (same shape as CNPG's
  `storage.postgres.instances`). `nats.ha.enabled: true` with an odd
  `nats.ha.replicas` (≥3) renders a clustered JetStream RAFT quorum. The default
  single replica keeps the ≤4h install (S1) light.
- **The apiserver outbox relay publishes to it** (Epic 12.1). The chart renders
  an `*-event-relay` ConfigMap telling the apiserver where the bus is
  (`ksquad.nats.url`, release-derived unless `events.relay.natsUrl` overrides)
  and the subject taxonomy `ksquad.{entity}.{project}.{squad}.{event_type}`.
- **NATS-down never blocks a Run/claim/write.** The relay is **decoupled** from
  the write path — it tails the transactional Postgres outbox out-of-band,
  republishing unflushed rows at-least-once, and is **never** wired into an
  apiserver liveness/readiness probe (`relay.blocksWritePath: "false"`,
  `relay.natsHealthGatesApiserver: "false"`). Setting `nats.enabled: false` turns
  the bus off entirely and the core still installs — the relay just buffers in
  the outbox.

```sh
# HA event bus (clustered JetStream) + explicit NATS StorageClass
helm install ksquad ./deploy/helm/ksquad \
  ... \
  --set storage.nats.storageClassName=fast-ssd \
  --set nats.ha.enabled=true --set nats.ha.replicas=3

# Core-only, no bus (relay buffers in the outbox)
helm install ksquad ./deploy/helm/ksquad ... --set nats.enabled=false
```

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

## Egress control (story 4.6, ISI-2888)

KSquad implements a default-deny egress policy to secure outbound traffic. Projects can specify their egress requirements via the `egressPolicyRef` field, which triggers the creation of allowlist NetworkPolicies.

### Configuration

```yaml
egress:
  enabled: true
  defaultEgressProxy:
    service: "egress-proxy"
    port: 8080
  networkPolicy:
    defaultDenyName: "ksquad-default-deny"
    projectEgressPrefix: "ksquad-egress"
```

### Egress proxy pattern

The default pattern uses an egress proxy for secure outbound access:

1. **Sandbox pods** can only reach their team's egress proxy
2. **Egress proxy** can reach infrastructure services  
3. **Infrastructure services** only accept traffic from egress proxies

This ensures all outbound traffic is proxied and auditable.

### Project egress configuration

Projects specify egress requirements in their spec:

```yaml
spec:
  egressPolicyRef:
    name: "my-egress-policy"
```

The Project controller automatically creates corresponding NetworkPolicies that allowlist specific destinations while maintaining the default-deny baseline.

See [docs/egress-configuration.md](../docs/egress-configuration.md) for detailed architecture and troubleshooting.

## Verify the chart

```sh
./deploy/helm/ksquad/ci/test.sh
```

Lints the chart, renders all three exposure modes, and asserts the fail-fast
guards (missing `gatewayClassName` / `storageClassName`) actually fail.
