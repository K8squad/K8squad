# CRD upgrade & migration guide

How K8squad's `ksquad.io` CustomResourceDefinitions (CRDs) are installed,
upgraded, and — when a release makes a breaking schema change — migrated
without losing your custom resources (CRs).

> **Contract:** the CRD lifecycle behaviour described here is set by
> [ADR-0002 — CRD lifecycle management via Helm](adr/ADR-0002-crd-lifecycle-via-helm.md).
> The Helm value keys and commands must match the CRD chart README
> (`config/helm-crds/README.md`) exactly.

## 1. How CRDs are delivered

The eleven `ksquad.io` CRDs (`Team`, `Agent`, `AgentRuntime`, `Role`, `Skill`,
`Project`, `Run`, `EgressPolicy`, `OtelConfig`, `MCPServer`, `Toolchain`) ship
in their **own standalone Helm chart, `k8squad-crds`** (`config/helm-crds`) —
**separate** from the `k8squad` control-plane chart. This is the key property:
the CRDs can be installed and upgraded **independently** of the control plane,
so you roll the schema forward on its own cadence.

Because the CRDs are ordinary chart resources of the `k8squad-crds` release,
**`helm upgrade k8squad-crds` propagates CRD schema changes** (new fields, new
versions, CEL rules) to existing installs via Helm's three-way merge — with no
`kubectl apply` step. The `k8squad` control-plane chart ships **zero** CRDs; it
owns none.

The `k8squad-crds` chart has exactly **one** value (default `true`):

| Value | Default | Effect |
| --- | --- | --- |
| `keep` | `true` | Annotate each CRD with `helm.sh/resource-policy: keep` so `helm uninstall k8squad-crds` **never** deletes the CRDs (and never cascade-deletes your CRs). Set `false` only on throwaway/test clusters. |

**Installing the `k8squad-crds` chart *is* the opt-in; not installing it is the
opt-out.** There is no `crds.install` toggle — the chart boundary is the switch.
If you manage CRDs out-of-band (GitOps/Flux/Argo, or a cluster-admin apply),
simply don't install `k8squad-crds`; the control-plane chart owns no CRDs and
will not fight your tooling.

> **Migrating from a pre-Option-B release?** Older K8squad chart revisions
> shipped the CRDs inside the single `k8squad` chart (Helm's install-only
> `crds/` directory), where Helm did **not** own them. The new `k8squad-crds`
> chart takes over that ownership. Because the generated CRD YAML is unchanged
> and the CRDs are `resource-policy: keep`, no CR data is at risk during the
> switch: install `k8squad-crds` alongside your existing release, then on the
> next `k8squad` upgrade the control-plane chart no longer carries CRDs. The
> exact adoption one-liner (labeling existing CRDs for the `k8squad-crds`
> release) ships with the chart — see its README.

### `--skip-crds` no longer applies (breaking UX change)

Helm's `--skip-crds` flag only ever affected the special `crds/` directory of a
single chart. Under the two-chart layout it has **no meaning** for K8squad CRDs:
the control-plane chart contains none, and the CRD chart *is* the CRDs. Anyone
scripting `--skip-crds` (or the interim Option-A `--set crds.install=false`)
should drop it — the opt-out is now simply "don't install `k8squad-crds`", which
is declarative and GitOps-friendly.

## 2. Install, then upgrade (no data loss)

**CRDs first, control plane second — always.** The API server must have the CRDs
*established* before the control-plane chart creates any CR (e.g. the default
`Toolchain` catalog).

```sh
# Install (fresh cluster) — CRDs first, then the control plane.
helm install k8squad-crds oci://charts.k8squad.io/k8squad-crds --wait
helm install k8squad      oci://charts.k8squad.io/k8squad      -n k8squad-system --create-namespace --wait
kubectl api-resources --api-group=ksquad.io

# Upgrade — roll CRD schema forward first, then the control plane that uses it.
helm upgrade k8squad-crds oci://charts.k8squad.io/k8squad-crds --wait
helm upgrade k8squad      oci://charts.k8squad.io/k8squad      -n k8squad-system --wait

# Verify the served/stored versions and that your CRs still list.
kubectl get crd runs.ksquad.io -o jsonpath='{.status.storedVersions}{"\n"}'
kubectl get runs.ksquad.io -A
```

The two charts stay independently installable and upgradeable; a convenience
wrapper (`Makefile`/script) may run them in order, but never couples them into
one release. `keep=true` (default) means a later `helm uninstall k8squad-crds`
leaves the CRDs and every CR in place — and `helm uninstall k8squad` (control
plane) never touches CRDs or CRs because it owns neither. Your data survives
install → upgrade → uninstall by design.

## 3. Version skew between the two charts

Because the charts release on independent cadences, keep them in a supported
skew window:

- The **control-plane chart declares the minimum CRD-chart version it needs** as
  `annotations."k8squad.io/min-crds-version"` in `config/helm/Chart.yaml`; the
  `k8squad` chart's `NOTES.txt` prints it plus a `kubectl get crd` self-check
  hint on install/upgrade.
- **Upgrade `k8squad-crds` to at least `min-crds-version` before (or with) the
  control-plane upgrade.** A CRD chart **newer** than the control plane requires
  is **always safe** — new fields are additive/optional and an older control
  plane simply doesn't populate them (this is *why* CRDs upgrade first). A CRD
  chart **older** than the control plane requires is **unsupported**: the
  control plane may write a field the CRD schema predates and have it rejected.
- Any CRD schema change bumps the `k8squad-crds` chart `version`; a control-plane
  change that depends on a new CRD field bumps `k8squad.io/min-crds-version` to
  match.

## 4. CRD versioning & deprecation policy

Summarised from [ADR-0002 §5](adr/ADR-0002-crd-lifecycle-via-helm.md#5-crd-versioning--deprecation-policy).
Current served/storage version for all kinds is **`v1alpha1`** (single
version).

While pre-1.0 (`v1alphaN`):

- **Additive-only within a version.** New optional fields, new enum values, or
  relaxed validation ship in `v1alpha1`; `helm upgrade k8squad-crds` propagates
  them and no version bump or migration is needed — bump only the CRD chart
  `version`. **This is the common case.**
- **Breaking changes** — remove/rename a field, tighten validation that would
  reject stored objects, or change a field's type/semantics — require a **new
  API version** (`v1alpha2` / `v1beta1`) served alongside the old one, with a
  conversion path before the old version is deprecated.
- **Deprecation:** the outgoing version is marked `deprecated: true` with a
  `deprecationWarning` and kept **served** until no client uses it.
- **Storage-version migration:** never drop a version that is still the
  `storage: true` version or that still has stored objects. Promote the new
  version to storage, migrate stored objects, *then* drop the old version in a
  later `k8squad-crds` release.
- **Invariant (non-negotiable):** no upgrade path may shrink schema in a way
  that orphans or rejects existing CRs.

A release that contains a **breaking** CRD change ships the notice in §5.

## 5. Per-release breaking-CRD-change notice (template)

Copy this into the release notes for any release with a breaking CRD change.
Omit sections that do not apply. Additive-only releases need no notice.

---

> **Applies to:** `k8squad-crds` chart `vX.Y.Z` → `vX.Y.Z+1`
> **CRD group:** `ksquad.io` · **Affected kinds:** `<Kind(s)>`

**1. Summary** — one line: what changed and whether action is required
*before* `helm upgrade k8squad-crds`.

**2. Breaking changes** (omit if none)

| CRD kind | Field | Change | Old → New | Action |
| --- | --- | --- | --- | --- |
| `Run` | `spec.<field>` | renamed / removed / retyped | `<old>` → `<new>` | migrate CRs before upgrade |

**3. Migration steps (no data loss)**

```sh
# 1. Back up existing CRs of every affected kind.
kubectl get <kind>.ksquad.io -A -o yaml > backup-<kind>-$(date +%F).yaml

# 2. If a field moved/renamed, transform stored CRs (script or manual re-apply)
#    BEFORE upgrading, so the new schema admits them.

# 3. Upgrade the CRD chart first, then the control-plane chart.
helm upgrade k8squad-crds oci://charts.k8squad.io/k8squad-crds --wait
helm upgrade k8squad      oci://charts.k8squad.io/k8squad -n k8squad-system --wait

# 4. Verify the served/stored versions and that CRs still reconcile.
kubectl get crd <kind>.ksquad.io -o jsonpath='{.status.storedVersions}{"\n"}'
kubectl get <kind>.ksquad.io -A
```

**4. Rollback** — `helm rollback k8squad-crds <previous-revision>` (and, if you
also upgraded it, `helm rollback k8squad <previous-revision>`), then restore CRs
from the step-1 backup if reconcile regressed. Because `keep=true`, the CRDs
themselves are never removed by a failed upgrade or rollback.

**5. Deprecations** — fields deprecated (not yet removed) and the release they
are scheduled for removal, per the versioning/deprecation policy in §4.

---

## See also

- CRD chart README (`config/helm-crds/README.md`) — the authoritative `keep`
  value, install-first ordering, and skew policy.
- [Chart README — control plane](../config/helm/README.md) — the `k8squad`
  chart; owns no CRDs, declares `k8squad.io/min-crds-version`.
- [ADR-0002 — CRD lifecycle management via Helm](adr/ADR-0002-crd-lifecycle-via-helm.md)
  — the decision, invariants, and versioning policy.
- [Getting started](getting-started-bmad.md) — first install of the charts.
