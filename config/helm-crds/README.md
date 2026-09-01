# k8squad-crds Helm chart

The **standalone, independently-versioned** chart that owns the eleven
`ksquad.io` CustomResourceDefinitions (`Team`, `Agent`, `AgentRuntime`, `Role`,
`Skill`, `Project`, `Run`, `EgressPolicy`, `OtelConfig`, `MCPServer`,
`Toolchain`).

It exists so CRD schema can be rolled forward — or held back — **independently
of the control-plane chart** (`k8squad`, `config/helm`), per
[ADR-0002](../../docs/adr/ADR-0002-crd-lifecycle-via-helm.md) (Option B). The
CRDs render as ordinary chart templates (not Helm's install-only `crds/` dir),
so `helm upgrade k8squad-crds` reconciles their schema onto existing installs —
the core guarantee of this chart.

## Install / upgrade ordering — CRDs first, always

```sh
# install
helm install k8squad-crds config/helm-crds --wait
helm install k8squad      config/helm      --wait --create-namespace -n k8squad-system

# roll CRD schema forward independently of the control plane
helm upgrade k8squad-crds config/helm-crds --wait
```

The API server must have the CRDs **established** before the control-plane chart
creates any CR (e.g. the `toolchain-default-catalog` `Toolchain`). Installing or
upgrading `k8squad-crds` first with `--wait` guarantees that ordering. Reversing
the order (control plane first, on a fresh cluster) fails fast with
`no matches for kind` — expected.

## Values

| Key | Default | Description |
| --- | --- | --- |
| `keep` | `true` | Annotate every CRD with `helm.sh/resource-policy: keep` so `helm uninstall` never deletes the CRDs (and never cascade-deletes user CRs). Set `false` only on throwaway/test clusters. |

There is deliberately **no `crds.install` toggle**: installing this chart *is*
the opt-in; not installing it (or `helm uninstall k8squad-crds --set keep=false`)
is the opt-out. GitOps users who manage CRDs out-of-band simply don't deploy
this chart and apply `config/crd/bases/` from their own source.

## Version-skew policy (ADR-0002 §4)

- This chart carries its **own `version`** (start `0.1.0`), bumped on **any** CRD
  schema change. It is the single version operators pin and roll independently.
- The control-plane chart declares the minimum CRD-chart version it needs as
  `annotations."k8squad.io/min-crds-version"` in `config/helm/Chart.yaml`.
- **CRD chart newer than the control plane requires → always safe** (new fields
  are optional/additive, §5). **Older → unsupported** — roll this chart to at
  least `min-crds-version` first.

## Source of truth

The CRD templates are **generated artifacts**, wrapped byte-for-byte from
`config/crd/bases/*.yaml` (controller-gen output) by `make helm-sync-crds` via
`hack/wrap-crd-template.sh`. Never edit them by hand. After regenerating
manifests:

```sh
make helm-sync-crds   # config/crd/bases/*.yaml -> config/helm-crds/templates/
make verify-codegen   # fails on drift
```

## Verification

```sh
helm lint config/helm-crds
helm template k8squad-crds config/helm-crds        # renders 11 CRDs
helm template k8squad-crds config/helm-crds --set keep=false | grep -c resource-policy   # 0
```

Upgrade propagation, CR survival, and install ordering are exercised on a kind
cluster by [`.github/workflows/helm-crd-upgrade.yml`](../../.github/workflows/helm-crd-upgrade.yml)
(logic in [`hack/test-crd-upgrade.sh`](../../hack/test-crd-upgrade.sh)).
