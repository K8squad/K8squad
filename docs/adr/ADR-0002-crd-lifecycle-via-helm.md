# ADR-0002 — CRD lifecycle management via Helm (upgrade-safe CRDs)

- **Status:** Accepted
- **Date:** 2026-09-01
- **Author:** Winston (System Architect)
- **Issue:** ISI-3517 (parent ISI-3516)
- **Implements via:** ISI-3518 (DevOps) · ISI-3519 (Docs/website)
- **Applies to chart:** `config/helm` (canonical). See §8 for `deploy/helm/ksquad`.

## Context

K8squad ships 11 `ksquad.io` CRDs (`agents, agentruntimes, egresspolicies,
mcpservers, otelconfigs, projects, roles, runs, skills, teams, toolchains`).
They currently live in `config/helm/crds/` — Helm's **special `crds/`
directory**.

Helm treats that directory specially and by documented design:

- Installs the CRDs **once**, on `helm install`, before templates render.
- **Never upgrades, patches, or reconciles them on `helm upgrade`.**
- Ignores `--set` values, hooks, and templating for those files.

We have had steady CRD churn — Skills, OtelConfig, EgressPolicy, revision
annotations, CEL validation rules. Every existing install that came up on an
older chart is now running a **stale CRD schema**: new fields are silently
dropped by the API server and CRs that use them are rejected. There is no
supported Helm path to fix this in place; operators must `kubectl apply` CRDs
by hand, which defeats the single-`helm upgrade` UX we ship in getting-started.

Generation/sync today: `make manifests` emits `config/crd/bases/*.yaml`;
`make helm-sync-crds` `cp`s them into `config/helm/crds/`; `make verify-codegen`
fails CI on drift between `api/`, `config/crd/bases/`, and `config/helm/crds/`.

## Decision

**Adopt Option A — templated CRDs in-chart, gated and keep-annotated.**

Move the 11 CRDs from `config/helm/crds/` into `config/helm/templates/crds/`.
As ordinary templates they become part of release state, so **`helm upgrade`
reconciles their schema** via Helm's three-way merge. Guard and annotate:

- `{{- if .Values.crds.install }}` … `{{- end }}` around each CRD
  (`crds.install` default **true**).
- `helm.sh/resource-policy: keep`, emitted only when `.Values.crds.keep` is
  true (default **true**), so **`helm uninstall` never deletes CRDs or the
  user's CRs**.

This matches cert-manager v1.15+ and most modern operators, and preserves our
single-chart `helm install` / `helm upgrade` getting-started UX.

### Why not Option B (separate `k8squad-crds` chart)

Option B (a dedicated CRD chart applied before the app chart, Flux /
prometheus-operator style) gives cleaner GitOps and multi-tenant separation,
but costs us a **two-chart install/upgrade UX** and release-coordination
burden that contradicts the one-command getting-started story we ship today.
It stays a documented future option **if and when** a GitOps/multi-tenant
distribution becomes a near-term goal — the migration from A→B is additive
(publish the CRD chart, point `crds.install=false` at the app chart). We are
not paying that complexity now for a need we do not yet have. **Boring,
single-chart, upgrade-safe wins.**

### Why not a subchart

A `config/helm/charts/k8squad-crds` subchart was considered as a middle
ground. It adds a chart boundary but **no ordering guarantee** over the
parent's CRs (subchart resources are merged into the same release and sorted
by Helm's kind order alongside parent resources — see §3), so it buys
indirection without solving the one real trade-off. Rejected for simplicity.

## 1. Chart layout changes (`config/helm`)

```
config/helm/
  crds/                      # REMOVED (Helm special dir, install-only)
  templates/
    crds/                    # NEW — 11 CRDs as guarded templates
      ksquad.io_agents.yaml
      ... (11 files)
    toolchain-default-catalog.yaml   # existing CR-creating template (see §3)
  values.yaml                # + crds.install / crds.keep
  values.schema.json         # + crds object schema
```

The generated CRD YAML is **unchanged**; only its location and a small
Helm header/footer wrapper differ. `make helm-sync-crds` retargets to
`templates/crds/` and injects the guard+annotation wrapper on sync so the
files stay round-trippable and `verify-codegen` still fails on drift.

## 2. Values keys

```yaml
crds:
  # Install/upgrade the ksquad.io CRDs as part of this release.
  # Set false when CRDs are managed out-of-band (GitOps, a separate CRD
  # chart, or cluster-admin-applied). Applies to BOTH install and upgrade.
  install: true
  # Annotate CRDs with helm.sh/resource-policy: keep so `helm uninstall`
  # never deletes them or the user's custom resources. Leave true in
  # production; set false only for throwaway/test clusters.
  keep: true
```

`values.schema.json` gains a `crds` object with two booleans (both default
true, `additionalProperties: false`).

## 3. Install / upgrade ordering

Helm's kind sorter installs `CustomResourceDefinition` (a known kind, early in
`installOrder`) **before** custom resources — `Toolchain`, `Skill`, etc. are
unknown kinds and sort to the **end**. So within our single release the
templated CRDs are applied before `templates/toolchain-default-catalog.yaml`
creates its `Toolchain` CR. Ordering on first install is therefore correct by
construction; no hook is required for the create order itself.

The one residual risk is **API-registration latency**: the API server must
finish establishing the CRD before it will admit the CR. On a busy first
install this can flake. Mitigations, in order of preference:

1. **Recommended:** getting-started already runs `helm install --wait`; keep
   that. For the CR catalog specifically, DevOps should confirm the catalog CR
   applies cleanly in the CI propagation test (§7 AC-6). If a flake is
   observed, escalate to (2).
2. Gate `toolchain-default-catalog.yaml` behind a `post-install,post-upgrade`
   Helm hook (hook-weight after CRD establishment). Only adopt if a real flake
   appears — do not add the hook speculatively.

On **upgrade**, CRDs and CRs already exist; Helm's three-way merge updates the
CRD schema in place. Adding a served field is additive and safe. Removing or
narrowing a field is governed by §5.

## 4. `--skip-crds` behavior (breaking UX change — document it)

`--skip-crds` is a Helm flag that **only** affects the special `crds/`
directory. Once CRDs are templates, `--skip-crds` **no longer has any effect
on them.** The replacement knob is `--set crds.install=false`, which is
strictly better: it is honored on **both install and upgrade** (unlike
`--skip-crds`, which is install-only) and is declarable in a values file for
GitOps.

Docs (ISI-3519) must call this out explicitly: anyone scripting `--skip-crds`
migrates to `crds.install=false`.

## 5. CRD versioning / deprecation policy

Current served/storage version for all kinds: **`v1alpha1`** (single version).

While pre-1.0 (`v1alphaN`):

- **Additive-only within a version.** New optional fields, new enum values,
  relaxed validation → ship in `v1alpha1`; `helm upgrade` propagates them.
  This is the common case and needs no version bump.
- **Breaking changes** (remove/rename a field, tighten validation that would
  reject stored objects, change a field's type/semantics) require a **new API
  version** (`v1alpha2` or `v1beta1`), served alongside the old one, with a
  **conversion path** (none/webhook) before the old version is deprecated.
- **Deprecation:** mark the outgoing version `deprecated: true` with a
  `deprecationWarning` in the CRD `versions[]` entry. Keep it **served** until
  no client uses it.
- **Storage version migration:** never drop a version that is still the
  `storage: true` version or that still has stored objects. Promote the new
  version to storage, migrate stored objects (re-write / storage-version
  migrator), *then* drop the old version in a later release.
- **Never** shrink schema on `helm upgrade` in a way that would orphan or
  reject existing CRs. The invariant below is absolute.

Graduation to `v1` follows the same additive/conversion discipline.

## 6. Invariants (non-negotiable)

1. `helm upgrade` **must** propagate CRD schema changes to existing installs.
2. **No path** — upgrade, uninstall, rollback, `crds.install=false` — may
   **delete a user's custom resources**. `resource-policy: keep` + never
   removing a stored/served version enforce this.
3. Generated CRD YAML remains the single source of truth
   (`config/crd/bases/`); `verify-codegen` keeps the chart copy in lockstep.

## 7. Acceptance criteria for DevOps implementation (ISI-3518)

- **AC-1** 11 CRDs relocated `config/helm/crds/` → `config/helm/templates/crds/`;
  `config/helm/crds/` removed. Each file wrapped with
  `{{- if .Values.crds.install }}` / `{{- end }}` and, gated by
  `{{- if .Values.crds.keep }}`, the literal annotation
  `helm.sh/resource-policy: keep`.
- **AC-2** `values.yaml` gains `crds: { install: true, keep: true }`;
  `values.schema.json` gains the matching object (booleans, defaults true,
  `additionalProperties: false`).
- **AC-3** `make helm-sync-crds` retargets to `templates/crds/` and injects the
  guard/annotation wrapper on sync; `make verify-codegen` updated to check the
  new path and passes clean (no drift) after `make manifests`.
- **AC-4 (the core proof)** CI test: `helm install` an **older** chart revision
  whose `runs` CRD lacks a recently-added field, then `helm upgrade` to HEAD,
  then assert `kubectl get crd runs.ksquad.io -o jsonpath=...` shows the new
  field present. Run on kind. This is the regression that proves the whole ADR.
- **AC-5** `helm uninstall` with `crds.keep=true` (default) leaves all 11 CRDs
  **and** any existing CRs intact; with `crds.keep=false` they are removed.
  Both cases asserted in CI.
- **AC-6** `helm install --wait` succeeds with the `toolchain-default-catalog`
  `Toolchain` CR admitted (CRD registered before CR). If flaky, apply §3
  mitigation (2) and note it.
- **AC-7** `--set crds.install=false` produces a release with **zero** CRD
  objects on both install and upgrade (asserted via `helm template` +
  kind apply).
- **AC-8** No behavioral regression to existing service templates; `helm lint`
  and `helm template` clean.

## 8. `deploy/helm/ksquad` disposition

`deploy/helm/ksquad` is the **services** chart (apiserver, operator, console,
NATS, postgres, gateway) and ships **no CRDs**. `config/helm` remains the
**canonical** chart and the **sole owner of CRD delivery**. Therefore:

- `deploy/helm/ksquad` needs **no** CRD-lifecycle change under this ADR.
- Its CRD dependency should be documented as "CRDs installed by the canonical
  `config/helm` chart" (or by a future `k8squad-crds` chart under Option B).
- Whether `deploy/helm/ksquad` is formally **deprecated** or converged into
  `config/helm` is **out of scope here** and should be tracked as a separate
  chart-consolidation decision. This ADR does not bless two divergent charts;
  it only fixes CRD upgrades in the canonical one.

## Consequences

- **Positive:** `helm upgrade` fixes stale schemas fleet-wide; one command;
  matches modern operator norms; `crds.install`/`crds.keep` give clean
  GitOps/uninstall control; migration path to Option B stays open and additive.
- **Negative / accepted:** CRDs join release state (larger release secret,
  three-way-merge semantics on CRDs); `--skip-crds` muscle memory breaks (§4,
  documented); a possible first-install API-registration race mitigated by
  `--wait` (§3).
