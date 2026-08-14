# k8squad Helm chart (Story 1.4 skeleton)

Installs the K8squad control-plane API surface on a cluster:

- the `ksquad.io` CRD group from `crds/` (Team, Agent, Role, Skill, Project,
  Run — plus AgentRuntime from Story 1.3; Helm installs CRDs on first install
  and never upgrades or deletes them — see *CRD lifecycle* below), and
- the control-plane namespace (`k8squad-system` by default).

This is the Story 1.4 **skeleton**: services (apiserver, operator, console,
memory, shim) are wired in later epics. Target: a first `helm install` that
puts the CRD contract on a cluster in minutes (arch S1, ≤4h full install).

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
