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

## Toolchains & the default catalog

Skills declare their CLI needs as `name@version` refs
(`Skill.spec.requires.toolchains`, e.g. `gh@2.62`). Those refs resolve against
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
| `kubectl` | `1.31` | read-only core + apps (get/list/watch) |
| `git` | `2.45` | none (staged onto PATH only) |
| `gh` | `2.62` | none |
| `go` | `1.23` | none |
| `node` | `22` | none |
| `dtctl` | `1.0` | none |
| `helm` | `3.16` | none |

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
