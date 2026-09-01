# CRD upgrade & migration guide

How K8squad's `ksquad.io` CustomResourceDefinitions (CRDs) are installed,
upgraded, and — when a release makes a breaking schema change — migrated
without losing your custom resources (CRs).

> **Contract:** the CRD lifecycle behaviour described here is set by
> [ADR-0002 — CRD lifecycle management via Helm](adr/ADR-0002-crd-lifecycle-via-helm.md).
> The Helm value keys and commands must match the chart README
> ([`config/helm/README.md`](../config/helm/README.md)) exactly.

## 1. How CRDs are delivered

The eleven `ksquad.io` CRDs (`Team`, `Agent`, `AgentRuntime`, `Role`, `Skill`,
`Project`, `Run`, `EgressPolicy`, `OtelConfig`, `MCPServer`, `Toolchain`) are
rendered as **ordinary chart templates**, not from Helm's special `crds/`
directory. Because they are part of release state, **`helm upgrade` propagates
CRD schema changes** (new fields, new versions, CEL rules) to existing installs
— the same three-way merge Helm applies to any other manifest.

Two values control the lifecycle (both default `true`):

| Value | Default | Effect |
| --- | --- | --- |
| `crds.install` | `true` | Render the `ksquad.io` CRDs with the release. Honored on **both install and upgrade**. Set `false` to manage CRDs out-of-band (GitOps/Flux/Argo, or a cluster-admin apply). |
| `crds.keep` | `true` | Annotate each CRD with `helm.sh/resource-policy: keep` so `helm uninstall` **never** deletes CRDs (and never cascade-deletes your CRs). Set `false` only on throwaway/test clusters. |

### `--skip-crds` no longer applies (breaking UX change)

Helm's `--skip-crds` flag **only** affects the special `crds/` directory. Now
that the CRDs are templates, `--skip-crds` has **no effect** on them. The
replacement is `--set crds.install=false`, which is strictly better: it is
honored on **both install and upgrade** and is declarable in a values file for
GitOps. Anyone scripting `--skip-crds` should migrate to `crds.install=false`.

## 2. Install, then upgrade (no data loss)

```sh
# Install (fresh cluster) — creates all 11 CRDs, then the release.
helm install k8squad config/helm -n k8squad-system --create-namespace --wait
kubectl api-resources --api-group=ksquad.io

# Upgrade — propagates any CRD schema changes to the existing install.
helm upgrade k8squad config/helm -n k8squad-system --wait

# Verify the served/stored versions and that your CRs still list.
kubectl get crd runs.ksquad.io -o jsonpath='{.status.storedVersions}{"\n"}'
kubectl get runs.ksquad.io -A
```

`crds.keep=true` (default) means a later `helm uninstall` leaves the CRDs and
every CR in place. Your data survives install → upgrade → uninstall by design.

## 3. CRD versioning & deprecation policy

Summarised from [ADR-0002 §5](adr/ADR-0002-crd-lifecycle-via-helm.md#5-crd-versioning--deprecation-policy).
Current served/storage version for all kinds is **`v1alpha1`** (single
version).

While pre-1.0 (`v1alphaN`):

- **Additive-only within a version.** New optional fields, new enum values, or
  relaxed validation ship in `v1alpha1`; `helm upgrade` propagates them and no
  version bump or migration is needed. **This is the common case.**
- **Breaking changes** — remove/rename a field, tighten validation that would
  reject stored objects, or change a field's type/semantics — require a **new
  API version** (`v1alpha2` / `v1beta1`) served alongside the old one, with a
  conversion path before the old version is deprecated.
- **Deprecation:** the outgoing version is marked `deprecated: true` with a
  `deprecationWarning` and kept **served** until no client uses it.
- **Storage-version migration:** never drop a version that is still the
  `storage: true` version or that still has stored objects. Promote the new
  version to storage, migrate stored objects, *then* drop the old version in a
  later release.
- **Invariant (non-negotiable):** no upgrade path may shrink schema in a way
  that orphans or rejects existing CRs.

A release that contains a **breaking** CRD change ships the notice in §4.

## 4. Per-release breaking-CRD-change notice (template)

Copy this into the release notes for any release with a breaking CRD change.
Omit sections that do not apply. Additive-only releases need no notice.

---

> **Applies to:** k8squad chart `vX.Y.Z` → `vX.Y.Z+1`
> **CRD group:** `ksquad.io` · **Affected kinds:** `<Kind(s)>`

**1. Summary** — one line: what changed and whether action is required
*before* `helm upgrade`.

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

# 3. Upgrade the chart.
helm upgrade k8squad config/helm -n k8squad-system --wait

# 4. Verify the served/stored versions and that CRs still reconcile.
kubectl get crd <kind>.ksquad.io -o jsonpath='{.status.storedVersions}{"\n"}'
kubectl get <kind>.ksquad.io -A
```

**4. Rollback** — `helm rollback k8squad <previous-revision>`, then restore CRs
from the step-1 backup if reconcile regressed. Because `crds.keep=true`, the
CRDs themselves are never removed by a failed upgrade or rollback.

**5. Deprecations** — fields deprecated (not yet removed) and the release they
are scheduled for removal, per the versioning/deprecation policy in §3.

---

## See also

- [Chart README — CRD lifecycle (upgrade-safe)](../config/helm/README.md#crd-lifecycle-upgrade-safe)
  — the authoritative value keys and commands.
- [ADR-0002 — CRD lifecycle management via Helm](adr/ADR-0002-crd-lifecycle-via-helm.md)
  — the decision, invariants, and versioning policy.
- [Getting started](getting-started-bmad.md) — first install of the chart.
